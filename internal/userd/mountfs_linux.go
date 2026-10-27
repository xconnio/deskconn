package userd

import (
	"os"
	"syscall"

	"github.com/xconnio/deskconn/internal/common"
)

// fsPlatformAttr fills attr from the stat information behind info, which also has
// the file type bits, link count and access/change times os.FileInfo doesn't.
func fsPlatformAttr(info os.FileInfo, attr *common.MountAttr) bool {
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return false
	}
	attr.Mode = st.Mode
	attr.Nlink = uint32(st.Nlink) // nolint: gosec
	attr.Atime = st.Atim.Nano()
	attr.Ctime = st.Ctim.Nano()
	attr.Dev = uint64(st.Dev) // nolint: unconvert,gosec
	attr.Ino = st.Ino
	return true
}

func fsStatfs(path string) (*common.MountStatfs, error) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(path, &st); err != nil {
		return nil, err
	}
	return &common.MountStatfs{
		Blocks:  st.Blocks,
		Bfree:   st.Bfree,
		Bavail:  st.Bavail,
		Files:   st.Files,
		Ffree:   st.Ffree,
		Bsize:   uint32(st.Bsize),   // nolint: gosec
		NameLen: uint32(st.Namelen), // nolint: gosec
	}, nil
}
