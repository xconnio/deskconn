package ai

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
)

// BuildTarball tars the given session files (by basename only - not by their on-disk path,
// which is specific to this machine's home directory) into a gzip-compressed archive.
// ExtractTarball re-derives the correct destination directory independently, using the
// extracting machine's own home directory and the same project path.
func BuildTarball(files []SessionFile) ([]byte, error) {
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

// ExtractTarball reverses BuildTarball, writing each entry into the Claude Code project
// directory for path under homeDir - re-derived using this machine's own homeDir, so the same
// project lands in the right place regardless of username or home directory location. Returns
// the number of files written. Entries aren't plain filenames (e.g. contain a path separator or
// "..") are rejected.
func ExtractTarball(tarball []byte, homeDir, path string) (int, error) {
	destDir := filepath.Join(homeDir, ".claude", "projects", claudeProjectDir(homeDir, path))

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
