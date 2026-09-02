package info

import (
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"howett.net/plist"
)

// appBundleDirs are where macOS keeps installed .app bundles - the closest equivalent to
// Linux's .desktop files.
func appBundleDirs() []string {
	dirs := []string{"/Applications", "/System/Applications"}
	if home, err := os.UserHomeDir(); err == nil && home != "" {
		dirs = append(dirs, filepath.Join(home, "Applications"))
	}
	return dirs
}

// scanDesktopFiles walks the app bundle directories for .app bundles and resolves each to its
// executable via Contents/Info.plist, the macOS equivalent of a .desktop file's Exec field.
func scanDesktopFiles() map[string]desktopEntry {
	result := make(map[string]desktopEntry)
	for _, dir := range appBundleDirs() {
		_ = filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
			if err != nil || !d.IsDir() || !strings.HasSuffix(d.Name(), ".app") {
				return nil //nolint:nilerr
			}
			if entry, ok := parseAppBundle(path); ok {
				key := strings.ToLower(entry.ExecBase)
				if _, exists := result[key]; !exists {
					result[key] = entry
				}
			}
			return filepath.SkipDir // don't descend into the bundle's own Contents/
		})
	}

	return result
}

// parseAppBundle reads bundlePath's Info.plist (XML or binary, plist.Unmarshal handles both)
// to find its real executable name and display name.
func parseAppBundle(bundlePath string) (desktopEntry, bool) {
	data, err := os.ReadFile(filepath.Join(bundlePath, "Contents", "Info.plist"))
	if err != nil {
		return desktopEntry{}, false
	}

	var info struct {
		CFBundleExecutable  string `plist:"CFBundleExecutable"`
		CFBundleName        string `plist:"CFBundleName"`
		CFBundleDisplayName string `plist:"CFBundleDisplayName"`
		CFBundleIconFile    string `plist:"CFBundleIconFile"`
	}
	if _, err := plist.Unmarshal(data, &info); err != nil || info.CFBundleExecutable == "" {
		return desktopEntry{}, false
	}

	name := info.CFBundleDisplayName
	if name == "" {
		name = info.CFBundleName
	}
	if name == "" {
		name = strings.TrimSuffix(filepath.Base(bundlePath), ".app")
	}

	var iconPath string
	if info.CFBundleIconFile != "" {
		iconName := info.CFBundleIconFile
		if !strings.HasSuffix(iconName, ".icns") {
			iconName += ".icns"
		}
		iconPath = filepath.Join(bundlePath, "Contents", "Resources", iconName)
	}

	id := strings.TrimSuffix(filepath.Base(bundlePath), ".app")
	return desktopEntry{ID: id, Name: name, IconName: iconPath, ExecBase: info.CFBundleExecutable}, true
}
