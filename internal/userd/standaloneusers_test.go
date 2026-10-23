package userd_test

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/xconnio/deskconn/internal/common"
	"github.com/xconnio/deskconn/internal/userd"
	wampproto "github.com/xconnio/wampproto-go"
	"github.com/xconnio/wampproto-go/auth"
	"github.com/xconnio/wampproto-go/serializers"
	"github.com/xconnio/xconn-go"
	xconnauth "github.com/xconnio/xconn-go/auth"
	xconnd "github.com/xconnio/xconn-go/xconn"
)

const (
	standaloneAlice    = "alice"
	standaloneBob      = "bob"
	standaloneCarol    = "carol"
	standaloneDave     = "dave"
	standaloneEve      = "eve"
	usernameKwarg      = "username"
	standalonePassword = "s3cret"
)

// standaloneServer is a standalone machine serving userd's procedures, with users as its
// authenticator, as desk serve --mode standalone has it.
type standaloneServer struct {
	users    *userd.StandaloneUsers
	url      string
	certHash string
}

func startStandaloneServer(t *testing.T, dbPath string, fixed ...userd.StandaloneUser) *standaloneServer {
	t.Helper()
	users, err := userd.OpenStandaloneUsers(dbPath, fixed)
	require.NoError(t, err)
	t.Cleanup(func() { _ = users.Close() })

	router, err := xconnd.NewDeviceRouter(common.StandaloneRealm)
	require.NoError(t, err)
	t.Cleanup(router.Close)
	local, err := xconn.ConnectInMemory(router, common.StandaloneRealm)
	require.NoError(t, err)
	d := userd.NewDeskconn(false, t.TempDir())
	d.ServeStandaloneUsers(users)
	require.NoError(t, d.Register(local))

	dir := t.TempDir()
	cert, err := common.LoadOrCreateCertificate(filepath.Join(dir, "cert.pem"), filepath.Join(dir, "key.pem"))
	require.NoError(t, err)
	server := xconn.NewServer(router, users, &xconn.ServerConfig{})
	listener, err := server.ListenAndServeQUIC("127.0.0.1:0",
		&tls.Config{Certificates: []tls.Certificate{cert}, MinVersion: tls.VersionTLS13})
	require.NoError(t, err)
	t.Cleanup(func() { _ = listener.Close() })
	go func() {
		for range listener.AcceptSession() { //nolint:revive // only draining
		}
	}()
	return &standaloneServer{users: users, url: "quic://" + listener.Addr().String(),
		certHash: common.CertificateFingerprint(cert)}
}

func (s *standaloneServer) connect(authenticator auth.ClientAuthenticator) (*common.MachineConn, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	return common.ConnectStandalone(ctx, s.url, s.certHash, common.StandaloneRealm, authenticator)
}

func password(username, password string) auth.ClientAuthenticator {
	return xconnauth.NewWAMPCRAAuthenticator(username, password, map[string]any{})
}

func keyPair(t *testing.T) (public, private string) {
	t.Helper()
	public, private, err := auth.GenerateCryptoSignKeyPair()
	require.NoError(t, err)
	return public, private
}

func key(t *testing.T, username, private string) auth.ClientAuthenticator {
	t.Helper()
	authenticator, err := xconnauth.NewCryptoSignAuthenticator(username, private, map[string]any{})
	require.NoError(t, err)
	return authenticator
}

// requireLogsIn checks authenticator is let in, or not.
func (s *standaloneServer) requireLogsIn(t *testing.T, authenticator auth.ClientAuthenticator, in bool) {
	t.Helper()
	conn, err := s.connect(authenticator)
	if !in {
		require.Error(t, err, "%s isn't let in", authenticator.AuthID())
		return
	}
	require.NoError(t, err, "%s is let in", authenticator.AuthID())
	_ = conn.Close()
}

// requireLogsInAs checks authenticator is let in as the user and role.
func (s *standaloneServer) requireLogsInAs(t *testing.T, authenticator auth.ClientAuthenticator, user, role string,
	why ...any) {
	t.Helper()
	conn, err := s.connect(authenticator)
	require.NoError(t, err, "%s is let in", authenticator.AuthID())
	defer conn.Close()
	require.Equal(t, user, conn.Details().AuthID(), why...)
	require.Equal(t, role, conn.Details().AuthRole(), why...)
}

