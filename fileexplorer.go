package deskconn

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"image"
	_ "image/gif"
	"image/jpeg"
	_ "image/png"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/sergi/go-diff/diffmatchpatch"

	"github.com/xconnio/xconn-go"
)

var errFilePathEscapesHome = errors.New("relative path escapes home directory")
var errEditConflict = errors.New("file changed on the device, patch could not be applied cleanly")

const binarySniffLen = 8000

func IsEditableExtension(name string) bool {
	switch fileCategory(name) {
	case CategoryImages, CategoryVideos, CategoryPDFs, CategoryDocuments:
		return false
	default:
		return true
	}
}

func isBinaryContent(content []byte) bool {
	sniffLen := len(content)
	if sniffLen > binarySniffLen {
		sniffLen = binarySniffLen
	}
	return bytes.IndexByte(content[:sniffLen], 0) != -1
}

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
	ItemCount  *int      `json:"item_count,omitempty"`
	Thumbnail  string    `json:"thumbnail,omitempty"`
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
	NextCursor string      `json:"next_cursor,omitempty"`
	HasMore    bool        `json:"has_more"`
}

type FileBrowser struct{}

func NewFileBrowser() *FileBrowser {
	return &FileBrowser{}
}

// Browse lists the contents of pathArg. If limit > 0, results are paginated:
// cursor (an opaque string from a previous call's NextCursor) resumes after
// the last entry of that page, and HasMore/NextCursor are set when more
// entries remain. limit <= 0 returns every entry in one page (legacy
// behavior for callers that don't paginate, e.g. shell completion).
func (f *FileBrowser) Browse(pathArg, cursor string, limit int) (*FileBrowseResult, error) {
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

	all := make([]FileEntry, 0, len(entries))
	for _, entry := range entries {
		entryPath := filepath.Join(resolvedPath, entry.Name())
		entryInfo, err := os.Lstat(entryPath)
		if err != nil {
			return nil, err
		}

		fe := buildFileEntry(entryPath, entryInfo)
		if fe.IsDir {
			if subEntries, err := os.ReadDir(entryPath); err == nil {
				n := len(subEntries)
				fe.ItemCount = &n
			}
		}
		all = append(all, fe)
	}

	sort.Slice(all, func(i, j int) bool {
		if all[i].IsDir != all[j].IsDir {
			return all[i].IsDir
		}
		return strings.ToLower(all[i].Name) < strings.ToLower(all[j].Name)
	})

	start := 0
	if cursor != "" {
		cursorKey, err := decodeBrowseCursor(cursor)
		if err != nil {
			return nil, fmt.Errorf("invalid cursor: %w", err)
		}
		start = sort.Search(len(all), func(i int) bool {
			return fileEntrySortKey(all[i]) > cursorKey
		})
	}

	end := len(all)
	hasMore := false
	if limit > 0 && start+limit < len(all) {
		end = start + limit
		hasMore = true
	}
	page := all[start:end]

	generateThumbnails(page)

	result.Entries = page
	result.HasMore = hasMore
	if hasMore {
		result.NextCursor = encodeBrowseCursor(fileEntrySortKey(page[len(page)-1]))
	}

	return result, nil
}

// fileEntrySortKey returns a string that sorts identically to the directory
// listing order (directories first, then case-insensitive name), so it can
// be used as a pagination cursor position.
func fileEntrySortKey(e FileEntry) string {
	dirRank := "1"
	if e.IsDir {
		dirRank = "0"
	}
	return dirRank + "\x00" + strings.ToLower(e.Name)
}

func encodeBrowseCursor(key string) string {
	return base64.StdEncoding.EncodeToString([]byte(key))
}

func decodeBrowseCursor(cursor string) (string, error) {
	decoded, err := base64.StdEncoding.DecodeString(cursor)
	if err != nil {
		return "", err
	}
	return string(decoded), nil
}

