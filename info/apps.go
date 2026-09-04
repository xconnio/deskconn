package info

import (
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
