package userd

import (
	"fmt"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"

	"github.com/shirou/gopsutil/v4/mem"
)

const psPath = "/bin/ps"

// psStatsLine is a line of listProcesses' first ps call. The command path comes last
// because it can have spaces.
var psStatsLine = regexp.MustCompile( //nolint:gochecknoglobals
	`^\s*(\d+)\s+(\d+)\s+(\S+)\s+(\d+)\s+(\S+)\s+(\S+)\s+(.*)$`)

// psCommandLine is a line of listProcesses' second ps call.
var psCommandLine = regexp.MustCompile(`^\s*(\d+)\s+(.*)$`) //nolint:gochecknoglobals

// listProcesses reads every process from ps at once. gopsutil runs ps per process for some
// fields on macOS, which takes seconds, and can't read the others for another user's
// processes without being root, while ps can.
func listProcesses() ([]processSnapshot, error) {
	vmStat, err := mem.VirtualMemory()
	if err != nil {
		return nil, err
	}

	stats, err := exec.Command(psPath, "-axww", "-o", "pid=,ppid=,state=,rss=,time=,user=,comm=").Output()
	if err != nil {
		return nil, fmt.Errorf("list processes: %w", err)
	}
	// The path and the arguments can't be told apart in one column, so they're read apart.
	commands, err := exec.Command(psPath, "-axww", "-o", "pid=,command=").Output()
	if err != nil {
		return nil, fmt.Errorf("list process commands: %w", err)
	}

	cmdlines := make(map[string]string)
	for line := range strings.Lines(string(commands)) {
		if m := psCommandLine.FindStringSubmatch(strings.TrimRight(line, "\n")); m != nil {
			cmdlines[m[1]] = m[2]
		}
	}

	return parsePSStats(string(stats), cmdlines, vmStat.Total), nil
}

func parsePSStats(stats string, cmdlines map[string]string, memTotal uint64) []processSnapshot {
	var snapshots []processSnapshot
	for line := range strings.Lines(stats) {
		m := psStatsLine.FindStringSubmatch(strings.TrimRight(line, "\n"))
		if m == nil {
			continue
		}
		pid, err := strconv.ParseInt(m[1], 10, 32)
		if err != nil {
			continue
		}
		ppid, err := strconv.ParseInt(m[2], 10, 32)
		if err != nil {
			continue
		}
		rssKB, err := strconv.ParseUint(m[4], 10, 64)
		if err != nil {
			continue
		}

		info := ProcessInfo{
			PID:     int32(pid),
			PPID:    int32(ppid),
			Name:    filepath.Base(m[7]),
			User:    m[6],
			Status:  m[3][:1],
			Cmdline: cmdlines[m[1]],
			MemRSS:  rssKB * 1024,
		}
		if filepath.IsAbs(m[7]) {
			info.Exe = m[7]
		}
		if memTotal > 0 {
			info.MemPercent = float32(float64(info.MemRSS) / float64(memTotal) * 100)
		}
		snapshots = append(snapshots, processSnapshot{info: info, cpuTime: parsePSTime(m[5])})
	}

	return snapshots
}

// parsePSTime parses ps's [[hours:]minutes:]seconds.hundredths time, in seconds. The decimal
// separator follows the locale.
func parsePSTime(value string) float64 {
	var seconds float64
	for part := range strings.SplitSeq(strings.ReplaceAll(value, ",", "."), ":") {
		n, err := strconv.ParseFloat(part, 64)
		if err != nil {
			return 0
		}
		seconds = seconds*60 + n
	}

	return seconds
}
