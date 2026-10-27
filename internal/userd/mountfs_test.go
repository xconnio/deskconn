//go:build linux

package userd_test

import (
	"bytes"
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/xconnio/deskconn/internal/common"
	"github.com/xconnio/deskconn/internal/deskconn"
	"github.com/xconnio/deskconn/internal/userd"
)

// mountHome mounts a fresh temporary home directory, served by the machine
// side's handler in-process, and returns (home, mountpoint, open files on the
// machine).
func mountHome(t *testing.T, opts deskconn.MountOptions) (string, string, func() int) {
	return mountHomeVia(t, opts, nil)
}

// mountHomeVia is mountHome with every request going through via, if not nil,
// which can delay or fail it instead of passing it to the machine.
func mountHomeVia(t *testing.T, opts deskconn.MountOptions,
	via func(req *common.MountRequest, machine func() ([]byte, error)) ([]byte, error)) (string, string, func() int) {
	t.Helper()
	if _, err := os.Stat("/dev/fuse"); err != nil {
		t.Skip("FUSE not available")
	}
	if _, err := exec.LookPath("fusermount3"); err != nil {
		if _, err := exec.LookPath("fusermount"); err != nil {
			t.Skip("fusermount not available")
		}
	}

	home := t.TempDir()
	t.Setenv("HOME", home)
	mnt := t.TempDir()

	handle, openFiles := userd.NewMountFS()
	call := func(_ context.Context, payload []byte) ([]byte, error) {
		if via == nil {
			return handle(payload)
		}
		var req common.MountRequest
		_, err := common.DecodeMountMessage(payload, &req)
		require.NoError(t, err)
		return via(&req, func() ([]byte, error) { return handle(payload) })
	}
	server, err := deskconn.MountServer(call, "", mnt, "test:", opts)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, server.Unmount()) })
	return home, mnt, openFiles
}

