//go:build !windows

package connectivity_test

import (
	"os"
	"path/filepath"
	"syscall"
	"testing"

	"github.com/stretchr/testify/require"

	app "github.com/xconnio/deskconn/internal/connectivity"
)

// TestAppLayerSocketIsPrivate checks the daemon's socket is the owner's alone even when the
// daemon starts with a umask that would leave it open to everyone.
func TestAppLayerSocketIsPrivate(t *testing.T) {
	previous := syscall.Umask(0)
	t.Cleanup(func() { syscall.Umask(previous) })

	path := filepath.Join(t.TempDir(), "deskconn.sock")
	router, listener, session, err := app.StartAppLayer("local", path)
	require.NoError(t, err)
	t.Cleanup(router.Close)
	t.Cleanup(func() { _ = listener.Close() })
	t.Cleanup(func() { _ = session.Close() })

	info, err := os.Stat(path)
	require.NoError(t, err)
	require.Equal(t, os.ModeSocket, info.Mode().Type())
	require.Equal(t, os.FileMode(0o600), info.Mode().Perm())

	connected, err := app.ConnectApp(t.Context(), path, "local")
	require.NoError(t, err, "the owner still connects")
	_ = connected.Close()
}
