package deskconn

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

var errFilePathEscapesHome = errors.New("relative path escapes home directory")

type FileEntry struct {
	Name       string    `json:"name"`
	Path       string    `json:"path"`
	Type       string    `json:"type"`
	Mode       string    `json:"mode"`
	Size       int64     `json:"size"`
	Hidden     bool      `json:"hidden"`
	ModTime    time.Time `json:"mod_time"`
	IsDir      bool      `json:"is_dir"`
	IsSymlink  bool      `json:"is_symlink"`
	LinkTarget string    `json:"link_target,omitempty"`
}

type FileBrowseResult struct {
	Path       string      `json:"path"`
	HomePath   string      `json:"home_path"`
	ParentPath string      `json:"parent_path,omitempty"`
	Type       string      `json:"type"`
	Mode       string      `json:"mode"`
	Size       int64       `json:"size"`
	ModTime    time.Time   `json:"mod_time"`
	IsDir      bool        `json:"is_dir"`
	IsSymlink  bool        `json:"is_symlink"`
	LinkTarget string      `json:"link_target,omitempty"`
	Entries    []FileEntry `json:"entries,omitempty"`
}

type FileBrowser struct{}

func NewFileBrowser() *FileBrowser {
	return &FileBrowser{}
}

func (f *FileBrowser) Browse(pathArg string) (*FileBrowseResult, error) {
	homeDir, err := os.UserHomeDir()
	if err != nil {
		return nil, fmt.Errorf("failed to get home dir: %w", err)
	}

	resolvedPath, err := resolveBrowsePath(homeDir, pathArg)
	if err != nil {
		return nil, err
	}

	info, err := os.Lstat(resolvedPath)
	if err != nil {
		return nil, err
	}

	result := buildFileBrowseResult(homeDir, resolvedPath, info)

	if !info.IsDir() {
		return result, nil
	}

	entries, err := os.ReadDir(resolvedPath)
	if err != nil {
		return nil, err
	}

	result.Entries = make([]FileEntry, 0, len(entries))
	for _, entry := range entries {
		entryPath := filepath.Join(resolvedPath, entry.Name())
		entryInfo, err := os.Lstat(entryPath)
		if err != nil {
			return nil, err
		}

		result.Entries = append(result.Entries, buildFileEntry(entryPath, entryInfo))
	}

	sort.Slice(result.Entries, func(i, j int) bool {
		if result.Entries[i].IsDir != result.Entries[j].IsDir {
			return result.Entries[i].IsDir
		}
		return strings.ToLower(result.Entries[i].Name) < strings.ToLower(result.Entries[j].Name)
	})

	return result, nil
}

func resolveBrowsePath(homeDir, pathArg string) (string, error) {
	if pathArg == "" {
		return filepath.Clean(homeDir), nil
	}

	if filepath.IsAbs(pathArg) {
		return filepath.Clean(pathArg), nil
	}

	resolvedPath := filepath.Clean(filepath.Join(homeDir, pathArg))
	rel, err := filepath.Rel(homeDir, resolvedPath)
	if err != nil {
		return "", err
	}
	if rel == ".." || strings.HasPrefix(rel, ".."+string(os.PathSeparator)) {
		return "", errFilePathEscapesHome
	}

	return resolvedPath, nil
}

func buildFileBrowseResult(homeDir, path string, info os.FileInfo) *FileBrowseResult {
	result := &FileBrowseResult{
		Path:      path,
		HomePath:  filepath.Clean(homeDir),
		Type:      fileTypeFromInfo(info),
		Mode:      info.Mode().String(),
		Size:      info.Size(),
		ModTime:   info.ModTime(),
		IsDir:     info.IsDir(),
		IsSymlink: info.Mode()&os.ModeSymlink != 0,
	}

	cleanPath := filepath.Clean(path)
	parentPath := filepath.Dir(cleanPath)
	if parentPath != cleanPath {
		result.ParentPath = parentPath
	}

	if result.IsSymlink {
		if target, err := os.Readlink(path); err == nil {
			result.LinkTarget = target
		}
	}

	return result
}

func buildFileEntry(path string, info os.FileInfo) FileEntry {
	entry := FileEntry{
		Name:      info.Name(),
		Path:      path,
		Type:      fileTypeFromInfo(info),
		Mode:      info.Mode().String(),
		Size:      info.Size(),
		Hidden:    strings.HasPrefix(info.Name(), "."),
		ModTime:   info.ModTime(),
		IsDir:     info.IsDir(),
		IsSymlink: info.Mode()&os.ModeSymlink != 0,
	}

	if entry.IsSymlink {
		if target, err := os.Readlink(path); err == nil {
			entry.LinkTarget = target
		}
	}

	return entry
}

func fileTypeFromInfo(info os.FileInfo) string {
	switch mode := info.Mode(); {
	case mode.IsDir():
		return "directory"
	case mode.IsRegular():
		return "file"
	case mode&os.ModeSymlink != 0:
		return "symlink"
	default:
		return "other"
	}
}
