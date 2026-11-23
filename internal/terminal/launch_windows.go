package terminal

import (
	"errors"
	"os/exec"
	"syscall"

	log "github.com/sirupsen/logrus"
	"golang.org/x/sys/windows"
)

// launch starts a worker for it to outlive the daemon: detached from its console,
// and out of the job of the scheduled task that runs the daemon, whose end kills the job's
// processes, where the job allows that.
// launcher starts workers.
type launcher struct{}

func (*launcher) launch(exe string, args []string, _ string) (*startedWorker, error) {
	flags := uint32(windows.CREATE_NEW_PROCESS_GROUP | windows.DETACHED_PROCESS)
	cmd := exec.Command(exe, args...) // #nosec G204 -- this executable
	cmd.SysProcAttr = &syscall.SysProcAttr{CreationFlags: flags | windows.CREATE_BREAKAWAY_FROM_JOB}
	started, err := startWorkerCommand(cmd)
	if err == nil {
		return started, nil
	}
	log.Warnf("terminal: can't start the worker out of the daemon's job, so ending the daemon "+
		"will end its terminal sessions: %v", err)
	cmd = exec.Command(exe, args...) // #nosec G204 -- this executable
	cmd.SysProcAttr = &syscall.SysProcAttr{CreationFlags: flags}
	return startWorkerCommand(cmd)
}

func isWindowsConnRefused(err error) bool {
	return errors.Is(err, windows.WSAECONNREFUSED)
}
