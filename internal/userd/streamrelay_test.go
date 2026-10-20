package userd_test

import (
	"errors"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/xconnio/deskconn/internal/common"
	"github.com/xconnio/deskconn/internal/deskconn"
	"github.com/xconnio/deskconn/internal/userd"
	xconnd "github.com/xconnio/xconn-go/xconn"
)

// TestCloseStreamsEndsOpenShell confirms CloseStreams, as userd's shutdown calls it,
// closes a live shell's stream all the way through xconn's relay to the client, and
// refuses streams relayed after it.
func TestCloseStreamsEndsOpenShell(t *testing.T) {
	// Short prefix, not t.TempDir(): unix socket paths are length-limited.
	dir, err := os.MkdirTemp("", "streamrelay")
	require.NoError(t, err)
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	relaySock := filepath.Join(dir, "xconn-streams.sock")
	listener, err := net.Listen("unix", relaySock)
	require.NoError(t, err)
	t.Cleanup(func() { _ = listener.Close() })

	d := userd.NewShellDeskconn()
	go d.ServeStreamRelay(listener)

	openShell := func() net.Conn {
		client, remote := net.Pipe()
		t.Cleanup(func() { _ = client.Close() })
		go xconnd.RelayStream(remote, relaySock)
		return client
	}

	client := openShell()
	require.NoError(t, common.WriteMsg(client, common.RoutingFrame{Op: common.FSOpShell}))
	sendKey, receiveKey, err := deskconn.QuicClientKeyExchange(client)
	require.NoError(t, err)
	require.NoError(t, sendQUICShellControl(client, sendKey,
		common.ShellControlMsg{Op: common.ShellOpSize, Cols: 80, Rows: 24}))
	ack, err := recvQUICShellEnvelope(client, receiveKey)
	require.NoError(t, err)
	require.NotEmpty(t, ack.ackShellID)

	require.Equal(t, 1, d.CloseStreams())

	_ = client.SetReadDeadline(time.Now().Add(5 * time.Second))
	for {
		_, err := common.ReadFrame(client)
		if err != nil {
			var netErr net.Error
			require.False(t, errors.As(err, &netErr) && netErr.Timeout(), "shell stream still open after CloseStreams")
			break
		}
	}

	// Refused once shutting down: closed without being served, maybe before the
	// routing frame is even written.
	late := openShell()
	_ = common.WriteMsg(late, common.RoutingFrame{Op: common.FSOpShell})
	_ = late.SetReadDeadline(time.Now().Add(5 * time.Second))
	_, err = common.ReadFrame(late)
	var netErr net.Error
	require.False(t, errors.As(err, &netErr) && netErr.Timeout(), "stream relayed after CloseStreams wasn't closed")
}