func TestMountReadWrite(t *testing.T) {
	home, mnt, openFiles := mountHome(t, deskconn.MountOptions{})

	t.Run("write through mount lands on the machine", func(t *testing.T) {
		require.NoError(t, os.WriteFile(filepath.Join(mnt, "a.txt"), []byte("hello"), 0o600))
		data, err := os.ReadFile(filepath.Join(home, "a.txt"))
		require.NoError(t, err)
		require.Equal(t, "hello", string(data))

		info, err := os.Stat(filepath.Join(mnt, "a.txt"))
		require.NoError(t, err)
		require.Equal(t, int64(5), info.Size())
		require.Equal(t, os.FileMode(0o600), info.Mode().Perm())
	})

	t.Run("append and truncate", func(t *testing.T) {
		f, err := os.OpenFile(filepath.Join(mnt, "a.txt"), os.O_WRONLY|os.O_APPEND, 0)
		require.NoError(t, err)
		_, err = f.WriteString(" world")
		require.NoError(t, err)
		require.NoError(t, f.Close())
		data, err := os.ReadFile(filepath.Join(home, "a.txt"))
		require.NoError(t, err)
		require.Equal(t, "hello world", string(data))

		require.NoError(t, os.Truncate(filepath.Join(mnt, "a.txt"), 4))
		data, err = os.ReadFile(filepath.Join(mnt, "a.txt"))
		require.NoError(t, err)
		require.Equal(t, "hell", string(data))

		// O_TRUNC on an existing file.
		require.NoError(t, os.WriteFile(filepath.Join(mnt, "a.txt"), []byte("x"), 0o600))
		data, err = os.ReadFile(filepath.Join(home, "a.txt"))
		require.NoError(t, err)
		require.Equal(t, "x", string(data))
	})

	t.Run("large file spanning several requests", func(t *testing.T) {
		content := make([]byte, 3*1024*1024+123)
		_, err := rand.Read(content)
		require.NoError(t, err)
		require.NoError(t, os.WriteFile(filepath.Join(mnt, "big.bin"), content, 0o644))

		onDevice, err := os.ReadFile(filepath.Join(home, "big.bin"))
		require.NoError(t, err)
		require.True(t, bytes.Equal(content, onDevice))

		throughMount, err := os.ReadFile(filepath.Join(mnt, "big.bin"))
		require.NoError(t, err)
		require.True(t, bytes.Equal(content, throughMount))
	})

	t.Run("read-ahead returns the machine's bytes", func(t *testing.T) {
		content := make([]byte, 5*1024*1024+77)
		_, err := rand.Read(content)
		require.NoError(t, err)
		require.NoError(t, os.WriteFile(filepath.Join(home, "ra.bin"), content, 0o644))

		data, err := os.ReadFile(filepath.Join(mnt, "ra.bin"))
		require.NoError(t, err)
		require.True(t, bytes.Equal(content, data))

		f, err := os.Open(filepath.Join(mnt, "ra.bin"))
		require.NoError(t, err)
		defer f.Close()
		for _, off := range []int64{4 << 20, 123, 1<<20 - 5, 5 << 20, 2<<20 + 99, 0} {
			buf := make([]byte, 300*1024)
			n, err := f.ReadAt(buf, off)
			if err != nil {
				require.ErrorIs(t, err, io.EOF)
			}
			require.True(t, bytes.Equal(content[off:off+int64(n)], buf[:n]), "offset %d", off)
		}
	})

	t.Run("read after write through the same file", func(t *testing.T) {
		require.NoError(t, os.WriteFile(filepath.Join(home, "rw.bin"), bytes.Repeat([]byte("a"), 3<<20), 0o644))
		f, err := os.OpenFile(filepath.Join(mnt, "rw.bin"), os.O_RDWR, 0)
		require.NoError(t, err)
		defer f.Close()

		buf := make([]byte, 4)
		_, err = f.ReadAt(buf, 1<<20)
		require.NoError(t, err)
		require.Equal(t, "aaaa", string(buf))

		_, err = f.WriteAt([]byte("bbbb"), 1<<20)
		require.NoError(t, err)
		_, err = f.ReadAt(buf, 1<<20)
		require.NoError(t, err)
		require.Equal(t, "bbbb", string(buf))
	})

	t.Run("directories", func(t *testing.T) {
		require.NoError(t, os.MkdirAll(filepath.Join(mnt, "d1", "d2"), 0o755))
		require.NoError(t, os.WriteFile(filepath.Join(mnt, "d1", "d2", "f"), []byte("f"), 0o644))
		require.DirExists(t, filepath.Join(home, "d1", "d2"))

		entries, err := os.ReadDir(mnt)
		require.NoError(t, err)
		var names []string
		for _, e := range entries {
			names = append(names, e.Name())
		}
		sort.Strings(names)
		require.Equal(t, []string{"a.txt", "big.bin", "d1", "ra.bin", "rw.bin"}, names)

		err = syscall.Rmdir(filepath.Join(mnt, "d1"))
		require.ErrorIs(t, err, syscall.ENOTEMPTY)
		err = syscall.Unlink(filepath.Join(mnt, "d1"))
		require.ErrorIs(t, err, syscall.EISDIR)

		require.NoError(t, os.RemoveAll(filepath.Join(mnt, "d1")))
		require.NoDirExists(t, filepath.Join(home, "d1"))
	})

	t.Run("rename", func(t *testing.T) {
		require.NoError(t, os.Rename(filepath.Join(mnt, "a.txt"), filepath.Join(mnt, "b.txt")))
		require.NoFileExists(t, filepath.Join(home, "a.txt"))
		require.FileExists(t, filepath.Join(home, "b.txt"))

		// Replacing an existing file, as editors' save-via-temp-file does.
		require.NoError(t, os.WriteFile(filepath.Join(mnt, "b.txt.tmp"), []byte("new"), 0o644))
		require.NoError(t, os.Rename(filepath.Join(mnt, "b.txt.tmp"), filepath.Join(mnt, "b.txt")))
		data, err := os.ReadFile(filepath.Join(mnt, "b.txt"))
		require.NoError(t, err)
		require.Equal(t, "new", string(data))
	})

	t.Run("exclusive create", func(t *testing.T) {
		_, err := os.OpenFile(filepath.Join(mnt, "b.txt"), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o644)
		require.True(t, errors.Is(err, os.ErrExist), err)
	})

	t.Run("write through descriptor of file created read-only", func(t *testing.T) {
		// git creates its objects like this.
		f, err := os.OpenFile(filepath.Join(mnt, "ro.txt"), os.O_CREATE|os.O_EXCL|os.O_RDWR, 0o444)
		require.NoError(t, err)
		_, err = f.WriteString("object")
		require.NoError(t, err)
		require.NoError(t, f.Close())

		data, err := os.ReadFile(filepath.Join(home, "ro.txt"))
		require.NoError(t, err)
		require.Equal(t, "object", string(data))
		info, err := os.Stat(filepath.Join(home, "ro.txt"))
		require.NoError(t, err)
		require.Equal(t, os.FileMode(0o444), info.Mode().Perm())

		_, err = os.OpenFile(filepath.Join(mnt, "ro.txt"), os.O_WRONLY, 0)
		require.ErrorIs(t, err, os.ErrPermission)
	})

	t.Run("symlinks", func(t *testing.T) {
		require.NoError(t, os.Symlink("b.txt", filepath.Join(mnt, "link")))
		target, err := os.Readlink(filepath.Join(home, "link"))
		require.NoError(t, err)
		require.Equal(t, "b.txt", target)

		target, err = os.Readlink(filepath.Join(mnt, "link"))
		require.NoError(t, err)
		require.Equal(t, "b.txt", target)

		data, err := os.ReadFile(filepath.Join(mnt, "link"))
		require.NoError(t, err)
		require.Equal(t, "new", string(data))
	})

	t.Run("chmod", func(t *testing.T) {
		require.NoError(t, os.Chmod(filepath.Join(mnt, "b.txt"), 0o600))
		info, err := os.Stat(filepath.Join(home, "b.txt"))
		require.NoError(t, err)
		require.Equal(t, os.FileMode(0o600), info.Mode().Perm())
	})

	t.Run("changes made on the machine show up", func(t *testing.T) {
		require.NoError(t, os.WriteFile(filepath.Join(home, "frommachine"), []byte("dev"), 0o644))
		data, err := os.ReadFile(filepath.Join(mnt, "frommachine"))
		require.NoError(t, err)
		require.Equal(t, "dev", string(data))
	})

	t.Run("inode numbers stay the same", func(t *testing.T) {
		// tar, git and find compare them across lookups the kernel has stopped caching.
		require.NoError(t, os.WriteFile(filepath.Join(home, "ino.txt"), nil, 0o644))
		inode := func() uint64 {
			var st syscall.Stat_t
			require.NoError(t, syscall.Stat(filepath.Join(mnt, "ino.txt"), &st))
			return st.Ino
		}
		before := inode()
		time.Sleep(1500 * time.Millisecond)
		require.Equal(t, before, inode())
	})

	t.Run("closed files are released on the machine", func(t *testing.T) {
		// Release is asynchronous to close(2).
		require.Eventually(t, func() bool { return openFiles() == 0 }, 5*time.Second, 10*time.Millisecond)
	})

	t.Run("statfs", func(t *testing.T) {
		var st syscall.Statfs_t
		require.NoError(t, syscall.Statfs(mnt, &st))
		require.NotZero(t, st.Blocks)
	})
}

