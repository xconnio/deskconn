package userd

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"sync"
	"syscall"
	"time"

	"github.com/xconnio/deskconn/internal/common"
	"github.com/xconnio/xconn-core/bindings/go"
)

// handleMountFS serves the operations of a `desk file mount` of this machine.
// Filesystem failures are returned as an errno in a successful result (see
// common.ProcedureMountFS); only protocol problems become WAMP errors.
func (d *Deskconn) handleMountFS(_ context.Context, inv *xconn.Invocation) *xconn.Result {
	enc, ok := d.keys.fetch(inv.Caller())
	if !ok {
		return xconn.NewResultError(common.ErrInvalidArgument, "no session keys found, call key exchange first")
	}

	encrypted, err := inv.ArgBytes(0)
	if err != nil {
		return xconn.NewResultError(common.ErrInvalidArgument, err.Error())
	}
	plaintext, err := common.DecryptPayload(encrypted, enc.receiveKey)
	if err != nil {
		return xconn.NewResultError(common.ErrInvalidArgument, err.Error())
	}

	resultBytes, err := d.mountHandles.call(inv.Caller(), plaintext)
	if err != nil {
		return xconn.NewResultError(common.ErrInvalidArgument, err.Error())
	}
	encryptedResult, err := common.EncryptPayload(resultBytes, enc.sendKey)
	if err != nil {
		return xconn.NewResultError(common.ErrOperationFailed, err.Error())
	}
	return xconn.NewResult(encryptedResult)
}

// mountMaxHandles bounds the files a machine keeps open for mounts.
const mountMaxHandles = 4096

// mountHandles are the files mounts have open (MountOpOpen), each owned by the
// session that opened it and closed when that session leaves. Writes need them:
// a file created read-only (as git does with its objects) can still be written
// through the descriptor that created it, but not reopened for writing.
type mountHandles struct {
	mu    sync.Mutex
	next  uint64
	files map[uint64]*mountHandle
}

type mountHandle struct {
	caller uint64
	file   *os.File
}

func newMountHandles() *mountHandles {
	return &mountHandles{files: make(map[uint64]*mountHandle)}
}

func (h *mountHandles) add(caller uint64, f *os.File) (uint64, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if len(h.files) >= mountMaxHandles {
		return 0, syscall.EMFILE
	}
	h.next++
	h.files[h.next] = &mountHandle{caller: caller, file: f}
	return h.next, nil
}

func (h *mountHandles) file(caller, id uint64) (*os.File, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	handle, ok := h.files[id]
	if !ok || handle.caller != caller {
		return nil, syscall.EBADF
	}
	return handle.file, nil
}

func (h *mountHandles) release(caller, id uint64) error {
	h.mu.Lock()
	handle, ok := h.files[id]
	if ok && handle.caller == caller {
		delete(h.files, id)
	}
	h.mu.Unlock()
	if !ok || handle.caller != caller {
		return syscall.EBADF
	}
	return handle.file.Close()
}

// closeSession closes the files caller left open.
func (h *mountHandles) closeSession(caller uint64) {
	h.mu.Lock()
	var files []*os.File
	for id, handle := range h.files {
		if handle.caller == caller {
			files = append(files, handle.file)
			delete(h.files, id)
		}
	}
	h.mu.Unlock()
	for _, f := range files {
		_ = f.Close()
	}
}

// call runs one common.MountRequest from caller, framed by
// common.EncodeMountMessage, returning the framed common.MountResponse.
func (h *mountHandles) call(caller uint64, payload []byte) ([]byte, error) {
	var req common.MountRequest
	data, err := common.DecodeMountMessage(payload, &req)
	if err != nil {
		return nil, err
	}
	req.Data = data

	resp, err := h.operation(caller, &req)
	if err != nil {
		resp = &common.MountResponse{Errno: common.MountErrnoName(err)}
	}
	return common.EncodeMountMessage(resp, resp.Data)
}

