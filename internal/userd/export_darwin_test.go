package userd

// ProcessSnapshot is a processSnapshot with its fields exported.
type ProcessSnapshot struct {
	Info    ProcessInfo
	CPUTime float64
}

func exportSnapshots(snapshots []processSnapshot) []ProcessSnapshot {
	out := make([]ProcessSnapshot, len(snapshots))
	for i, s := range snapshots {
		out[i] = ProcessSnapshot{Info: s.info, CPUTime: s.cpuTime}
	}
	return out
}

func ParsePSStats(stats string, cmdlines map[string]string, memTotal uint64) []ProcessSnapshot {
	return exportSnapshots(parsePSStats(stats, cmdlines, memTotal))
}

func ListProcesses() ([]ProcessSnapshot, error) {
	snapshots, err := listProcesses()
	return exportSnapshots(snapshots), err
}
