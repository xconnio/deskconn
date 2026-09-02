package info

import (
	"bufio"
	"os"
	"path/filepath"
	"strings"
)

// desktopDirs and scanDesktopFiles below scan Linux's .desktop files.
func desktopDirs() []string {
	dirs := []string{
		"/usr/share/applications",
		"/var/lib/snapd/desktop/applications",
		"/var/lib/flatpak/exports/share/applications",
	}

	if home, err := os.UserHomeDir(); err == nil && home != "" {
		dirs = append(dirs,
			filepath.Join(home, ".local/share/applications"),
			filepath.Join(home, ".local/share/flatpak/exports/share/applications"),
		)
	}

	return dirs
}

func scanDesktopFiles() map[string]desktopEntry {
	result := make(map[string]desktopEntry)
	for _, dir := range desktopDirs() {
		entries, err := os.ReadDir(dir)
		if err != nil {
			continue
		}
		for _, e := range entries {
			if e.IsDir() || !strings.HasSuffix(e.Name(), ".desktop") {
				continue
			}
			entry, ok := parseDesktopFile(filepath.Join(dir, e.Name()))
			if !ok {
				continue
			}
			key := strings.ToLower(entry.ExecBase)
			if _, exists := result[key]; !exists {
				result[key] = entry
			}
		}
	}

	return result
}

func parseDesktopFile(path string) (desktopEntry, bool) {
	f, err := os.Open(path)
	if err != nil {
		return desktopEntry{}, false
	}
	defer f.Close() //nolint:errcheck

	var name, icon, execLine string
	var noDisplay, hidden, inDesktopEntry bool

	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if strings.HasPrefix(line, "[") {
			inDesktopEntry = line == "[Desktop Entry]"
			continue
		}
		if !inDesktopEntry || line == "" || strings.HasPrefix(line, "#") {
			continue
		}

		key, value, found := strings.Cut(line, "=")
		if !found {
			continue
		}
		key = strings.TrimSpace(key)
		value = strings.TrimSpace(value)

		switch key {
		case "Name":
			name = value
		case "Icon":
			icon = value
		case "Exec":
			execLine = value
		case "NoDisplay":
			noDisplay = value == "true"
		case "Hidden":
			hidden = value == "true"
		}
	}

	if noDisplay || hidden || execLine == "" || name == "" {
		return desktopEntry{}, false
	}

	execBase := execBaseName(execLine)
	if execBase == "" {
		return desktopEntry{}, false
	}

	id := strings.TrimSuffix(filepath.Base(path), ".desktop")
	return desktopEntry{ID: id, Name: name, IconName: icon, ExecBase: execBase}, true
}

// execBaseName skips Exec field-code placeholders like %f/%U.
func execBaseName(execLine string) string {
	for _, field := range strings.Fields(execLine) {
		if strings.HasPrefix(field, "%") {
			continue
		}
		return filepath.Base(field)
	}
	return ""
}