// generateThumbnails fills in Thumbnail for image and video entries in
// parallel. Only called for the page actually returned to the caller.
func generateThumbnails(entries []FileEntry) {
	var wg sync.WaitGroup
	sem := make(chan struct{}, 4)
	for i := range entries {
		e := &entries[i]
		if e.IsDir || e.IsSymlink {
			continue
		}
		if isImageFile(e.Name) && e.Size <= maxThumbnailSourceSize {
			wg.Add(1)
			SafeGo(func() {
				defer wg.Done()
				sem <- struct{}{}
				defer func() { <-sem }()
				e.Thumbnail = generateThumbnail(e.Path)
			})
		} else if isVideoFile(e.Name) {
			wg.Add(1)
			SafeGo(func() {
				defer wg.Done()
				sem <- struct{}{}
				defer func() { <-sem }()
				e.Thumbnail = generateVideoThumbnail(e.Path)
			})
		}
	}
	wg.Wait()
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

const maxThumbnailSourceSize = 20 * 1024 * 1024 // 20 MB
const thumbnailMaxDim = 160

func isImageFile(name string) bool {
	switch strings.ToLower(filepath.Ext(name)) {
	case extJpg, extJpeg, extPng, extGif:
		return true
	}
	return false
}

func isVideoFile(name string) bool {
	switch strings.ToLower(filepath.Ext(name)) {
	case ".mp4", ".webm", ".mov", ".avi", ".mkv", ".ogv", ".flv", ".wmv":
		return true
	}
	return false
}

func generateThumbnail(path string) string {
	f, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer f.Close()

	src, _, err := image.Decode(f)
	if err != nil {
		return ""
	}

	thumb := scaledImage(src, thumbnailMaxDim)

	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, thumb, &jpeg.Options{Quality: 75}); err != nil {
		return ""
	}
	return base64.StdEncoding.EncodeToString(buf.Bytes())
}

func generateVideoThumbnail(path string) string {
	ffmpeg, err := exec.LookPath("ffmpeg")
	if err != nil {
		return ""
	}

	cmd := exec.Command(ffmpeg,
		"-ss", "00:00:01",
		"-i", path,
		"-vframes", "1",
		"-vf", fmt.Sprintf("scale=%d:-1", thumbnailMaxDim),
		"-f", "image2pipe",
		"-vcodec", "mjpeg",
		"-",
	)
	out, err := cmd.Output()
	if err != nil || len(out) == 0 {
		return ""
	}
	return base64.StdEncoding.EncodeToString(out)
}

func scaledImage(src image.Image, maxDim int) image.Image {
	b := src.Bounds()
	srcW, srcH := b.Dx(), b.Dy()
	if srcW <= maxDim && srcH <= maxDim {
		return src
	}

	dstW, dstH := maxDim, maxDim
	if srcW > srcH {
		dstH = srcH * maxDim / srcW
	} else {
		dstW = srcW * maxDim / srcH
	}
	if dstW < 1 {
		dstW = 1
	}
	if dstH < 1 {
		dstH = 1
	}

	dst := image.NewRGBA(image.Rect(0, 0, dstW, dstH))
	for y := 0; y < dstH; y++ {
		for x := 0; x < dstW; x++ {
			dst.Set(x, y, src.At(b.Min.X+x*srcW/dstW, b.Min.Y+y*srcH/dstH))
		}
	}
	return dst
}

