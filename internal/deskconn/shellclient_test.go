package deskconn_test

import (
	"bytes"
	"io"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/xconnio/deskconn/internal/common"
	"github.com/xconnio/deskconn/internal/deskconn"
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
