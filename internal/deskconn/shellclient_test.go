package deskconn_test

import (
	"bytes"
	"context"
	"io"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/xconnio/deskconn/internal/common"
	"github.com/xconnio/deskconn/internal/deskconn"
	"github.com/xconnio/xconn/bindings/go"
)

func TestShellConcurrentFrames(t *testing.T) {
	client, server := net.Pipe()
	t.Cleanup(func() { _ = client.Close() })
	t.Cleanup(func() { _ = server.Close() })
	require.NoError(t, client.SetDeadline(time.Now().Add(5*time.Second)))
	require.NoError(t, server.SetDeadline(time.Now().Add(5*time.Second)))

	send := deskconn.NewShellFrameWriter(client)
	const writers = 3
	const frames = 20
	var group sync.WaitGroup
	errors := make(chan error, writers)
	for id := range writers {
		group.Add(1)
		go func() {
			defer group.Done()
			for range frames {
				if err := send(bytes.Repeat([]byte{byte(id)}, 32)); err != nil {
					errors <- err
					return
				}
			}
		}()
	}

	counts := make([]int, writers)
	for range writers * frames {
		frame, err := common.ReadFrame(server)
		require.NoError(t, err)
		require.Len(t, frame, 32)
		require.Less(t, int(frame[0]), writers)
		require.Equal(t, bytes.Repeat(frame[:1], 32), frame)
		counts[frame[0]]++
	}
	group.Wait()
	close(errors)
	for err := range errors {
		require.NoError(t, err)
	}
	for _, count := range counts {
		require.Equal(t, frames, count)
	}
}

func TestShellStdinEOF(t *testing.T) {
	const line = "abc\n"
	key := bytes.Repeat([]byte{7}, 32)
	tests := []struct {
		name       string
		input      string
		forwardEOF bool
		want       string
	}{
		{"complete line", line, true, line + "\x04"},
		{"partial line", "abc", true, "abc\x04\x04"},
		{"empty", "", true, "\x04"},
		{"terminal", line, false, line},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			client, server := net.Pipe()
			t.Cleanup(func() { _ = server.Close() })
			require.NoError(t, server.SetDeadline(time.Now().Add(5*time.Second)))
			go func() {
				deskconn.RunShellStdin(client, key, strings.NewReader(tt.input), tt.forwardEOF)
				_ = client.Close()
			}()

			var got []byte
			for {
				frame, err := common.ReadFrame(server)
				if err != nil {
					require.ErrorIs(t, err, io.EOF)
					break
				}
				kind, plaintext, err := common.DecryptEnvelope(frame, key)
				require.NoError(t, err)
				require.Equal(t, common.ShellMsgData, kind)
				got = append(got, plaintext...)
			}
			require.Equal(t, tt.want, string(got))
		})
	}
}

// A peer that accepts a stream but stops during setup must not trap the terminal.
func TestShellHandshakeStall(t *testing.T) {
	for _, stage := range []string{"key exchange", "shell acknowledgement"} {
		t.Run(stage, func(t *testing.T) {
			client, server := net.Pipe()
			defer server.Close()
			defer client.Close()
			ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
			defer cancel()
			if stage == "shell acknowledgement" {
				go func() {
					_, err := common.ReadFrame(server)
					if err != nil {
						return
					}
					publicKey, _, err := common.CreateX25519KeyPair()
					if err != nil {
						return
					}
					if common.WriteMsg(server, common.KeyExchangeMsg{PublicKey: publicKey}) == nil {
						_, _ = common.ReadFrame(server)
					}
				}()
			}
			done := make(chan error, 1)
			go func() {
				_, err := deskconn.FinishShellHandshake(ctx, client, common.ShellControlMsg{}, func() { _ = client.Close() })
				done <- err
			}()
			select {
			case err := <-done:
				require.Error(t, err)
			case <-time.After(2 * time.Second):
				t.Fatal("stalled handshake ignored its deadline")
			}
		})
	}
}

func TestShellSetupKeepsRPCStreamAlive(t *testing.T) {
	router, err := xconn.NewRouter(nil)
	require.NoError(t, err)
	defer router.Close()
	require.NoError(t, router.AddRealm("shell-setup", nil))
	backend, err := xconn.ConnectInMemory(router, "shell-setup")
	require.NoError(t, err)
	defer backend.Close()
	caller, err := xconn.ConnectInMemory(router, "shell-setup")
	require.NoError(t, err)
	defer caller.Close()
	endpoint, peer := net.Pipe()
	defer endpoint.Close()
	defer peer.Close()
	require.NoError(t, backend.Register("test.shell", func(context.Context, *xconn.Invocation) *xconn.Result {
		return xconn.NewResult(nil, xconn.DuplexStream(endpoint))
	}).ExecuteContext(t.Context()).Err)
	ctx, finish, cancel := deskconn.ShellSetupContext(t.Context(), 100*time.Millisecond)
	defer cancel()
	stream, err := common.OpenRPCStream(ctx, caller, "test.shell", false)
	require.NoError(t, err)
	defer stream.Close()
	require.NoError(t, finish())
	// Keep using the actual RPC stream beyond the setup timeout.
	time.Sleep(150 * time.Millisecond)
	require.NoError(t, ctx.Err())
	require.NoError(t, stream.SetDeadline(time.Now().Add(time.Second)))
	written := make(chan error, 1)
	go func() { _, err := peer.Write([]byte("ok")); written <- err }()
	data := make([]byte, 2)
	_, err = io.ReadFull(stream, data)
	require.NoError(t, err)
	require.NoError(t, <-written)
	require.Equal(t, "ok", string(data))
	cancel()
	require.ErrorIs(t, ctx.Err(), context.Canceled)
}
