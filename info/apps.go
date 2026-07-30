package info

import (
	"bufio"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

const systemAppID = "system"

type AppInfo struct {
	ID         string  `json:"id"`
	Name       string  `json:"name"`
	IconName   string  `json:"icon_name"`
	PIDs       []int32 `json:"pids"`
	CPUPercent float64 `json:"cpu_percent"`
	MemRSS     uint64  `json:"mem_rss"`
}

type desktopEntry struct {
	ID       string
	Name     string
	IconName string
	ExecBase string
}

// AppRegistry caches parsed .desktop files, keyed by Exec binary basename, to
// avoid rescanning the applications directories on every poll.
type AppRegistry struct {
	mu        sync.Mutex
	byExec    map[string]desktopEntry
	scannedAt time.Time
}

const appRegistryTTL = 60 * time.Second

func NewAppRegistry() *AppRegistry {
	return &AppRegistry{}
}

func (r *AppRegistry) entries() map[string]desktopEntry {
	r.mu.Lock()
	defer r.mu.Unlock()

	if r.byExec == nil || time.Since(r.scannedAt) > appRegistryTTL {
		r.byExec = scanDesktopFiles()
		r.scannedAt = time.Now()
	}

	return r.byExec
}

// Build groups a process snapshot into installed apps using the registry.
func (r *AppRegistry) Build(procs []ProcessInfo) []AppInfo {
	return buildApps(procs, r.entries())
}

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

// buildApps groups processes by executable basename; unmatched ones fall
// into a synthetic "System Processes" app.
func buildApps(procs []ProcessInfo, registry map[string]desktopEntry) []AppInfo {
	buckets := make(map[string]*AppInfo)

	getBucket := func(id, name, icon string) *AppInfo {
		b, ok := buckets[id]
		if !ok {
			b = &AppInfo{ID: id, Name: name, IconName: icon}
			buckets[id] = b
		}
		return b
	}

	for _, p := range procs {
		execBase := filepath.Base(p.Exe)

		var b *AppInfo
		if entry, ok := registry[strings.ToLower(execBase)]; execBase != "" && execBase != "." && ok {
			b = getBucket(entry.ID, entry.Name, entry.IconName)
		} else if entry, ok := registry[strings.ToLower(p.Name)]; ok {
			b = getBucket(entry.ID, entry.Name, entry.IconName)
		} else {
			b = getBucket(systemAppID, "System Processes", "applications-system")
		}

		b.PIDs = append(b.PIDs, p.PID)
		b.CPUPercent += p.CPUPercent
		b.MemRSS += p.MemRSS
	}

	apps := make([]AppInfo, 0, len(buckets))
	for _, b := range buckets {
		apps = append(apps, *b)
	}
	sort.Slice(apps, func(i, j int) bool {
		iSys, jSys := apps[i].ID == systemAppID, apps[j].ID == systemAppID
		if iSys != jSys {
			return jSys
		}
		return strings.ToLower(apps[i].Name) < strings.ToLower(apps[j].Name)
	})

	return apps
}
