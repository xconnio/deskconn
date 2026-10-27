//go:build !linux

package main

import "errors"

// file mount is FUSE, which is Linux-only here; see mount.go.
func mountFiles(_, _, _, _ string, _, _ bool) error {
	return errors.New("file mount is only supported on Linux")
}