func TestMountReadOnly(t *testing.T) {
	home, mnt, _ := mountHome(t, deskconn.MountOptions{ReadOnly: true})
	require.NoError(t, os.WriteFile(filepath.Join(home, "a.txt"), []byte("hello"), 0o644))

	data, err := os.ReadFile(filepath.Join(mnt, "a.txt"))
	require.NoError(t, err)
	require.Equal(t, "hello", string(data))

	err = os.WriteFile(filepath.Join(mnt, "a.txt"), []byte("x"), 0o644)
	require.ErrorIs(t, err, syscall.EROFS)
}

func TestMountRootOutsideHome(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	handle, _ := userd.NewMountFS()
	call := func(_ context.Context, payload []byte) ([]byte, error) { return handle(payload) }
	_, err := deskconn.MountServer(call, "/etc", t.TempDir(), "test:", deskconn.MountOptions{})
	require.ErrorIs(t, err, syscall.EACCES)
}

func TestMountWriteBehind(t *testing.T) {
	var failWrites atomic.Bool
	slowWrites := func(req *common.MountRequest, machine func() ([]byte, error)) ([]byte, error) {
		if req.Op != common.MountOpWrite {
			return machine()
		}
		time.Sleep(50 * time.Millisecond)
		if failWrites.Load() {
			return common.EncodeMountMessage(&common.MountResponse{Errno: "ENOSPC"}, nil)
		}
		return machine()
	}
	home, mnt, _ := mountHomeVia(t, deskconn.MountOptions{}, slowWrites)

	t.Run("stat sees writes still in flight", func(t *testing.T) {
		f, err := os.Create(filepath.Join(mnt, "pending.bin"))
		require.NoError(t, err)
		defer f.Close()
		_, err = f.Write(make([]byte, 3<<20))
		require.NoError(t, err)

		info, err := os.Stat(filepath.Join(mnt, "pending.bin"))
		require.NoError(t, err)
		require.Equal(t, int64(3<<20), info.Size())
		require.NoError(t, f.Sync())
	})

	t.Run("overlapping writes land in order", func(t *testing.T) {
		f, err := os.Create(filepath.Join(mnt, "order.txt"))
		require.NoError(t, err)
		for _, s := range []string{"aaaa", "bbbb", "cccc"} {
			_, err = f.WriteAt([]byte(s), 0)
			require.NoError(t, err)
		}
		require.NoError(t, f.Close())
		data, err := os.ReadFile(filepath.Join(home, "order.txt"))
		require.NoError(t, err)
		require.Equal(t, "cccc", string(data))
	})

	t.Run("a failed write is reported by close", func(t *testing.T) {
		f, err := os.Create(filepath.Join(mnt, "full.bin"))
		require.NoError(t, err)
		failWrites.Store(true)
		defer failWrites.Store(false)
		_, err = f.Write([]byte("data"))
		require.NoError(t, err, "acknowledged before the machine fails it")
		require.ErrorIs(t, f.Close(), syscall.ENOSPC)
	})
}

