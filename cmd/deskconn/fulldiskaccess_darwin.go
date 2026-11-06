package main

import (
	"os"
	"path/filepath"

	log "github.com/sirupsen/logrus"
)

// fullDiskAccessFile is where userd records, in its config directory, whether it has Full
// Disk Access, for install.sh to read. Only userd itself can tell: macOS decides it for the
// process asking, so a check run from the installer would test the terminal's access instead.
const fullDiskAccessFile = "full-disk-access"

// recordFullDiskAccess checks whether userd has Full Disk Access and writes "granted" or
// "missing" to fullDiskAccessFile. Without it, macOS asks separately the first time userd
// reads each protected place (Desktop, Documents, Downloads, external and network volumes,
// other apps' data), and there's no way to ask for those up front. The user's TCC database
// is only readable with Full Disk Access, and opening it never prompts.
func recordFullDiskAccess(cfgDirectory string) {
	status := "granted"
	home, err := os.UserHomeDir()
	if err != nil {
		return
	}
	f, err := os.Open(filepath.Join(home, "Library", "Application Support", "com.apple.TCC", "TCC.db"))
	if err != nil {
		status = "missing"
		log.Warn("no Full Disk Access: macOS will ask before each protected folder is read. Switch on " +
			"deskconn in System Settings > Privacy & Security > Full Disk Access to allow them all at once")
	} else {
		_ = f.Close()
	}
	_ = os.WriteFile(filepath.Join(cfgDirectory, fullDiskAccessFile), []byte(status+"\n"), 0600)
}
