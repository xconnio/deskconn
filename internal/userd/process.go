package userd

import (
	"errors"
	"runtime"
	"sync"
	"syscall"
	"time"

	"github.com/shirou/gopsutil/v4/process"
)

type ProcessInfo struct {
	PID        int32   `json:"pid"`
	PPID       int32   `json:"ppid"`
	Name       string  `json:"name"`
	Exe        string  `json:"exe"`
	User       string  `json:"user"`
	Status     string  `json:"status"`
	Cmdline    string  `json:"cmdline"`
	CPUPercent float64 `json:"cpu_percent"`
	MemRSS     uint64  `json:"mem_rss"`
	MemPercent float32 `json:"mem_percent"`
}

// processSnapshot is one process as an OS-specific listProcesses reads it, before its
// CPU usage is known.
type processSnapshot struct {
	info    ProcessInfo
	cpuTime float64 // seconds the process has run for
}

// processSample lets CPU usage be computed as a delta against the previous poll.
type processSample struct {
	cpuTime float64
	at      time.Time
}

type ProcessMonitor struct {
	mu      sync.Mutex
	samples map[int32]processSample
}

func NewProcessMonitor() *ProcessMonitor {
	return &ProcessMonitor{samples: make(map[int32]processSample)}
}

func (pm *ProcessMonitor) List() ([]ProcessInfo, error) {
	snapshots, err := listProcesses()
	if err != nil {
		return nil, err
	}

	numCPU := runtime.NumCPU()
	now := time.Now()

	pm.mu.Lock()
	defer pm.mu.Unlock()

	seen := make(map[int32]bool, len(snapshots))
	infos := make([]ProcessInfo, 0, len(snapshots))
	for _, s := range snapshots {
		info := s.info
		if prev, ok := pm.samples[info.PID]; ok {
			if elapsed := now.Sub(prev.at).Seconds(); elapsed > 0 {
				delta := s.cpuTime - prev.cpuTime
				info.CPUPercent = delta / elapsed / float64(numCPU) * 100
				if info.CPUPercent < 0 {
					info.CPUPercent = 0
				}
			}
		}
		pm.samples[info.PID] = processSample{cpuTime: s.cpuTime, at: now}
		seen[info.PID] = true

		infos = append(infos, info)
	}

	for pid := range pm.samples {
		if !seen[pid] {
			delete(pm.samples, pid)
		}
	}

	return infos, nil
}

type SignalResult struct {
	PID   int32  `json:"pid"`
	OK    bool   `json:"ok"`
	Error string `json:"error,omitempty"`
}

func signalProcess(pid int32, sig string) error {
	p, err := process.NewProcess(pid)
	if err != nil {
		return err
	}

	switch sig {
	case "term":
		return p.Terminate()
	case "kill":
		return p.Kill()
	default:
		return syscall.EINVAL
	}
}

// SignalProcesses sends sig ("term" or "kill") to each pid, collecting a per-pid
// result rather than failing the whole call on one permission error.
func SignalProcesses(pids []int32, sig string) ([]SignalResult, error) {
	if sig != "term" && sig != "kill" {
		return nil, errors.New(`signal must be "term" or "kill"`)
	}

	results := make([]SignalResult, 0, len(pids))
	for _, pid := range pids {
		result := SignalResult{PID: pid, OK: true}
		if err := signalProcess(pid, sig); err != nil {
			result.OK = false
			result.Error = err.Error()
		}
		results = append(results, result)
	}

	return results, nil
}
