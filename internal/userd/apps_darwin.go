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
			if !strings.HasSuffix(e.Name(), ".app") {
				continue
			}
			bundle := filepath.Join(dir, e.Name())
			if info, err := os.Stat(bundle); err != nil || !info.IsDir() {
				continue
			}
			name := strings.TrimSuffix(e.Name(), ".app")
			entry := desktopEntry{ID: name, Name: name}
			addBundle(result, bundle, entry)
			if resolved, err := filepath.EvalSymlinks(bundle); err == nil && resolved != bundle {
				addBundle(result, resolved, entry)
			}
		}
	}

	return result
}

func addBundle(result map[string]desktopEntry, bundle string, entry desktopEntry) {
	key := bundle + "/"
	if _, exists := result[key]; !exists {
		result[key] = entry
	}
}
