package info

import (
	"fmt"
	"mime"
	"os"
	"path/filepath"
	"sync"
)

// iconPathCache maps an icon name to its resolved path, avoiding repeated
// filesystem globbing for the same icon across polls.
var iconPathCache sync.Map //nolint:gochecknoglobals

var iconThemePriority = []string{"hicolor", "Yaru", "Adwaita"} //nolint:gochecknoglobals

var iconSizePriority = []string{ //nolint:gochecknoglobals
	"256x256", "128x128", "96x96", "64x64", "48x48", "32x32", "scalable",
}

var iconExtPriority = []string{".png", ".svg", ".xpm"} //nolint:gochecknoglobals

func iconSearchDirs() []string {
	dirs := []string{"/usr/share/icons", "/usr/share/pixmaps"}
	if home, err := os.UserHomeDir(); err == nil && home != "" {
		dirs = append([]string{
			filepath.Join(home, ".local/share/icons"),
			filepath.Join(home, ".icons"),
		}, dirs...)
	}
	return dirs
}

// ResolveIconName resolves an icon name (as found in a .desktop file's Icon=
// key) to an absolute file path. Absolute paths are used as-is; bare names
// are searched across the standard icon theme directories, preferring larger
// PNGs and falling back to SVG/XPM.
func ResolveIconName(name string) (string, bool) {
	if name == "" {
		return "", false
	}
	if filepath.IsAbs(name) {
		if _, err := os.Stat(name); err == nil {
			return name, true
		}
		return "", false
	}

	if cached, ok := iconPathCache.Load(name); ok {
		path, _ := cached.(string)
		return path, path != ""
	}

	path := searchIcon(name)
	iconPathCache.Store(name, path)
	return path, path != ""
}

func searchIcon(name string) string {
	for _, base := range iconSearchDirs() {
		for _, theme := range prioritizedThemeDirs(base) {
			for _, size := range iconSizePriority {
				for _, category := range []string{"apps", "categories", "mimetypes", ""} {
					dir := theme
					if size != "" {
						dir = filepath.Join(dir, size)
					}
					if category != "" {
						dir = filepath.Join(dir, category)
					}
					for _, ext := range iconExtPriority {
						candidate := filepath.Join(dir, name+ext)
						if _, err := os.Stat(candidate); err == nil {
							return candidate
						}
					}
				}
			}
		}
		// Flat directories with no theme/size subdirectories (e.g. /usr/share/pixmaps).
		for _, ext := range iconExtPriority {
			candidate := filepath.Join(base, name+ext)
			if _, err := os.Stat(candidate); err == nil {
				return candidate
			}
		}
	}
	return ""
}

func prioritizedThemeDirs(base string) []string {
	seen := make(map[string]bool)
	var dirs []string

	addIfDir := func(dir string) {
		if seen[dir] {
			return
		}
		if info, err := os.Stat(dir); err == nil && info.IsDir() {
			dirs = append(dirs, dir)
			seen[dir] = true
		}
	}

	for _, theme := range iconThemePriority {
		addIfDir(filepath.Join(base, theme))
	}

	entries, err := os.ReadDir(base)
	if err != nil {
		return dirs
	}
	for _, e := range entries {
		if e.IsDir() {
			addIfDir(filepath.Join(base, e.Name()))
		}
	}

	return dirs
}

// ReadIcon resolves name to a file and returns its MIME type and contents.
func ReadIcon(name string) (string, []byte, error) {
	path, ok := ResolveIconName(name)
	if !ok {
		return "", nil, fmt.Errorf("icon %q not found", name)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		return "", nil, err
	}

	mimeType := mime.TypeByExtension(filepath.Ext(path))
	if mimeType == "" {
		if filepath.Ext(path) == ".svg" {
			mimeType = "image/svg+xml"
		} else {
			mimeType = "application/octet-stream"
		}
	}

	return mimeType, data, nil
}