// admin is a caller of the users' procedures.
type admin struct {
	conn *common.MachineConn
}

func (s *standaloneServer) admin(t *testing.T, authenticator auth.ClientAuthenticator) *admin {
	t.Helper()
	conn, err := s.connect(authenticator)
	require.NoError(t, err)
	t.Cleanup(func() { _ = conn.Close() })
	return &admin{conn: conn}
}

// call calls procedure with args: a user's fields as kwargs, a username as the argument, or
// kwargs as they are.
func (a *admin) call(procedure string, args any) (xconn.CallResponse, error) {
	call := a.conn.Call(procedure)
	switch args := args.(type) {
	case userd.StandaloneUser:
		data, err := json.Marshal(args)
		if err != nil {
			return xconn.CallResponse{}, err
		}
		var kwargs map[string]any
		if err := json.Unmarshal(data, &kwargs); err != nil {
			return xconn.CallResponse{}, err
		}
		call = call.Kwargs(kwargs)
	case map[string]any:
		call = call.Kwargs(args)
	case string:
		call = call.Args(args)
	}
	resp := call.Do()
	return resp, resp.Err
}

func (a *admin) list(t *testing.T) []userd.StandaloneUserInfo {
	t.Helper()
	resp, err := a.call(common.ProcedureStandaloneUsersList, nil)
	require.NoError(t, err)
	require.Len(t, resp.Args(), 1)
	data, err := json.Marshal(resp.Args()[0])
	require.NoError(t, err)
	var users []userd.StandaloneUserInfo
	require.NoError(t, json.Unmarshal(data, &users))
	return users
}

func TestStandaloneUsersFromTheConfig(t *testing.T) {
	bobPublic, bobPrivate := keyPair(t)
	_, strangerPrivate := keyPair(t)
	s := startStandaloneServer(t, filepath.Join(t.TempDir(), "users.db"),
		userd.StandaloneUser{Username: standaloneAlice, Password: standalonePassword, Role: common.RoleAdmin},
		userd.StandaloneUser{Username: standaloneBob, AuthorizedKeys: []string{bobPublic}})

	s.requireLogsInAs(t, password(standaloneAlice, standalonePassword), standaloneAlice, common.RoleAdmin)
	s.requireLogsInAs(t, key(t, standaloneBob, bobPrivate), standaloneBob, common.RoleMember)
	s.requireLogsInAs(t, key(t, standaloneAlice, bobPrivate), standaloneBob, common.RoleMember,
		"a key logs in as its user, not whoever its holder says")
	s.requireLogsIn(t, password(standaloneAlice, "wrong"), false)
	s.requireLogsIn(t, password(standaloneBob, standalonePassword), false)
	s.requireLogsIn(t, password(standaloneCarol, standalonePassword), false)
	s.requireLogsIn(t, key(t, standaloneBob, strangerPrivate), false)

	count, err := s.users.Count()
	require.NoError(t, err)
	require.Equal(t, 2, count)
}

