package connectivity_test

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"golang.org/x/sys/windows"

	app "github.com/xconnio/deskconn/internal/connectivity"
)

func TestWindowsMachineIDIsStable(t *testing.T) {
	first, err := app.MachineID()
	require.NoError(t, err)
	require.NotEmpty(t, first)
	second, err := app.MachineID()
	require.NoError(t, err)
	require.Equal(t, first, second)
}

func TestWindowsLocalEndpointHasOneOwner(t *testing.T) {
	const realm = "local"
	path := filepath.Join(t.TempDir(), "app.sock")
	router, listener, session, err := app.StartAppLayer(realm, path)
	require.NoError(t, err)
	defer router.Close()
	defer session.Close()
	defer listener.Close()
	descriptor, err := windows.GetNamedSecurityInfo(path, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION)
	require.NoError(t, err)
	acl, _, err := descriptor.DACL()
	require.NoError(t, err)
	require.EqualValues(t, 1, acl.AceCount)
	user, err := windows.GetCurrentProcessToken().GetTokenUser()
	require.NoError(t, err)
	expected, err := windows.SecurityDescriptorFromString("D:P(A;;FA;;;" + user.User.Sid.String() + ")")
	require.NoError(t, err)
	require.Contains(t, descriptor.String(), strings.TrimPrefix(expected.String(), "D:P"))

	_, duplicate, _, err := app.StartAppLayer(realm, path)
	if duplicate != nil {
		defer duplicate.Close()
	}
	require.Error(t, err)
	require.NoError(t, listener.Close())
	next, replacement, peer, err := app.StartAppLayer(realm, path)
	require.NoError(t, err)
	defer next.Close()
	defer replacement.Close()
	defer peer.Close()
	connected, err := app.ConnectApp(t.Context(), path, realm)
	require.NoError(t, err)
	require.NoError(t, connected.Close())
}