func (h *mountHandles) operation(caller uint64, req *common.MountRequest) (*common.MountResponse, error) {
	// Handle operations don't need a path.
	switch req.Op {
	case common.MountOpRead:
		if req.Handle == 0 {
			break
		}
		f, err := h.file(caller, req.Handle)
		if err != nil {
			return nil, err
		}
		return fsRead(f, req)
	case common.MountOpWrite:
		if req.Handle == 0 {
			break
		}
		f, err := h.file(caller, req.Handle)
		if err != nil {
			return nil, err
		}
		return fsWrite(f, req)
	case common.MountOpFsync:
		f, err := h.file(caller, req.Handle)
		if err != nil {
			return nil, err
		}
		return &common.MountResponse{}, f.Sync()
	case common.MountOpRelease:
		return &common.MountResponse{}, h.release(caller, req.Handle)
	}

	homeDir, err := os.UserHomeDir()
	if err != nil {
		return nil, err
	}
	name, err := fsResolve(homeDir, req.Path)
	if err != nil {
		return nil, err
	}
	// Everything goes through home as an os.Root, which refuses paths that
	// leave it, through symlinks too: the mount can create symlinks, so
	// checking the path alone would let one reach any file outside home.
	root, err := os.OpenRoot(homeDir)
	if err != nil {
		return nil, err
	}
	defer root.Close()

	switch req.Op {
	case common.MountOpStat:
		if req.Follow {
			return fsStatResponse(root.Stat(name))
		}
		return fsStatResponse(root.Lstat(name))

	case common.MountOpReadDir:
		dir, err := root.Open(name)
		if err != nil {
			return nil, err
		}
		entries, err := dir.ReadDir(-1)
		_ = dir.Close()
		if err != nil {
			return nil, err
		}
		// A listed directory may be a symlink to one: describe the directory.
		resp, err := fsStatResponse(root.Stat(name))
		if err != nil {
			return nil, err
		}
		resp.Entries = make([]common.MountDirEntry, 0, len(entries))
		for _, entry := range entries {
			info, err := root.Lstat(filepath.Join(name, entry.Name()))
			if err != nil {
				continue // removed since ReadDir
			}
			resp.Entries = append(resp.Entries, common.MountDirEntry{Name: entry.Name(), Attr: fsAttr(info)})
		}
		return resp, nil

	case common.MountOpRead:
		f, err := root.Open(name)
		if err != nil {
			return nil, err
		}
		defer f.Close()
		return fsRead(f, req)

	case common.MountOpWrite:
		f, err := root.OpenFile(name, os.O_WRONLY, 0)
		if err != nil {
			return nil, err
		}
		resp, err := fsWrite(f, req)
		if closeErr := f.Close(); err == nil {
			err = closeErr
		}
		return resp, err

	case common.MountOpOpen:
		var flags int
		switch req.Access {
		case common.MountAccessRead:
			flags = os.O_RDONLY
		case common.MountAccessWrite:
			flags = os.O_WRONLY
		case common.MountAccessReadWrite:
			flags = os.O_RDWR
		default:
			return nil, syscall.EINVAL
		}
		if req.Create {
			flags |= os.O_CREATE
			if req.Exclusive {
				flags |= os.O_EXCL
			}
		}
		if req.Truncate {
			flags |= os.O_TRUNC
		}
		// O_APPEND isn't passed on: the kernel already sends appending writes at
		// the end of the file, and os.File refuses WriteAt on an O_APPEND file.
		f, err := root.OpenFile(name, flags, os.FileMode(req.Mode&0o777))
		if err != nil {
			return nil, err
		}
		resp, err := fsStatResponse(f.Stat())
		if err != nil {
			_ = f.Close()
			return nil, err
		}
		if resp.Handle, err = h.add(caller, f); err != nil {
			_ = f.Close()
			return nil, err
		}
		return resp, nil

	case common.MountOpMkdir:
		if err := root.Mkdir(name, os.FileMode(req.Mode&0o777)); err != nil {
			return nil, err
		}
		return fsStatResponse(root.Lstat(name))

	case common.MountOpUnlink, common.MountOpRmdir:
		if name == "." {
			return nil, syscall.EBUSY
		}
		info, err := root.Lstat(name)
		if err != nil {
			return nil, err
		}
		if req.Op == common.MountOpUnlink && info.IsDir() {
			return nil, syscall.EISDIR
		}
		if req.Op == common.MountOpRmdir && !info.IsDir() {
			return nil, syscall.ENOTDIR
		}
		// Remove only removes empty directories, giving rmdir semantics.
		return &common.MountResponse{}, root.Remove(name)

	case common.MountOpRename:
		newName, err := fsResolve(homeDir, req.NewPath)
		if err != nil {
			return nil, err
		}
		if name == "." || newName == "." {
			return nil, syscall.EBUSY
		}
		if req.NoReplace {
			if _, err := root.Lstat(newName); err == nil {
				return nil, syscall.EEXIST
			}
		}
		return &common.MountResponse{}, root.Rename(name, newName)

	case common.MountOpChange:
		if req.NewMode != nil {
			if err := root.Chmod(name, os.FileMode(*req.NewMode&0o777)); err != nil {
				return nil, err
			}
		}
		if req.NewSize != nil {
			if err := fsTruncate(root, name, *req.NewSize); err != nil {
				return nil, err
			}
		}
		if req.NewAtime != nil || req.NewMtime != nil {
			info, err := root.Stat(name)
			if err != nil {
				return nil, err
			}
			current := fsAttr(info)
			atime, mtime := current.Atime, current.Mtime
			if req.NewAtime != nil {
				atime = *req.NewAtime
			}
			if req.NewMtime != nil {
				mtime = *req.NewMtime
			}
			if err := root.Chtimes(name, time.Unix(0, atime), time.Unix(0, mtime)); err != nil {
				return nil, err
			}
		}
		return fsStatResponse(root.Lstat(name))

	case common.MountOpSymlink:
		if err := root.Symlink(req.Target, name); err != nil {
			return nil, err
		}
		return fsStatResponse(root.Lstat(name))

	case common.MountOpReadlink:
		target, err := root.Readlink(name)
		if err != nil {
			return nil, err
		}
		return &common.MountResponse{Target: target}, nil

	case common.MountOpStatfs:
		// Only sizes come back, so the path needn't be confined like the rest.
		st, err := fsStatfs(filepath.Join(homeDir, name))
		if err != nil {
			return nil, err
		}
		return &common.MountResponse{Statfs: st}, nil
	}

	return nil, syscall.ENOTSUP
}

