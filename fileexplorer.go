package deskconn

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/xconnio/xconn-go"
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

// resolveOperationPath resolves a path and always enforces home-directory containment.
func resolveOperationPath(homeDir, pathArg string) (string, error) {
	if pathArg == "" {
		return "", errors.New("path cannot be empty")
	}

	var resolved string
	if filepath.IsAbs(pathArg) {
		resolved = filepath.Clean(pathArg)
	} else {
		resolved = filepath.Clean(filepath.Join(homeDir, pathArg))
	}

	rel, err := filepath.Rel(homeDir, resolved)
	if err != nil {
		return "", err
	}
	if rel == ".." || strings.HasPrefix(rel, ".."+string(os.PathSeparator)) {
		return "", errFilePathEscapesHome
	}

	return resolved, nil
}

func (f *FileBrowser) Rename(oldPath, newPath string) error {
	homeDir, err := os.UserHomeDir()
	if err != nil {
		return fmt.Errorf("failed to get home dir: %w", err)
	}

	resolvedOld, err := resolveOperationPath(homeDir, oldPath)
	if err != nil {
		return err
	}
	resolvedNew, err := resolveOperationPath(homeDir, newPath)
	if err != nil {
		return err
	}

	if dstInfo, err := os.Lstat(resolvedNew); err == nil && dstInfo.IsDir() {
		resolvedNew = filepath.Join(resolvedNew, filepath.Base(resolvedOld))
	}

	return os.Rename(resolvedOld, resolvedNew)
}

func (f *FileBrowser) Delete(path string) error {
	homeDir, err := os.UserHomeDir()
	if err != nil {
		return fmt.Errorf("failed to get home dir: %w", err)
	}

	resolved, err := resolveOperationPath(homeDir, path)
	if err != nil {
		return err
	}

	if filepath.Clean(resolved) == filepath.Clean(homeDir) {
		return errors.New("cannot delete home directory")
	}

	if _, err := os.Lstat(resolved); err != nil {
		return err
	}

	return os.RemoveAll(resolved)
}

func (f *FileBrowser) Copy(srcPath, dstPath string) error {
	homeDir, err := os.UserHomeDir()
	if err != nil {
		return fmt.Errorf("failed to get home dir: %w", err)
	}

	resolvedSrc, err := resolveOperationPath(homeDir, srcPath)
	if err != nil {
		return err
	}
	resolvedDst, err := resolveOperationPath(homeDir, dstPath)
	if err != nil {
		return err
	}

	srcInfo, err := os.Lstat(resolvedSrc)
	if err != nil {
		return err
	}

	if dstInfo, err := os.Lstat(resolvedDst); err == nil && dstInfo.IsDir() {
		resolvedDst = filepath.Join(resolvedDst, filepath.Base(resolvedSrc))
	}

	if srcInfo.IsDir() {
		srcClean := filepath.Clean(resolvedSrc)
		dstClean := filepath.Clean(resolvedDst)
		if dstClean == srcClean || strings.HasPrefix(dstClean, srcClean+string(os.PathSeparator)) {
			return fmt.Errorf("cannot copy a directory into itself")
		}
		return copyDir(resolvedSrc, resolvedDst)
	}
	return copyFile(resolvedSrc, resolvedDst)
}

func copyFile(src, dst string) error {
	srcInfo, err := os.Stat(src)
	if err != nil {
		return err
	}

	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()

	out, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, srcInfo.Mode())
	if err != nil {
		return err
	}

	_, copyErr := io.Copy(out, in)
	closeErr := out.Close()
	if copyErr != nil {
		return copyErr
	}
	return closeErr
}

func copyDir(src, dst string) error {
	srcInfo, err := os.Stat(src)
	if err != nil {
		return err
	}

	if err := os.MkdirAll(dst, srcInfo.Mode()); err != nil {
		return err
	}

	entries, err := os.ReadDir(src)
	if err != nil {
		return err
	}

	for _, entry := range entries {
		srcPath := filepath.Join(src, entry.Name())
		dstPath := filepath.Join(dst, entry.Name())

		if entry.IsDir() {
			if err := copyDir(srcPath, dstPath); err != nil {
				return err
			}
		} else {
			if err := copyFile(srcPath, dstPath); err != nil {
				return err
			}
		}
	}

	return nil
}