// resolveOperationPath resolves a path and always enforces home-directory containment.
func resolveOperationPath(homeDir, pathArg string) (string, error) {
	if pathArg == "" {
		return "", errors.New("path cannot be empty")
	}

	// filepath.IsAbs requires a drive letter on Windows, so a POSIX-style rooted path like
	// "/etc/hosts" doesn't count as absolute there - without this, it would silently fall
	// through to the homeDir-relative branch below instead of being treated as escaping home.
	isRooted := filepath.IsAbs(pathArg) ||
		strings.HasPrefix(pathArg, "/") || strings.HasPrefix(pathArg, `\`)

	var resolved string
	if isRooted {
		resolved = filepath.Clean(pathArg)
	} else {
		resolved = filepath.Clean(filepath.Join(homeDir, pathArg))
	}

	rel, err := filepath.Rel(homeDir, resolved)
	if err != nil {
		return "", errFilePathEscapesHome
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

func (f *FileBrowser) Edit(pathArg, patchText string) error {
	homeDir, err := os.UserHomeDir()
	if err != nil {
		return fmt.Errorf("failed to get home dir: %w", err)
	}

	resolved, err := resolveOperationPath(homeDir, pathArg)
	if err != nil {
		return err
	}

	info, err := os.Lstat(resolved)
	if err != nil {
		return err
	}
	if info.IsDir() {
		return fmt.Errorf("%s: is a directory", pathArg)
	}

	original, err := os.ReadFile(resolved)
	if err != nil {
		return err
	}
	if isBinaryContent(original) {
		return fmt.Errorf("%s: binary file, editing is not supported", pathArg)
	}

	dmp := diffmatchpatch.New()
	patches, err := dmp.PatchFromText(patchText)
	if err != nil {
		return fmt.Errorf("invalid patch: %w", err)
	}

	patched, applied := dmp.PatchApply(patches, string(original))
	for _, ok := range applied {
		if !ok {
			return errEditConflict
		}
	}

	//nolint:gosec // resolved is validated by resolveOperationPath
	return os.WriteFile(resolved, []byte(patched), info.Mode().Perm())
}

func BuildEditPatch(original, edited []byte) string {
	dmp := diffmatchpatch.New()
	diffs := dmp.DiffMain(string(original), string(edited), false)
	patches := dmp.PatchMake(string(original), diffs)
	return dmp.PatchToText(patches)
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

const searchBatchSize = 50

func (f *FileBrowser) Search(pathArg, query string, showHidden bool, sendKey []byte, inv *xconn.Invocation) error {
	homeDir, err := os.UserHomeDir()
	if err != nil {
		return fmt.Errorf("failed to get home dir: %w", err)
	}

	resolvedPath, err := resolveBrowsePath(homeDir, pathArg)
	if err != nil {
		return err
	}

	lowerQuery := strings.ToLower(query)
	batch := make([]FileEntry, 0, searchBatchSize)
	var mediaPaths []string

	walkErr := filepath.WalkDir(resolvedPath, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if path == resolvedPath {
			return nil
		}
		if !showHidden && strings.HasPrefix(d.Name(), ".") {
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if strings.Contains(strings.ToLower(d.Name()), lowerQuery) {
			info, infoErr := d.Info()
			if infoErr != nil {
				return infoErr
			}
			batch = append(batch, buildFileEntry(path, info))
			if !d.IsDir() {
				name := d.Name()
				if (isImageFile(name) && info.Size() <= maxThumbnailSourceSize) || isVideoFile(name) {
					mediaPaths = append(mediaPaths, path)
				}
			}
			if len(batch) >= searchBatchSize {
				if sendErr := sendSearchBatch(inv, sendKey, batch); sendErr != nil {
					return sendErr
				}
				batch = batch[:0]
			}
		}
		return nil
	})
	if walkErr != nil {
		return walkErr
	}
	if len(batch) > 0 {
		if err := sendSearchBatch(inv, sendKey, batch); err != nil {
			return err
		}
	}

	if len(mediaPaths) == 0 {
		return nil
	}

	type thumbResult struct {
		path  string
		thumb string
	}
	results := make(chan thumbResult, len(mediaPaths))
	var wg sync.WaitGroup
	sem := make(chan struct{}, 4)

	for _, p := range mediaPaths {
		wg.Add(1)
		SafeGo(func() {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			name := filepath.Base(p)
			var thumb string
			if isImageFile(name) {
				thumb = generateThumbnail(p)
			} else {
				thumb = generateVideoThumbnail(p)
			}
			if thumb != "" {
				results <- thumbResult{path: p, thumb: thumb}
			}
		})
	}

	SafeGo(func() {
		wg.Wait()
		close(results)
	})

	for tr := range results {
		if err := sendSearchThumbnail(inv, sendKey, tr.path, tr.thumb); err != nil {
			return err
		}
	}

	return nil
}

func sendSearchBatch(inv *xconn.Invocation, sendKey []byte, entries []FileEntry) error {
	plaintext, err := json.Marshal(entries)
	if err != nil {
		return err
	}
	encrypted, err := EncryptPayload(plaintext, sendKey)
	if err != nil {
		return err
	}
	return inv.SendProgress([]any{encrypted}, nil)
}

func sendSearchThumbnail(inv *xconn.Invocation, sendKey []byte, path, thumb string) error {
	plaintext, err := json.Marshal(map[string]string{"path": path, "thumbnail": thumb})
	if err != nil {
		return err
	}
	encrypted, err := EncryptPayload(plaintext, sendKey)
	if err != nil {
		return err
	}
	return inv.SendProgress([]any{encrypted}, nil)
}

func (d *Deskconn) handleFileSearch(_ context.Context, inv *xconn.Invocation) *xconn.InvocationResult {
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
		Path       string `json:"path"`
		Query      string `json:"query"`
		ShowHidden bool   `json:"show_hidden"`
	}
	if err := json.Unmarshal(plaintext, &args); err != nil {
		return xconn.NewInvocationError(ErrInvalidArgument, err.Error())
	}

	if strings.TrimSpace(args.Query) == "" {
		return xconn.NewInvocationResult()
	}

	if err := d.files.Search(args.Path, args.Query, args.ShowHidden, enc.sendKey, inv); err != nil {
		if errors.Is(err, errFilePathEscapesHome) || errors.Is(err, os.ErrNotExist) {
			return xconn.NewInvocationError(ErrInvalidArgument, err.Error())
		}
		return xconn.NewInvocationError(ErrOperationFailed, err.Error())
	}

	return xconn.NewInvocationResult()
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

func (d *Deskconn) handleFileEdit(_ context.Context, inv *xconn.Invocation) *xconn.InvocationResult {
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
		Path  string `json:"path"`
		Patch string `json:"patch"`
	}
	if err := json.Unmarshal(plaintext, &args); err != nil {
		return xconn.NewInvocationError(ErrInvalidArgument, err.Error())
	}

	if err := d.files.Edit(args.Path, args.Patch); err != nil {
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
