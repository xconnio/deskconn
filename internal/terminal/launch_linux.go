package terminal

import (
	"context"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	log "github.com/sirupsen/logrus"
)

// launch starts a worker for it to outlive the daemon. Under systemd, stopping or
// restarting the daemon's service kills every process in its cgroup, so the worker gets a
// scope of its own through systemd-run, where systemd allows it. Either way it leads a
// session of its own, away from hangups and signals meant for the daemon's.
func (l *launcher) launch(exe string, args []string, id string) (*startedWorker, error) {
	if prefix := l.scopePrefix(); prefix != nil {
		name := "deskconn-terminal-" + id[:12]
		full := append(append(prefix, "--unit="+name, "--", exe), args...)
		cmd := exec.Command(full[0], full[1:]...) // #nosec G204 -- systemd-run and this executable
		cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
		if started, err := startWorkerCommand(cmd); err == nil {
			return started, nil
		} else {
			log.Warnf("terminal: systemd-run failed, starting the worker in the daemon's cgroup: %v", err)
		}
	}
	cmd := exec.Command(exe, args...) // #nosec G204 -- this executable
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	return startWorkerCommand(cmd)
}

// launcher starts workers in systemd scopes where it can.
type launcher struct {
	scopeOnce sync.Once
	scope     []string
}

// scopePrefix is the systemd-run command line that runs a command in a scope of its
// own, or nil when this process doesn't run in a systemd service or can't make scopes.
// Whether it can is checked once, by making one.
func (l *launcher) scopePrefix() []string {
	l.scopeOnce.Do(func() {
		prefix := scopeCommand()
		if prefix == nil {
			return
		}
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		probe := append(append([]string{}, prefix...), "--", "true")
		if out, err := exec.CommandContext(ctx, probe[0], probe[1:]...).CombinedOutput(); err != nil { // #nosec G204
			log.Warnf("terminal: can't start terminal workers in scopes of their own (%v: %s); "+
				"restarting the daemon's service will end its terminal sessions",
				err, strings.TrimSpace(string(out)))
			return
		}
		l.scope = prefix
	})
	return l.scope
}

// scopeCommand is systemd-run's command line for this process's systemd manager: the
// user's for a user service, the system's for a system service run as root.
func scopeCommand() []string {
	if _, err := exec.LookPath("systemd-run"); err != nil {
		return nil
	}
	data, err := os.ReadFile("/proc/self/cgroup")
	if err != nil {
		return nil
	}
	cgroup := string(data)
	base := []string{"systemd-run", "--scope", "--quiet", "--collect", "--description=deskconn terminal session"}
	switch {
	case !strings.Contains(cgroup, ".service"):
		return nil
	case strings.Contains(cgroup, "/user@"+strconv.Itoa(os.Getuid())+".service/"):
		return append(base, "--user")
	case os.Getuid() == 0:
		return base
	default:
		return nil
	}
}

func isWindowsConnRefused(error) bool { return false }