func fsTruncate(root *os.Root, name string, size int64) error {
	f, err := root.OpenFile(name, os.O_WRONLY, 0)
	if err != nil {
		return err
	}
	err = f.Truncate(size)
	if closeErr := f.Close(); err == nil {
		err = closeErr
	}
	return err
}

func fsRead(f *os.File, req *common.MountRequest) (*common.MountResponse, error) {
	if req.Length < 0 || req.Length > common.MountMaxIO || req.Offset < 0 {
		return nil, syscall.EINVAL
	}
	buf := make([]byte, req.Length)
	n, err := f.ReadAt(buf, req.Offset)
	if err != nil && !errors.Is(err, io.EOF) {
		return nil, err
	}
	return &common.MountResponse{Data: buf[:n]}, nil
}

func fsWrite(f *os.File, req *common.MountRequest) (*common.MountResponse, error) {
	if len(req.Data) > common.MountMaxIO || req.Offset < 0 {
		return nil, syscall.EINVAL
	}
	n, err := f.WriteAt(req.Data, req.Offset)
	if err != nil {
		return nil, err
	}
	return &common.MountResponse{Written: n}, nil
}

// fsResolve resolves a mount path the same way the other file operations
// do, relative to home and never outside it, returning it relative to home
// ("." for home itself, which is also what an empty path is).
func fsResolve(homeDir, path string) (string, error) {
	if path == "" {
		return ".", nil
	}
	resolved, err := resolveOperationPath(homeDir, path)
	if errors.Is(err, errFilePathEscapesHome) {
		return "", syscall.EACCES
	}
	if err != nil {
		return "", syscall.EINVAL
	}
	return filepath.Rel(homeDir, resolved)
}

func fsStatResponse(info os.FileInfo, err error) (*common.MountResponse, error) {
	if err != nil {
		return nil, err
	}
	attr := fsAttr(info)
	return &common.MountResponse{Attr: &attr}, nil
}

func fsAttr(info os.FileInfo) common.MountAttr {
	attr := common.MountAttr{
		Size:  info.Size(),
		Nlink: 1,
		Mtime: info.ModTime().UnixNano(),
	}
	attr.Atime, attr.Ctime = attr.Mtime, attr.Mtime
	if fsPlatformAttr(info, &attr) {
		return attr
	}

	attr.Mode = uint32(info.Mode().Perm())
	switch {
	case info.IsDir():
		attr.Mode |= syscall.S_IFDIR
	case info.Mode()&os.ModeSymlink != 0:
		attr.Mode |= syscall.S_IFLNK
	default:
		attr.Mode |= syscall.S_IFREG
	}
	return attr
}
