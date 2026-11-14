package deskconn_test

import (
	"bytes"
	"net"
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