func TestMountRelistsInsteadOfStatingEachEntry(t *testing.T) {
	var stats, listings atomic.Int32
	count := func(req *common.MountRequest, machine func() ([]byte, error)) ([]byte, error) {
		switch req.Op {
		case common.MountOpStat:
			stats.Add(1)
		case common.MountOpReadDir:
			listings.Add(1)
		}
		return machine()
	}
	home, mnt, _ := mountHomeVia(t, deskconn.MountOptions{}, count)
	dir := filepath.Join(home, "many")
	require.NoError(t, os.Mkdir(dir, 0o755))
	for i := range 50 {
		require.NoError(t, os.WriteFile(filepath.Join(dir, fmt.Sprintf("f%d", i)), nil, 0o644))
	}

	entries, err := os.ReadDir(filepath.Join(mnt, "many"))
	require.NoError(t, err)
	require.Len(t, entries, 50)

	// Let the listing's attributes, and the kernel's, expire, as they do over a
	// slow link part way through an ls -l.
	time.Sleep(1200 * time.Millisecond)
	stats.Store(0)
	listings.Store(0)
	for _, entry := range entries {
		_, err := os.Lstat(filepath.Join(mnt, "many", entry.Name()))
		require.NoError(t, err)
	}
	require.LessOrEqual(t, stats.Load()+listings.Load(), int32(5),
		"stats: %d, listings: %d", stats.Load(), listings.Load())

	// New names are looked up on their own, without listing the directory.
	listings.Store(0)
	require.NoError(t, os.WriteFile(filepath.Join(mnt, "many", "new"), nil, 0o644))
	require.Zero(t, listings.Load())
}

