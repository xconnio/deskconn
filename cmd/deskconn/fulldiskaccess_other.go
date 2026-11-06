//go:build !darwin

package main

// recordFullDiskAccess is only needed on macOS; see fulldiskaccess_darwin.go.
func recordFullDiskAccess(string) {}

func forgetFullDiskAccess() {}

func reportFullDiskAccess(string) {}
