package terminal

import (
	"os/exec"
	"syscall"
)

// launch starts a worker for it to outlive the daemon: in a session and process
// group of its own, which launchd doesn't kill with the daemon's job.
// launcher starts workers.
type launcher struct{}

func (*launcher) launch(exe string, args []string, _ string) (*startedWorker, error) {
	cmd := exec.Command(exe, args...) // #nosec G204 -- this executable
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	return startWorkerCommand(cmd)
}

func isWindowsConnRefused(error) bool { return false }
