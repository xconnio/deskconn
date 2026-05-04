package deskconn_test

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/xconnio/deskconn"
	"github.com/xconnio/wampproto-go/auth"
	"github.com/xconnio/wampproto-go/messages"
)

func makeCryptoSignRequest(authid, publicKeyHex string) auth.Request {
	hello := messages.NewHello("realm1", authid, nil, nil, nil)
	return auth.NewCryptoSignRequest(hello, publicKeyHex)
}

func makeBaseRequest(method auth.Method, authid string) auth.Request {
	hello := messages.NewHello("realm1", authid, nil, nil, nil)
	return auth.NewRequest(hello, method)
}

func TestNewAuthenticator(t *testing.T) {
	a := deskconn.NewAuthenticator([]*deskconn.CryptosignPrincipal{
		{AuthID: "user1", AuthorizedKeys: []string{"key1"}, AuthRole: admin},
	})
	require.NotNil(t, a)

	p, ok := a.RetrievePrincipal("user1")
	require.True(t, ok)
	require.Equal(t, admin, p.AuthRole)

	_, ok = a.RetrievePrincipal("nobody")
	require.False(t, ok)
}

func TestAuthenticatorMethods(t *testing.T) {
	methods := deskconn.NewAuthenticator(nil).Methods()
	require.Len(t, methods, 1)
	require.Equal(t, auth.Method(auth.MethodCryptoSign), methods[0])
}

func TestAuthenticatorAuthenticate(t *testing.T) {
	pubKey, _, err := auth.GenerateCryptoSignKeyPair()
	require.NoError(t, err)
	otherKey, _, err := auth.GenerateCryptoSignKeyPair()
	require.NoError(t, err)

	a := deskconn.NewAuthenticator([]*deskconn.CryptosignPrincipal{
		{AuthID: "alice", AuthorizedKeys: []string{pubKey}, AuthRole: admin},
	})

	resp, err := a.Authenticate(makeCryptoSignRequest("alice", pubKey))
	require.NoError(t, err)
	require.Equal(t, "alice", resp.AuthID())
	require.Equal(t, admin, resp.AuthRole())

	_, err = a.Authenticate(makeCryptoSignRequest("nobody", pubKey))
	require.ErrorContains(t, err, "unknown authid")

	_, err = a.Authenticate(makeCryptoSignRequest("alice", otherKey))
	require.ErrorContains(t, err, "unknown publickey")

	_, err = a.Authenticate(makeBaseRequest(auth.MethodCryptoSign, "alice"))
	require.ErrorContains(t, err, "invalid request")

	_, err = a.Authenticate(makeBaseRequest(auth.Ticket, "alice"))
	require.ErrorContains(t, err, "unknown authentication method")
}

func TestAuthenticatorSetPrincipals(t *testing.T) {
	a := deskconn.NewAuthenticator([]*deskconn.CryptosignPrincipal{
		{AuthID: "old", AuthorizedKeys: []string{"k"}, AuthRole: user},
	})

	a.SetPrincipals([]*deskconn.CryptosignPrincipal{
		{AuthID: "new1", AuthorizedKeys: []string{"k1"}, AuthRole: admin},
		{AuthID: "new2", AuthorizedKeys: []string{"k2"}, AuthRole: user},
	})

	_, ok := a.RetrievePrincipal("old")
	require.False(t, ok)

	p, ok := a.RetrievePrincipal("new1")
	require.True(t, ok)
	require.Equal(t, admin, p.AuthRole)
}

func TestAuthenticatorSetPrincipal(t *testing.T) {
	a := deskconn.NewAuthenticator(nil)
	const key = "key-d"
	a.SetPrincipal("dave", &deskconn.CryptosignPrincipal{
		AuthID: "dave", AuthorizedKeys: []string{key}, AuthRole: user,
	})

	p, ok := a.RetrievePrincipal("dave")
	require.True(t, ok)
	require.Equal(t, []string{key}, p.AuthorizedKeys)

	a.SetPrincipal("dave", &deskconn.CryptosignPrincipal{
		AuthID: "dave", AuthorizedKeys: []string{key, "new-key"}, AuthRole: admin,
	})

	p, ok = a.RetrievePrincipal("dave")
	require.True(t, ok)
	require.Equal(t, admin, p.AuthRole)
	require.Len(t, p.AuthorizedKeys, 2)
}
