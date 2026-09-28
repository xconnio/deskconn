package deskconnd

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"fmt"
	"os"
	"path/filepath"

	"github.com/xconnio/deskconn/common"
)

// BuildAITarball tars the given session files (by basename only - not by their on-disk path,
// which is specific to this machine's home directory) into a gzip-compressed archive.
// ExtractTarball re-derives the correct destination directory independently, using the
// extracting machine's own home directory and the same project path.
func BuildAITarball(files []common.AISessionFile) ([]byte, error) {
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)

	for _, f := range files {
		data, err := os.ReadFile(f.Path) //nolint:gosec
		if err != nil {
			return nil, fmt.Errorf("failed to read %s: %w", f.Path, err)
		}
		name := filepath.Base(f.Path)

		if err := tw.WriteHeader(&tar.Header{
			Name:    name,
			Mode:    0600,
			Size:    int64(len(data)),
			ModTime: f.ModTime,
		}); err != nil {
			return nil, fmt.Errorf("failed to write tar header for %s: %w", name, err)
		}
		if _, err := tw.Write(data); err != nil {
			return nil, fmt.Errorf("failed to write tar entry for %s: %w", name, err)
		}
	}

	if err := tw.Close(); err != nil {
		return nil, fmt.Errorf("failed to finalize tar archive: %w", err)
	}
	if err := gz.Close(); err != nil {
		return nil, fmt.Errorf("failed to finalize gzip stream: %w", err)
	}

	return buf.Bytes(), nil
}