func TestMountAlreadyMounted(t *testing.T) {
	_, mnt, _ := mountHome(t, deskconn.MountOptions{})

	handle, _ := userd.NewMountFS()
	call := func(_ context.Context, payload []byte) ([]byte, error) { return handle(payload) }
	_, err := deskconn.MountServer(call, "", mnt, "test:", deskconn.MountOptions{})
	require.ErrorContains(t, err, "already mounted")
}

// TestMountStaysInHome sends requests the way a malicious client could, the
// kernel never following a symlink on the machine's side.
func TestMountStaysInHome(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	outside := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(outside, "secret"), []byte("secret"), 0o600))

	handle, _ := userd.NewMountFS()
	do := func(req *common.MountRequest) *common.MountResponse {
		payload, err := common.EncodeMountMessage(req, req.Data)
		require.NoError(t, err)
		result, err := handle(payload)
		require.NoError(t, err)
		var resp common.MountResponse
		data, err := common.DecodeMountMessage(result, &resp)
		require.NoError(t, err)
		resp.Data = data
		return &resp
	}

	require.Empty(t, do(&common.MountRequest{Op: common.MountOpSymlink, Path: "esc", Target: outside}).Errno,
		"a symlink may point anywhere")
	const secret = "esc/secret"

	resp := do(&common.MountRequest{Op: common.MountOpRead, Path: secret, Length: 100})
	require.NotEmpty(t, resp.Errno)
	require.Empty(t, resp.Data)

	resp = do(&common.MountRequest{Op: common.MountOpOpen, Path: secret, Access: common.MountAccessRead})
	require.NotEmpty(t, resp.Errno)

	resp = do(&common.MountRequest{
		Op: common.MountOpOpen, Path: "esc/new", Access: common.MountAccessWrite, Create: true, Mode: 0o644,
	})
	require.NotEmpty(t, resp.Errno)
	require.NoFileExists(t, filepath.Join(outside, "new"))

	require.NotEmpty(t, do(&common.MountRequest{Op: common.MountOpUnlink, Path: secret}).Errno)
	require.NotEmpty(t, do(&common.MountRequest{Op: common.MountOpRename, Path: secret, NewPath: "got"}).Errno)
	require.FileExists(t, filepath.Join(outside, "secret"))

	require.Equal(t, "EACCES", do(&common.MountRequest{Op: common.MountOpStat, Path: "../"}).Errno)
}

func TestMountSymlinkedRoot(t *testing.T) {
	if _, err := os.Stat("/dev/fuse"); err != nil {
		t.Skip("FUSE not available")
	}
	home := t.TempDir()
	t.Setenv("HOME", home)
	require.NoError(t, os.MkdirAll(filepath.Join(home, "work", "proj"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(home, "work", "proj", "f"), []byte("f"), 0o644))
	require.NoError(t, os.Symlink(filepath.Join("work", "proj"), filepath.Join(home, "proj")))

	handle, _ := userd.NewMountFS()
	call := func(_ context.Context, payload []byte) ([]byte, error) { return handle(payload) }
	mnt := t.TempDir()
	server, err := deskconn.MountServer(call, "proj", mnt, "test:", deskconn.MountOptions{})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, server.Unmount()) })

	info, err := os.Stat(mnt)
	require.NoError(t, err)
	require.True(t, info.IsDir())
	data, err := os.ReadFile(filepath.Join(mnt, "f"))
	require.NoError(t, err)
	require.Equal(t, "f", string(data))
}
