package ai_test

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/xconnio/deskconn/ai"
)

func TestExtractTarballRejectsPathTraversal(t *testing.T) {
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	content := []byte("evil")
	require.NoError(t, tw.WriteHeader(&tar.Header{
		Name: "../../etc/passwd",
		Mode: 0600,
		Size: int64(len(content)),
	}))
	_, err := tw.Write(content)
	require.NoError(t, err)
	require.NoError(t, tw.Close())
	require.NoError(t, gz.Close())

	homeDir := t.TempDir()
	_, err = ai.ExtractTarball(buf.Bytes(), homeDir, testProjectPath)
	require.Error(t, err)

	_, statErr := os.Stat(filepath.Join(filepath.Dir(homeDir), "etc", "passwd"))
	require.True(t, os.IsNotExist(statErr))
}
