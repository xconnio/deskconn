package userd_test

import (
	"errors"
	"net"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/xconnio/deskconn/internal/common"
	"github.com/xconnio/deskconn/internal/deskconn"
)

func TestCloseStreamsEndsOpenShell(t *testing.T) {
	caller, backend := newRPCFeatureSession(t)
	stream, err := common.OpenRPCStream(t.Context(), caller, common.ProcedureShellStream, false)
	require.NoError(t, err)
	defer stream.Close()
	sendKey, receiveKey, err := deskconn.StreamClientKeyExchange(stream)
	require.NoError(t, err)
	require.NoError(t, sendQUICShellControl(stream, sendKey,
		common.ShellControlMsg{Op: common.ShellOpSize, Cols: 80, Rows: 24}))
	ack, err := recvQUICShellEnvelope(stream, receiveKey)
	require.NoError(t, err)
	require.NotEmpty(t, ack.ackShellID)
	require.Equal(t, 1, backend.CloseStreams())
	require.NoError(t, stream.SetReadDeadline(time.Now().Add(3*time.Second)))
	for {
		_, err = common.ReadFrame(stream)
		if err != nil {
			var timeout net.Error
			require.False(t, errors.As(err, &timeout) && timeout.Timeout(), "shutdown did not close the shell")
			break
		}
	}
	_, err = common.OpenRPCStream(t.Context(), caller, common.ProcedureShellStream, false)
	require.ErrorContains(t, err, "service stopping")
}
