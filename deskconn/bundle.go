package deskconn

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/xconnio/deskconn/common"
)

// ExtractAITarball reverses BuildTarball, writing each entry into the Claude Code project
// directory for path under homeDir - re-derived using this machine's own homeDir, so the same
// project lands in the right place regardless of username or home directory location. Returns
// the number of files written. Entries aren't plain filenames (e.g. contain a path separator or
// "..") are rejected.
func ExtractAITarball(tarball []byte, homeDir, path string) (int, error) {
	destDir := filepath.Join(homeDir, ".claude", "projects", common.ClaudeProjectDir(homeDir, path))

	gz, err := gzip.NewReader(bytes.NewReader(tarball))
	if err != nil {
		return 0, fmt.Errorf("failed to open gzip stream: %w", err)
	}
	defer gz.Close() //nolint:errcheck

	count := 0
	tr := tar.NewReader(gz)
	for {
		header, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return count, nil
		}
		if err != nil {
			return 0, fmt.Errorf("failed to read tar entry: %w", err)
		}
		if header.Typeflag != tar.TypeReg {
			continue
		}
		if header.Name != filepath.Base(header.Name) || header.Name == ".." {
			return 0, fmt.Errorf("refusing to extract entry with unexpected name %q", header.Name)
		}

		if err := os.MkdirAll(destDir, 0700); err != nil {
			return 0, fmt.Errorf("failed to create directory %s: %w", destDir, err)
		}
		// header.Name is verified above to be a plain basename, not a path.
		dest := filepath.Join(destDir, header.Name)                             //nolint:gosec
		out, err := os.OpenFile(dest, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0600) //nolint:gosec
		if err != nil {
			return 0, fmt.Errorf("failed to create %s: %w", dest, err)
		}
		if _, err := io.Copy(out, tr); err != nil { //nolint:gosec
			_ = out.Close()
			return 0, fmt.Errorf("failed to write %s: %w", dest, err)
		}
		if err := out.Close(); err != nil {
			return 0, fmt.Errorf("failed to close %s: %w", dest, err)
		}
		count++
	}
}
