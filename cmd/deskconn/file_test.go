package main

import (
	"os"
	"testing"

	"github.com/stretchr/testify/require"
)

// Progress redraws its line, so it's only shown on a terminal, and never with --no-progress.
func TestTransferProgressOut(t *testing.T) {
	r, w, err := os.Pipe()
	require.NoError(t, err)
	t.Cleanup(func() { _ = r.Close(); _ = w.Close() })
	stderr := os.Stderr
	os.Stderr = w
	t.Cleanup(func() { os.Stderr = stderr })

	require.Nil(t, transferProgressOut(true))
	require.Nil(t, transferProgressOut(false))
}
