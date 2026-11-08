package deskconn_test

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/xconnio/deskconn/internal/common"
	"github.com/xconnio/deskconn/internal/deskconn"
	"github.com/xconnio/deskconn/relay"
	"github.com/xconnio/xconn-core/bindings/go"
	"github.com/xconnio/xconn-core/bindings/go/auth"
)

const (
	cloudUser     = "alice"
	cloudPassword = "secret"
	cloudUserRole = "user"
)

// fakeCloud is a backend of its own, no account service: it keeps the keys users log in
// with, and the router it runs next to admits whoever holds one.
type fakeCloud struct {
	sync.Mutex
	keys     map[string]string // public key -> authid
	machines []common.Machine
}

func (f *fakeCloud) authenticate(request auth.Request) (auth.Response, error) {
	cryptosign, ok := request.(*auth.RequestCryptoSign)
	if !ok {
		return nil, errors.New("only cryptosign")
	}
	f.Lock()
	defer f.Unlock()
	if f.keys[cryptosign.PublicKey()] != request.AuthID() {
		return nil, errors.New("unknown key")
	}
	return auth.NewResponse(request.AuthID(), cloudUserRole, 0)
}

func (f *fakeCloud) cloud() *deskconn.Cloud {
	checkPassword := func(username, password string) error {
		if username != cloudUser || password != cloudPassword {
			return errors.New("wrong password")
		}
		return nil
	}
	return &deskconn.Cloud{
		Login: func(_ context.Context, username, password, publicKey string) (time.Time, error) {
			if err := checkPassword(username, password); err != nil {
				return time.Time{}, err
			}
			f.Lock()
			defer f.Unlock()
			f.keys[publicKey] = username
			return time.Time{}, nil
		},
		Name: func(session *xconn.Session) (string, error) {
			return "Alice " + session.Details().AuthID(), nil
		},
		Logout: func(_ *xconn.Session, publicKey string) error {
			f.Lock()
			defer f.Unlock()
			delete(f.keys, publicKey)
			return nil
		},
		Machines: func(*xconn.Session) ([]common.Machine, error) {
			f.Lock()
			defer f.Unlock()
			return append([]common.Machine(nil), f.machines...), nil
		},
		Attach: func(_ context.Context, username, password, machineID, _, name string) (string, error) {
			if err := checkPassword(username, password); err != nil {
				return "", err
			}
			f.Lock()
			defer f.Unlock()
			f.machines = append(f.machines, common.Machine{Authid: machineID, Name: name, Realm: "realm-" + name})
			return "realm-" + name, nil
		},
		Detach: func(_ *xconn.Session, machineID string) error {
			f.Lock()
			defer f.Unlock()
			for i, machine := range f.machines {
				if machine.Authid == machineID {
					f.machines = append(f.machines[:i], f.machines[i+1:]...)
					return nil
				}
			}
			return errors.New("no such machine")
		},
	}
}

// startCloud serves a relay router authenticating with f over QUIC, and points this
// process's cloud at it, with a home of its own.
func startCloud(t *testing.T, f *fakeCloud) {
	t.Helper()
	conn, err := net.ListenPacket("udp", "127.0.0.1:0")
	require.NoError(t, err)
	address := conn.LocalAddr().String()
	require.NoError(t, conn.Close())

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	r, err := relay.New(ctx, relay.Config{
		Authenticate: f.authenticate,
		Roles:        []xconn.RealmRole{{Name: cloudUserRole}},
		QUICAddress:  address,
	})
	require.NoError(t, err)
	go func() { _ = r.Serve(ctx) }()

	t.Setenv("HOME", t.TempDir())
	t.Setenv("DESKCONN_CLOUD_QUIC_ADDRESS", address)
	deskconn.SetCloud(f.cloud())
	t.Cleanup(func() { deskconn.SetCloud(nil) })
}

func TestCloudWithoutAccountService(t *testing.T) {
	f := &fakeCloud{keys: map[string]string{}}
	startCloud(t, f)
	cfgDirectory, err := common.CfgDirectory()
	require.NoError(t, err)

	_, err = deskconn.Login(context.Background(), cloudUser, "wrong")
	require.ErrorContains(t, err, "wrong password")

	machines, err := deskconn.Login(context.Background(), cloudUser, cloudPassword)
	require.NoError(t, err)
	require.Empty(t, machines)
	authid, _, err := common.ReadCredentials(cfgDirectory)
	require.NoError(t, err)
	require.Equal(t, cloudUser, authid)
	pub, err := os.ReadFile(filepath.Join(cfgDirectory, "id_ed25519.pub"))
	require.NoError(t, err)
	require.Equal(t, "Alice "+cloudUser, strings.SplitN(strings.TrimSpace(string(pub)), " ", 3)[2])

	require.NoError(t, deskconn.Attach(cloudUser, cloudPassword, "office"))
	credentialsFile, err := common.CredentialsFilePath()
	require.NoError(t, err)
	data, err := os.ReadFile(credentialsFile)
	require.NoError(t, err)
	var credentials common.Credentials
	require.NoError(t, json.Unmarshal(data, &credentials))
	require.Equal(t, "realm-office", credentials.Realm)

	machines, err = deskconn.FetchMachinesFromCloud(cfgDirectory)
	require.NoError(t, err)
	require.Len(t, machines, 1)
	require.Equal(t, "office", machines[0].Name)

	session, err := deskconn.ConnectCloudRealm(cfgDirectory)
	require.NoError(t, err)
	require.NoError(t, deskconn.Detach(session, credentials.AuthID))
	_ = session.Leave()
	machines, err = deskconn.FetchMachinesFromCloud(cfgDirectory)
	require.NoError(t, err)
	require.Empty(t, machines)

	require.NoError(t, deskconn.Logout(cfgDirectory))
	_, _, err = common.ReadCredentials(cfgDirectory)
	require.Error(t, err)
	f.Lock()
	require.Empty(t, f.keys, "logging out revokes the key")
	f.Unlock()
}

func TestCloudDaemonConfig(t *testing.T) {
	deskconn.SetCloud(&deskconn.Cloud{
		ProcedureListKeys:     "my.keys.list",
		TopicKeyAddedFormat:   "my.%s.key.add",
		TopicKeyRemovedFormat: "my.%s.key.remove",
		TopicDetachFormat:     "my.%s.detach",
	})
	t.Cleanup(func() { deskconn.SetCloud(nil) })
	t.Setenv("HOME", t.TempDir())

	config, err := deskconn.DaemonCloudConfig(t.TempDir())
	require.NoError(t, err)
	require.Equal(t, "my.keys.list", config.ProcedureListKeys)
	require.Equal(t, "my.%s.key.add", config.TopicKeyAddedFormat)
	require.Equal(t, "my.%s.key.remove", config.TopicKeyRemovedFormat)
	require.Equal(t, "my.%s.detach", config.TopicDetachFormat)
}

func TestCloudNotConfigured(t *testing.T) {
	_, err := deskconn.FetchMachinesFromCloud(t.TempDir())
	require.ErrorContains(t, err, "no cloud backend configured")
}
