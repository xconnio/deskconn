package info

import (
	"errors"
	"runtime"
	"sync"
	"syscall"
	"time"

	"github.com/shirou/gopsutil/cpu"
	"github.com/shirou/gopsutil/process"
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

// processSample lets CPU usage be computed as a delta against the previous poll.
type processSample struct {
	times *cpu.TimesStat
	at    time.Time
}

type ProcessMonitor struct {
	mu      sync.Mutex
	samples map[int32]processSample
}

func NewProcessMonitor() *ProcessMonitor {
	return &ProcessMonitor{samples: make(map[int32]processSample)}
}

func (pm *ProcessMonitor) List() ([]ProcessInfo, error) {
	procs, err := process.Processes()
	if err != nil {
		return nil, err
	}

	numCPU := runtime.NumCPU()
	now := time.Now()

	pm.mu.Lock()
	defer pm.mu.Unlock()

	seen := make(map[int32]bool, len(procs))
	infos := make([]ProcessInfo, 0, len(procs))
	for _, p := range procs {
		times, err := p.Times()
		if err != nil {
			// Process likely exited between listing and sampling.
			continue
		}

		name, _ := p.Name()
		exe, _ := p.Exe()
		username, _ := p.Username()
		status, _ := p.Status()
		cmdline, _ := p.Cmdline()
		ppid, _ := p.Ppid()
		memPercent, _ := p.MemoryPercent()

		var rss uint64
		if memInfo, err := p.MemoryInfo(); err == nil && memInfo != nil {
			rss = memInfo.RSS
		}

		var cpuPercent float64
		if prev, ok := pm.samples[p.Pid]; ok {
			if elapsed := now.Sub(prev.at).Seconds(); elapsed > 0 {
				delta := times.Total() - prev.times.Total()
				cpuPercent = delta / elapsed / float64(numCPU) * 100
				if cpuPercent < 0 {
					cpuPercent = 0
				}
			}
		}
		pm.samples[p.Pid] = processSample{times: times, at: now}
		seen[p.Pid] = true

		infos = append(infos, ProcessInfo{
			PID:        p.Pid,
			PPID:       ppid,
			Name:       name,
			Exe:        exe,
			User:       username,
			Status:     status,
			Cmdline:    cmdline,
			CPUPercent: cpuPercent,
			MemRSS:     rss,
			MemPercent: memPercent,
		})
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

// Signal sends sig ("term" or "kill") to each pid, collecting a per-pid
// result rather than failing the whole call on one permission error.
func Signal(pids []int32, sig string) ([]SignalResult, error) {
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
