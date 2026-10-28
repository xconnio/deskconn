package deskconn

import (
	"context"
	"errors"
	"path/filepath"
	"time"

	"github.com/xconnio/deskconn/internal/common"
	"github.com/xconnio/xconn-go"
	xconnd "github.com/xconnio/xconn-go/xconn"
)

// Cloud is what the CLI and daemon leave to the cloud's account backend: how users sign in
// and out, which machines they have, and how a machine joins and leaves them. Joining the
// cloud and the machines with the login's CryptoSign key, keeping the credentials and caching
// the machines work the same with any backend. Every field is required.
type Cloud struct {
	// Login signs username in with password and makes publicKey one of their keys,
	// returning when it expires: zero for never.
	Login func(ctx context.Context, username, password, publicKey string) (time.Time, error)
	// Name returns the display name of the user signed in on session.
	Name func(session *xconn.Session) (string, error)
	// Logout revokes publicKey, on a session signed in with it.
	Logout func(session *xconn.Session, publicKey string) error
	// Machines lists the machines of the user signed in on session.
	Machines func(session *xconn.Session) ([]common.Machine, error)
	// Attach adds this machine, machineID, to username's machines as name, with publicKey
	// as its daemon's key, and returns the realm it is served in.
	Attach func(ctx context.Context, username, password, machineID, publicKey, name string) (string, error)
	// Detach removes machine machineID, on a session signed in as one of its users.
	Detach func(session *xconn.Session, machineID string) error

	// How the machine's daemon follows who may connect to it, on the cloud realm (see
	// xconn.CloudConfig): ProcedureListKeys lists the keys, and the topics, which take the
	// machine ID, are published when one is added or removed and when it is detached.
	ProcedureListKeys     string
	TopicKeyAddedFormat   string
	TopicKeyRemovedFormat string
	TopicDetachFormat     string
}

var cloud *Cloud //nolint:gochecknoglobals // set once at startup, see SetCloud

// SetCloud makes c the cloud this process signs in to and attaches machines to.
func SetCloud(c *Cloud) { cloud = c }

func currentCloud() (*Cloud, error) {
	if cloud == nil {
		return nil, errors.New("no cloud backend configured")
	}
	return cloud, nil
}

// DaemonCloudConfig is how xconn serves this machine through the cloud.
func DaemonCloudConfig(cfgDirectory string) (*xconnd.CloudConfig, error) {
	c, err := currentCloud()
	if err != nil {
		return nil, err
	}
	credentialsFile, err := common.CredentialsFilePath()
	if err != nil {
		return nil, err
	}
	return &xconnd.CloudConfig{
		Address:               common.CloudQUICAddress(),
		TLSConfig:             common.CloudQUICTLSConfig(),
		Realm:                 common.CloudRealm,
		ProcedureListKeys:     c.ProcedureListKeys,
		TopicKeyAddedFormat:   c.TopicKeyAddedFormat,
		TopicKeyRemovedFormat: c.TopicKeyRemovedFormat,
		TopicDetachFormat:     c.TopicDetachFormat,
		CredentialsFile:       credentialsFile,
		PrincipalsFile:        filepath.Join(cfgDirectory, "principals.json"),
		ICEServers:            common.ICEServers(),
	}, nil
}
