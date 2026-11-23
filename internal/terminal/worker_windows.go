//go:build windows

package terminal

import (
	"io/fs"
	"os"
	"os/signal"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

// A ConPTY outlives its process: reading it only ends once it's closed.
const (
	ptyNeedsCloseOnExit = true
	ptyDrainDelay       = 500 * time.Millisecond
)

// processTracker reaches a session's processes through a job object holding the shell,
// which the processes it starts join too.
type processTracker struct {
	pid int
	job windows.Handle
}

func trackProcesses(pid int) processTracker {
	p := processTracker{pid: pid}
	job, err := windows.CreateJobObject(nil, nil)
	if err != nil {
		return p
	}
	info := windows.JOBOBJECT_EXTENDED_LIMIT_INFORMATION{
		BasicLimitInformation: windows.JOBOBJECT_BASIC_LIMIT_INFORMATION{
			LimitFlags: windows.JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE,
		},
	}
	if _, err := windows.SetInformationJobObject(job, windows.JobObjectExtendedLimitInformation,
		uintptr(unsafe.Pointer(&info)), uint32(unsafe.Sizeof(info))); err != nil {
		_ = windows.CloseHandle(job)
		return p
	}
	process, err := windows.OpenProcess(windows.PROCESS_SET_QUOTA|windows.PROCESS_TERMINATE, false, uint32(pid))
	if err != nil {
		_ = windows.CloseHandle(job)
		return p
	}
	defer windows.CloseHandle(process)
	if err := windows.AssignProcessToJobObject(job, process); err != nil {
		_ = windows.CloseHandle(job)
		return p
	}
	p.job = job
	return p
}

// hangup kills the session: Windows has no hangup to give a console program first.
func (p processTracker) hangup() { p.kill() }

func (p processTracker) killGroup() { p.kill() }

func (p processTracker) kill() {
	if p.job != 0 {
		_ = windows.TerminateJobObject(p.job, 1)
		return
	}
	if process, err := os.FindProcess(p.pid); err == nil {
		_ = process.Kill()
	}
}

// release closes the job, which kills anything still in it.
func (p processTracker) release() {
	if p.job != 0 {
		_ = windows.CloseHandle(p.job)
	}
}

// handleSignals keeps the worker running through interrupts meant for whatever started it.
func handleSignals(func()) (stop func()) {
	signal.Ignore(os.Interrupt)
	return func() {}
}

// checkOwner isn't needed on Windows: the terminals directory is under the user's profile.
func checkOwner(fs.FileInfo) error { return nil }

// signaledExitCode is never needed here: a Windows process always has an exit code.
func signaledExitCode(*os.ProcessState) int { return -1 }
