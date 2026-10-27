package common

import (
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"syscall"
)

// ProcedureMountFS carries the POSIX-style operations `desk file mount` needs
// (stat, ranged read/write, create, mkdir, ...). It's a single procedure
// rather than one per operation so a failure can travel back as an errno
// in the (encrypted) result instead of a WAMP error string, which the
// mount has to turn back into the exact errno the kernel expects.
const ProcedureMountFS = "io.xconn.deskconn.file.mount"

// MountMaxIO bounds a single read or write request.
const MountMaxIO = 1024 * 1024

const (
	MountOpStat     = "stat"
	MountOpReadDir  = "readdir"
	MountOpRead     = "read"
	MountOpWrite    = "write"
	MountOpMkdir    = "mkdir"
	MountOpUnlink   = "unlink"
	MountOpRmdir    = "rmdir"
	MountOpRename   = "rename"
	MountOpChange   = "change"
	MountOpSymlink  = "symlink"
	MountOpReadlink = "readlink"
	MountOpStatfs   = "statfs"
	MountOpOpen     = "open"
	MountOpRelease  = "release"
	MountOpFsync    = "fsync"
)

// MountRequest.Access values for MountOpOpen.
const (
	MountAccessRead      = "r"
	MountAccessWrite     = "w"
	MountAccessReadWrite = "rw"
)

type MountRequest struct {
	Op   string `json:"op"`
	Path string `json:"path"`
	// Follow makes MountOpStat describe what a symlink at Path points to.
	Follow bool `json:"follow,omitempty"`
	// Handle is a file MountOpOpen opened, for MountOpRead, MountOpWrite,
	// MountOpFsync and MountOpRelease. Read and write fall back to opening Path when it's 0.
	Handle  uint64 `json:"handle,omitempty"`
	NewPath string `json:"new_path,omitempty"`
	// Target is the symlink target for MountOpSymlink.
	Target string `json:"target,omitempty"`
	Offset int64  `json:"offset,omitempty"`
	Length int    `json:"length,omitempty"`
	Data   []byte `json:"-"` // carried after the JSON, see EncodeMountMessage
	Mode   uint32 `json:"mode,omitempty"`
	// Access, Create and Truncate are MountOpOpen's open(2) flags.
	Access   string `json:"access,omitempty"`
	Create   bool   `json:"create,omitempty"`
	Truncate bool   `json:"truncate,omitempty"`
	// Exclusive makes a creating MountOpOpen, NoReplace MountOpRename, fail
	// with EEXIST if the destination already exists.
	Exclusive bool `json:"exclusive,omitempty"`
	NoReplace bool `json:"no_replace,omitempty"`
	// MountOpChange fields; nil leaves it unchanged.
	NewSize  *int64  `json:"new_size,omitempty"`
	NewMode  *uint32 `json:"new_mode,omitempty"`
	NewAtime *int64  `json:"new_atime,omitempty"` // unix nanoseconds
	NewMtime *int64  `json:"new_mtime,omitempty"` // unix nanoseconds
}

type MountAttr struct {
	Mode  uint32 `json:"mode"` // permission bits plus syscall.S_IF* type bits
	Size  int64  `json:"size"`
	Nlink uint32 `json:"nlink"`
	Atime int64  `json:"atime"` // unix nanoseconds
	Mtime int64  `json:"mtime"`
	Ctime int64  `json:"ctime"`
	// Dev and Ino identify the file on the machine; 0 where the machine's OS
	// doesn't say.
	Dev uint64 `json:"dev,omitempty"`
	Ino uint64 `json:"ino,omitempty"`
}

type MountDirEntry struct {
	Name string    `json:"name"`
	Attr MountAttr `json:"attr"`
}

type MountStatfs struct {
	Blocks  uint64 `json:"blocks"`
	Bfree   uint64 `json:"bfree"`
	Bavail  uint64 `json:"bavail"`
	Files   uint64 `json:"files"`
	Ffree   uint64 `json:"ffree"`
	Bsize   uint32 `json:"bsize"`
	NameLen uint32 `json:"name_len"`
}

