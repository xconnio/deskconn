package userd

import (
	"os"
	"path/filepath"
	"strings"
)

// appDirs and scanDesktopFiles below scan macOS's .app bundles, the counterpart of Linux's
// .desktop files.
func appDirs() []string {
	dirs := []string{
		"/Applications",
		"/Applications/Utilities",
		"/System/Applications",
		"/System/Applications/Utilities",
	}

	if home, err := os.UserHomeDir(); err == nil && home != "" {
		dirs = append(dirs, filepath.Join(home, "Applications"))
	}

	return dirs
}

func scanDesktopFiles() map[string]desktopEntry {
	result := make(map[string]desktopEntry)
	for _, dir := range appDirs() {
		entries, err := os.ReadDir(dir)
		if err != nil {
			continue
		}
		for _, e := range entries {
			if !e.IsDir() || !strings.HasSuffix(e.Name(), ".app") {
				continue
			}
			name := strings.TrimSuffix(e.Name(), ".app")
			// A bundle's executables live in Contents/MacOS; the main one is
			// (CFBundleExecutable) almost always the only one there.
			binaries, err := os.ReadDir(filepath.Join(dir, e.Name(), "Contents", "MacOS"))
			if err != nil {
				continue
			}
			for _, b := range binaries {
				if b.IsDir() {
					continue
				}
				key := strings.ToLower(b.Name())
				if _, exists := result[key]; !exists {
					// IconName stays empty: .icns icons aren't something ResolveIconName serves.
					result[key] = desktopEntry{ID: name, Name: name, ExecBase: b.Name()}
				}
			}
		}
	}

	return result
}
