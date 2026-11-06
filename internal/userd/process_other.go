//go:build !darwin

package userd

import "github.com/shirou/gopsutil/v4/process"

func listProcesses() ([]processSnapshot, error) {
	procs, err := process.Processes()
	if err != nil {
		return nil, err
	}

	snapshots := make([]processSnapshot, 0, len(procs))
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

		snapshots = append(snapshots, processSnapshot{
			info: ProcessInfo{
				PID:        p.Pid,
				PPID:       ppid,
				Name:       name,
				Exe:        exe,
				User:       username,
				Status:     statusLetter(status),
				Cmdline:    cmdline,
				MemRSS:     rss,
				MemPercent: memPercent,
			},
			cpuTime: times.User + times.System + times.Iowait,
		})
	}

	return snapshots, nil
}

// statusLetter is the ps state letter clients get for what gopsutil reports.
func statusLetter(status []string) string {
	if len(status) == 0 {
		return ""
	}
	switch status[0] {
	case process.Running:
		return "R"
	case process.Blocked:
		return "D"
	case process.Idle:
		return "I"
	case process.Lock:
		return "L"
	case process.Sleep:
		return "S"
	case process.Stop:
		return "T"
	case process.Wait:
		return "W"
	case process.Zombie:
		return "Z"
	default:
		return ""
	}
}
