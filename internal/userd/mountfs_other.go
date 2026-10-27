//go:build !linux

package userd

import (
	"os"
	"syscall"

	"github.com/xconnio/deskconn/internal/common"
)

// Elsewhere (Windows) there's no stat information beyond what os.FileInfo
// carries; fsAttr derives the rest.
func fsPlatformAttr(os.FileInfo, *common.MountAttr) bool { return false }

func fsStatfs(string) (*common.MountStatfs, error) { return nil, syscall.ENOTSUP }
