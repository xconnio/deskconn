package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	log "github.com/sirupsen/logrus"

	"github.com/xconnio/deskconn/internal/common"
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

func fullDiskAccessPath() (string, error) {
	cfgDirectory, err := common.CfgDirectory()
	if err != nil {
		return "", err
	}

	return filepath.Join(cfgDirectory, fullDiskAccessFile), nil
}

func forgetFullDiskAccess() {
	if path, err := fullDiskAccessPath(); err == nil {
		_ = os.Remove(path)
	}
}

func reportFullDiskAccess(homeDir string) {
	path, err := fullDiskAccessPath()
	if err != nil {
		return
	}

	for range 20 {
		data, err := os.ReadFile(path)
		if err == nil && len(data) > 0 {
			if strings.TrimSpace(string(data)) == "missing" {
				_, execDir := installDirs(homeDir)
				fmt.Println("deskconn doesn't have Full Disk Access. macOS ties it to the exact binary, so after an")
				fmt.Println("update it has to be added again: in System Settings > Privacy & Security > Full Disk Access,")
				fmt.Println("remove any old deskconn entry and add:")
				fmt.Printf("  %s\n", filepath.Join(execDir, "deskconn"))
				fmt.Printf("Then restart it with:  launchctl kickstart -k %s\n", launchdTarget())
			}
			return
		}
		time.Sleep(500 * time.Millisecond)
	}
}