func TestStandaloneUsersAPI(t *testing.T) {
	bobPublic, _ := keyPair(t)
	davePublic, davePrivate := keyPair(t)
	s := startStandaloneServer(t, filepath.Join(t.TempDir(), "users.db"),
		userd.StandaloneUser{Username: standaloneAlice, Password: standalonePassword, Role: common.RoleAdmin},
		userd.StandaloneUser{Username: standaloneBob, AuthorizedKeys: []string{bobPublic}})
	a := s.admin(t, password(standaloneAlice, standalonePassword))

	require.Equal(t, []userd.StandaloneUserInfo{
		{Username: standaloneAlice, AuthorizedKeys: []string{}, Password: true, Role: common.RoleAdmin, Config: true},
		{Username: standaloneBob, AuthorizedKeys: []string{bobPublic}, Role: common.RoleMember, Config: true},
	}, a.list(t), "always a list of keys, even none")

	// Added, a user may log in from then on: with a password, salted in the database...
	s.requireLogsIn(t, password(standaloneCarol, "carols"), false)
	_, err := a.call(common.ProcedureStandaloneUsersAdd,
		userd.StandaloneUser{Username: standaloneCarol, Password: "carols"})
	require.NoError(t, err)
	s.requireLogsInAs(t, password(standaloneCarol, "carols"), standaloneCarol, common.RoleMember)
	s.requireLogsIn(t, password(standaloneCarol, "wrong"), false)

	// ...or with a key, as its user.
	_, err = a.call(common.ProcedureStandaloneUsersAdd,
		userd.StandaloneUser{Username: standaloneDave, AuthorizedKeys: []string{davePublic}})
	require.NoError(t, err)
	s.requireLogsInAs(t, key(t, standaloneDave, davePrivate), standaloneDave, common.RoleMember)
	s.requireLogsInAs(t, key(t, standaloneAlice, davePrivate), standaloneDave, common.RoleMember)
	// A single key needn't be in a list.
	evePublic, evePrivate := keyPair(t)
	_, err = a.call(common.ProcedureStandaloneUsersAdd,
		map[string]any{usernameKwarg: standaloneEve, "authorized_keys": evePublic})
	require.NoError(t, err)
	s.requireLogsInAs(t, key(t, standaloneEve, evePrivate), standaloneEve, common.RoleMember)
	_, err = a.call(common.ProcedureStandaloneUsersRemove, standaloneEve)
	require.NoError(t, err)

	require.Equal(t, []userd.StandaloneUserInfo{
		{Username: standaloneAlice, AuthorizedKeys: []string{}, Password: true, Role: common.RoleAdmin, Config: true},
		{Username: standaloneBob, AuthorizedKeys: []string{bobPublic}, Role: common.RoleMember, Config: true},
		{Username: standaloneCarol, AuthorizedKeys: []string{}, Password: true, Role: common.RoleMember},
		{Username: standaloneDave, AuthorizedKeys: []string{davePublic}, Role: common.RoleMember},
	}, a.list(t), "never the passwords")

	// Only admins manage users: not the members, the config's or those added.
	carol := s.admin(t, password(standaloneCarol, "carols"))
	for procedure, args := range map[string]any{
		common.ProcedureStandaloneUsersList:   nil,
		common.ProcedureStandaloneUsersAdd:    userd.StandaloneUser{Username: standaloneEve, Password: "x"},
		common.ProcedureStandaloneUsersRemove: standaloneDave,
	} {
		_, err := carol.call(procedure, args)
		require.ErrorContains(t, err, common.ErrNotAuthorized, procedure)
	}

	for name, user := range map[string]userd.StandaloneUser{
		"already a user":        {Username: standaloneCarol, Password: "x"},
		"already in the config": {Username: standaloneAlice, Password: "x"},
		"a key of the config's": {Username: standaloneEve, AuthorizedKeys: []string{bobPublic}},
		"a key already added":   {Username: standaloneEve, AuthorizedKeys: []string{davePublic}},
		"not a key":             {Username: standaloneEve, AuthorizedKeys: []string{"abc"}},
		"no way to log in":      {Username: standaloneEve},
		"no name":               {Password: "x"},
		"a ':' in the name":     {Username: "e:ve", Password: "x"},
		"an admin":              {Username: standaloneEve, Password: "x", Role: common.RoleAdmin},
		"a role there isn't":    {Username: standaloneEve, Password: "x", Role: common.RoleOwner},
	} {
		_, err := a.call(common.ProcedureStandaloneUsersAdd, user)
		require.ErrorContains(t, err, common.ErrInvalidArgument, name)
	}
	for name, kwargs := range map[string]map[string]any{
		"a field there isn't": {usernameKwarg: standaloneEve, "pasword": "x"},
		"a name not a string": {usernameKwarg: 1, "password": "x"},
		"keys not strings":    {usernameKwarg: standaloneEve, "authorized_keys": []any{1}},
	} {
		_, err := a.call(common.ProcedureStandaloneUsersAdd, kwargs)
		require.ErrorContains(t, err, common.ErrInvalidArgument, name)
	}
	_, err = a.call(common.ProcedureStandaloneUsersRemove, nil)
	require.ErrorContains(t, err, common.ErrInvalidArgument, "no username")

	// Removed, a user can't log in from then on.
	_, err = a.call(common.ProcedureStandaloneUsersRemove, standaloneCarol)
	require.NoError(t, err)
	s.requireLogsIn(t, password(standaloneCarol, "carols"), false)
	_, err = a.call(common.ProcedureStandaloneUsersRemove, standaloneDave)
	require.NoError(t, err)
	s.requireLogsIn(t, key(t, standaloneDave, davePrivate), false)

	_, err = a.call(common.ProcedureStandaloneUsersRemove, standaloneAlice)
	require.ErrorContains(t, err, "in the config file", "the config's users are the config's to change")
	_, err = a.call(common.ProcedureStandaloneUsersRemove, "nobody")
	require.ErrorContains(t, err, userd.ErrNoStandaloneUser.Error())
	require.Len(t, a.list(t), 2)
}