type MountResponse struct {
	// Errno is the failure's errno name (see MountErrnoName), empty on success.
	Errno   string          `json:"errno,omitempty"`
	Handle  uint64          `json:"handle,omitempty"`
	Attr    *MountAttr      `json:"attr,omitempty"`
	Entries []MountDirEntry `json:"entries,omitempty"`
	Data    []byte          `json:"-"` // carried after the JSON, see EncodeMountMessage
	Written int             `json:"written,omitempty"`
	Target  string          `json:"target,omitempty"`
	Statfs  *MountStatfs    `json:"statfs,omitempty"`
}

// EncodeMountMessage frames a MountRequest or MountResponse for the wire: its
// JSON, then its file data raw, saving the third base64 would add to every
// read and write.
func EncodeMountMessage(v any, data []byte) ([]byte, error) {
	header, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	msg := make([]byte, 4, 4+len(header)+len(data))
	binary.BigEndian.PutUint32(msg, uint32(len(header))) // nolint: gosec
	msg = append(msg, header...)
	return append(msg, data...), nil
}

// DecodeMountMessage unmarshals msg's JSON into v and returns its file data.
func DecodeMountMessage(msg []byte, v any) ([]byte, error) {
	if len(msg) < 4 {
		return nil, fmt.Errorf("mount message too short: %d bytes", len(msg))
	}
	headerLen := int(binary.BigEndian.Uint32(msg))
	if headerLen > len(msg)-4 {
		return nil, fmt.Errorf("mount message header of %d bytes exceeds message", headerLen)
	}
	if err := json.Unmarshal(msg[4:4+headerLen], v); err != nil {
		return nil, err
	}
	return msg[4+headerLen:], nil
}

// Errno names rather than numbers go over the wire: numbers differ between
// operating systems (ENOTEMPTY is 39 on Linux, 66 on macOS).
func mountErrnos() map[string]syscall.Errno {
	return map[string]syscall.Errno{
		"ENOENT":       syscall.ENOENT,
		"EEXIST":       syscall.EEXIST,
		"EACCES":       syscall.EACCES,
		"EPERM":        syscall.EPERM,
		"ENOTDIR":      syscall.ENOTDIR,
		"EISDIR":       syscall.EISDIR,
		"ENOTEMPTY":    syscall.ENOTEMPTY,
		"EINVAL":       syscall.EINVAL,
		"EXDEV":        syscall.EXDEV,
		"ENOSPC":       syscall.ENOSPC,
		"EROFS":        syscall.EROFS,
		"ENAMETOOLONG": syscall.ENAMETOOLONG,
		"ELOOP":        syscall.ELOOP,
		"EFBIG":        syscall.EFBIG,
		"EBUSY":        syscall.EBUSY,
		"ENOTSUP":      syscall.ENOTSUP,
		"EBADF":        syscall.EBADF,
		"EMFILE":       syscall.EMFILE,
	}
}

// MountErrnoName maps err to the name MountResponse.Errno carries, falling back
// to EIO for anything without a portable equivalent.
func MountErrnoName(err error) string {
	var errno syscall.Errno
	if errors.As(err, &errno) {
		for name, e := range mountErrnos() {
			if e == errno {
				return name
			}
		}
	}
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return "ENOENT"
	case errors.Is(err, fs.ErrExist):
		return "EEXIST"
	case errors.Is(err, fs.ErrPermission):
		return "EACCES"
	case errors.Is(err, fs.ErrInvalid):
		return "EINVAL"
	}
	return "EIO"
}

// MountErrno is the inverse of MountErrnoName.
func MountErrno(name string) syscall.Errno {
	if errno, ok := mountErrnos()[name]; ok {
		return errno
	}
	return syscall.EIO
}
