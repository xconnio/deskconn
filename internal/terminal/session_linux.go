package terminal

import (
	"os"
	"strconv"
	"strings"
	"syscall"
)

// killSession kills every process of the session sid leads, found from /proc: an
// interactive shell's jobs run in process groups of their own, which signalling the shell's
// group misses.
func killSession(sid int) {
	if sid <= 0 {
		return
	}
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return
	}
	for _, entry := range entries {
		pid, err := strconv.Atoi(entry.Name())
		if err != nil || pid == os.Getpid() {
			continue
		}
		if processSession(pid) == sid {
			_ = syscall.Kill(pid, syscall.SIGKILL)
		}
	}
}

// processSession is pid's session ID from /proc/<pid>/stat, or -1.
func processSession(pid int) int {
	data, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
	if err != nil {
		return -1
	}
	// The command name, in parentheses, may itself hold spaces and parentheses.
	end := strings.LastIndexByte(string(data), ')')
	if end < 0 {
		return -1
	}
	// After it: state, ppid, pgrp, session.
	fields := strings.Fields(string(data[end+1:]))
	if len(fields) < 4 {
		return -1
	}
	sid, err := strconv.Atoi(fields[3])
	if err != nil {
		return -1
	}
	return sid
}