func TestStandaloneUsersOnlyForAdmins(t *testing.T) {
	// A realm where callers of every role may call, so that it's the procedures refusing.
	roles := []string{common.RoleAdmin, common.RoleOwner, common.RoleMember, common.CloudRoleOwner,
		common.CloudRoleAdmin}
	realm := &xconn.RealmConfig{AutoDiscloseCaller: true}
	for _, role := range roles { // and the built-in trusted
		realm.Roles = append(realm.Roles, xconn.RealmRole{Name: role, Permissions: []xconn.Permission{
			{URI: "io.xconn.", MatchPolicy: wampproto.MatchPrefix, AllowCall: true, AllowRegister: true},
		}})
	}
	router, err := xconn.NewRouter(&xconn.RouterConfig{})
	require.NoError(t, err)
	require.NoError(t, router.AddRealm("realm1", realm))
	users, err := userd.OpenStandaloneUsers(filepath.Join(t.TempDir(), "users.db"),
		[]userd.StandaloneUser{{Username: standaloneAlice, Password: standalonePassword, Role: common.RoleAdmin}})
	require.NoError(t, err)
	t.Cleanup(func() { _ = users.Close() })
	callee, err := xconn.ConnectInMemory(router, "realm1")
	require.NoError(t, err)
	d := userd.NewDeskconn(false, t.TempDir())
	d.ServeStandaloneUsers(users)
	require.NoError(t, d.Register(callee))

	list := func(role string) error {
		base, err := xconn.ConnectInMemoryBase(router, "realm1", "someone", role, &serializers.MsgPackSerializer{}, 16)
		require.NoError(t, err)
		caller := xconn.NewSession(base, base.Serializer())
		return caller.Call(common.ProcedureStandaloneUsersList).Do().Err
	}
	require.NoError(t, list(common.RoleAdmin))
	// No other role, owners' and the cloud's included, nor local sessions.
	for _, role := range append(roles[1:], "trusted") {
		require.ErrorContains(t, list(role), common.ErrNotAuthorized, role)
	}
}

func TestStandaloneUsersKept(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "users.db")
	users, err := userd.OpenStandaloneUsers(dbPath, nil)
	require.NoError(t, err)
	require.NoError(t, users.Add(userd.StandaloneUser{Username: standaloneCarol, Password: "carols"}))
	require.ErrorContains(t, users.Remove(standaloneCarol), "last user", "no one could connect without them")
	require.NoError(t, users.Close())

	data, err := os.ReadFile(dbPath)
	require.NoError(t, err)
	require.NotContains(t, string(data), "carols", "the password isn't kept, only what's derived from it")

	// Across restarts.
	s := startStandaloneServer(t, dbPath)
	s.requireLogsIn(t, password(standaloneCarol, "carols"), true)
}

func TestStandaloneUsersInBothPlaces(t *testing.T) {
	public, _ := keyPair(t)
	dbPath := filepath.Join(t.TempDir(), "users.db")
	users, err := userd.OpenStandaloneUsers(dbPath, nil)
	require.NoError(t, err)
	require.NoError(t, users.Add(userd.StandaloneUser{Username: standaloneCarol, AuthorizedKeys: []string{public}}))
	require.NoError(t, users.Close())

	_, err = userd.OpenStandaloneUsers(dbPath, []userd.StandaloneUser{{Username: standaloneCarol, Password: "x"}})
	require.ErrorContains(t, err, "carol is in the config file and was added since too")
	_, err = userd.OpenStandaloneUsers(dbPath,
		[]userd.StandaloneUser{{Username: standaloneDave, AuthorizedKeys: []string{public}}})
	require.ErrorContains(t, err, "is dave's in the config file and carol's added since")

	// And while one desk serve has it open, another can't.
	users, err = userd.OpenStandaloneUsers(dbPath, nil)
	require.NoError(t, err)
	t.Cleanup(func() { _ = users.Close() })
	_, err = userd.OpenStandaloneUsers(dbPath, nil)
	require.ErrorContains(t, err, "another desk serve is using it")
}