func (d *Deskconn) handleFileRename(_ context.Context, inv *xconn.Invocation) *xconn.InvocationResult {
	enc, ok := d.keys.fetch(inv.Caller())
	if !ok {
		return xconn.NewInvocationError(ErrInvalidArgument, "no session keys found, call key exchange first")
	}

	encrypted, err := inv.ArgBytes(0)
	if err != nil {
		return xconn.NewInvocationError(ErrInvalidArgument, err.Error())
	}
	plaintext, err := DecryptPayload(encrypted, enc.receiveKey)
	if err != nil {
		return xconn.NewInvocationError(ErrInvalidArgument, err.Error())
	}

	var args struct {
		OldPath string `json:"old_path"`
		NewPath string `json:"new_path"`
	}
	if err := json.Unmarshal(plaintext, &args); err != nil {
		return xconn.NewInvocationError(ErrInvalidArgument, err.Error())
	}

	if err := d.files.Rename(args.OldPath, args.NewPath); err != nil {
		if errors.Is(err, errFilePathEscapesHome) || errors.Is(err, os.ErrNotExist) {
			return xconn.NewInvocationError(ErrInvalidArgument, err.Error())
		}
		return xconn.NewInvocationError(ErrOperationFailed, err.Error())
	}

	resultBytes, _ := json.Marshal(map[string]bool{"ok": true})
	encryptedResult, err := EncryptPayload(resultBytes, enc.sendKey)
	if err != nil {
		return xconn.NewInvocationError(ErrOperationFailed, err.Error())
	}
	return xconn.NewInvocationResult(encryptedResult)
}

func (d *Deskconn) handleFileDelete(_ context.Context, inv *xconn.Invocation) *xconn.InvocationResult {
	enc, ok := d.keys.fetch(inv.Caller())
	if !ok {
		return xconn.NewInvocationError(ErrInvalidArgument, "no session keys found, call key exchange first")
	}

	encrypted, err := inv.ArgBytes(0)
	if err != nil {
		return xconn.NewInvocationError(ErrInvalidArgument, err.Error())
	}
	plaintext, err := DecryptPayload(encrypted, enc.receiveKey)
	if err != nil {
		return xconn.NewInvocationError(ErrInvalidArgument, err.Error())
	}

	var args struct {
		Path string `json:"path"`
	}
	if err := json.Unmarshal(plaintext, &args); err != nil {
		return xconn.NewInvocationError(ErrInvalidArgument, err.Error())
	}

	if err := d.files.Delete(args.Path); err != nil {
		if errors.Is(err, errFilePathEscapesHome) || errors.Is(err, os.ErrNotExist) {
			return xconn.NewInvocationError(ErrInvalidArgument, err.Error())
		}
		return xconn.NewInvocationError(ErrOperationFailed, err.Error())
	}

	resultBytes, _ := json.Marshal(map[string]bool{"ok": true})
	encryptedResult, err := EncryptPayload(resultBytes, enc.sendKey)
	if err != nil {
		return xconn.NewInvocationError(ErrOperationFailed, err.Error())
	}
	return xconn.NewInvocationResult(encryptedResult)
}

func (d *Deskconn) handleFileCopy(_ context.Context, inv *xconn.Invocation) *xconn.InvocationResult {
	enc, ok := d.keys.fetch(inv.Caller())
	if !ok {
		return xconn.NewInvocationError(ErrInvalidArgument, "no session keys found, call key exchange first")
	}

	encrypted, err := inv.ArgBytes(0)
	if err != nil {
		return xconn.NewInvocationError(ErrInvalidArgument, err.Error())
	}
	plaintext, err := DecryptPayload(encrypted, enc.receiveKey)
	if err != nil {
		return xconn.NewInvocationError(ErrInvalidArgument, err.Error())
	}

	var args struct {
		Src string `json:"src"`
		Dst string `json:"dst"`
	}
	if err := json.Unmarshal(plaintext, &args); err != nil {
		return xconn.NewInvocationError(ErrInvalidArgument, err.Error())
	}

	if err := d.files.Copy(args.Src, args.Dst); err != nil {
		if errors.Is(err, errFilePathEscapesHome) || errors.Is(err, os.ErrNotExist) {
			return xconn.NewInvocationError(ErrInvalidArgument, err.Error())
		}
		return xconn.NewInvocationError(ErrOperationFailed, err.Error())
	}

	resultBytes, _ := json.Marshal(map[string]bool{"ok": true})
	encryptedResult, err := EncryptPayload(resultBytes, enc.sendKey)
	if err != nil {
		return xconn.NewInvocationError(ErrOperationFailed, err.Error())
	}
	return xconn.NewInvocationResult(encryptedResult)
}
