//go:build linux

package main

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	log "github.com/sirupsen/logrus"
)

// systemctlUser runs "systemctl --user args...", returning what systemctl said if it fails.
// Tests replace it to record the calls.
var systemctlUser = func(args ...string) error { //nolint:gochecknoglobals // replaced in tests
	out, err := exec.Command("systemctl", append([]string{"--user"}, args...)...).CombinedOutput() // nolint: gosec
	if err == nil {
		return nil
	}
	// Not wrapped: the CLI takes an *exec.ExitError for a command that has reported its own
	// failure, and systemctl's output was captured here instead.
	if msg := strings.TrimSpace(string(out)); msg != "" {
		return errors.New(msg)
	}
	return fmt.Errorf("systemctl --user %s: %s", strings.Join(args, " "), err.Error())
}

// procSelfCgroup is where the daemon reads which systemd unit it runs in. Tests replace it.
var procSelfCgroup = "/proc/self/cgroup" //nolint:gochecknoglobals // replaced in tests

// serviceMainPID returns the main process of the systemd user unit name.service, or 0.
// Tests replace it.
var serviceMainPID = func(name string) int { //nolint:gochecknoglobals // replaced in tests
	out, err := exec.Command("systemctl", "--user", "show", "--property=MainPID", "--value", // nolint: gosec
		name+".service").Output()
	if err != nil {
		return 0
	}
	pid, _ := strconv.Atoi(strings.TrimSpace(string(out)))
	return pid
}

// migrationDropIn is the drop-in a daemon running as deskconn-userd gives deskconn.service, so
// that starting deskconn.service stops deskconn-userd first: Conflicts= stops it, and any
// ordering between the two makes systemd finish that stop before the start. The daemon that
// then runs as deskconn.service removes it.
const (
	migrationDropIn         = "50-migrate-from-" + legacyServiceName + ".conf"
	migrationDropInContents = "[Unit]\nConflicts=" + legacyServiceName + ".service\nAfter=" +
		legacyServiceName + ".service\n"
)

// serviceSwapTimeout is how long a daemon that asked systemd to replace deskconn-userd with
// deskconn.service waits to be stopped, before taking the change back and running on as
// deskconn-userd.
const serviceSwapTimeout = 30 * time.Second

// serviceUnits are the unit files of the service and of the one it replaces.
type serviceUnits struct {
	unit, dropIns             string
	legacyUnit, legacyDropIns string
}

func newServiceUnits(homeDir string) serviceUnits {
	dir := filepath.Join(homeDir, ".config", "systemd", "user")
	return serviceUnits{
		unit:          filepath.Join(dir, serviceName+".service"),
		dropIns:       filepath.Join(dir, serviceName+".service.d"),
		legacyUnit:    filepath.Join(dir, legacyServiceName+".service"),
		legacyDropIns: filepath.Join(dir, legacyServiceName+".service.d"),
	}
}

// legacyInstalled reports whether deskconn-userd's unit file is still there, to migrate.
func (u serviceUnits) legacyInstalled() (bool, error) {
	_, err := os.Stat(u.legacyUnit)
	if errors.Is(err, fs.ErrNotExist) {
		return false, nil
	}
	return err == nil, err
}

// copyFromLegacy gives deskconn.service deskconn-userd's unit file, with the environment the
// installer captured in it, and its drop-ins, with the flags and variables added to it, leaving
// any file deskconn.service already has as it is. A drop-in that's a symlink (to a dotfiles
// repository, say) stays one, to the same target: the two drop-in directories are siblings, so
// a relative target still resolves. The unit file is copied whatever it is, as systemd would
// take a symlink named deskconn.service for an alias of the unit it points to. undo removes
// what it created.
func (u serviceUnits) copyFromLegacy() (undo func(), err error) {
	var created []string
	undo = func() {
		for i := len(created) - 1; i >= 0; i-- {
			_ = os.Remove(created[i])
		}
	}
	copyFile := func(from, to string, keepSymlink bool) error {
		if _, err := os.Lstat(to); err == nil {
			return nil
		} else if !errors.Is(err, fs.ErrNotExist) {
			return err
		}
		if info, err := os.Lstat(from); err == nil && keepSymlink && info.Mode()&fs.ModeSymlink != 0 {
			target, err := os.Readlink(from)
			if err != nil {
				return err
			}
			if err := os.Symlink(target, to); err != nil {
				return err
			}
			created = append(created, to)
			return nil
		}
		data, err := os.ReadFile(from)
		if err != nil {
			return err
		}
		if err := os.WriteFile(to, data, 0644); err != nil { // nolint: gosec
			return err
		}
		created = append(created, to)
		return nil
	}

	if err := copyFile(u.legacyUnit, u.unit, false); err != nil {
		undo()
		return nil, fmt.Errorf("failed to create %s: %w", u.unit, err)
	}
	dropIns, err := os.ReadDir(u.legacyDropIns)
	if errors.Is(err, fs.ErrNotExist) {
		return undo, nil
	}
	if err != nil {
		undo()
		return nil, err
	}
	if _, err := os.Stat(u.dropIns); errors.Is(err, fs.ErrNotExist) {
		if err := os.MkdirAll(u.dropIns, 0755); err != nil {
			undo()
			return nil, err
		}
		created = append(created, u.dropIns)
	}
	for _, dropIn := range dropIns {
		if dropIn.IsDir() {
			continue
		}
		if err := copyFile(filepath.Join(u.legacyDropIns, dropIn.Name()),
			filepath.Join(u.dropIns, dropIn.Name()), true); err != nil {
			undo()
			return nil, fmt.Errorf("failed to copy %s's drop-in %s: %w", legacyServiceName, dropIn.Name(), err)
		}
	}
	return undo, nil
}

// removeLegacy removes deskconn-userd's unit file and drop-ins, and the migration drop-in,
// once deskconn.service has taken over. It only ever removes files in the systemd user unit
// directory, never the deskconn executable.
func (u serviceUnits) removeLegacy() error {
	for _, path := range []string{u.legacyUnit, filepath.Join(u.dropIns, migrationDropIn)} {
		if err := os.Remove(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return fmt.Errorf("failed to remove %s: %w", path, err)
		}
	}
	if err := os.RemoveAll(u.legacyDropIns); err != nil {
		return fmt.Errorf("failed to remove %s: %w", u.legacyDropIns, err)
	}
	_ = os.Remove(u.dropIns) // only if the migration drop-in was all it had
	_ = systemctlUser("daemon-reload")
	_ = systemctlUser("reset-failed", legacyServiceName)
	return nil
}

// migrateService moves an install's deskconn-userd service to deskconn.service, for the CLI
// (desk self update): deskconn.service takes over deskconn-userd's unit and drop-ins, and its
// enablement, and replaces it if it was running. If deskconn.service can't start, deskconn-userd
// is put back as it was. It reports whether there was a deskconn-userd service to move.
func migrateService(homeDir string) (bool, error) {
	units := newServiceUnits(homeDir)
	if installed, err := units.legacyInstalled(); !installed {
		return false, err
	}
	wasActive := systemctlUser("is-active", "--quiet", legacyServiceName) == nil

	fmt.Printf("Renaming the %s service to %s...\n", legacyServiceName, serviceName)
	rollback, err := prepareServiceSwap(units)
	if err != nil {
		return true, fmt.Errorf("failed to move the %s service to %s, so %s was kept: %w",
			legacyServiceName, serviceName, legacyServiceName, err)
	}
	// One restart, which systemd carries out whole: it stops deskconn-userd, then starts
	// deskconn.service (see migrationDropIn). It's done even if this process is stopped with
	// deskconn-userd, as it is when run from a desk shell on this machine; the new daemon
	// then finishes the move. A restart rather than a start, for a deskconn.service that's
	// running already (deskconn-userd reinstalled beside it): starting it would do nothing,
	// leaving deskconn-userd running too.
	if wasActive {
		if err := systemctlUser("restart", serviceName); err != nil {
			rollback()
			_ = systemctlUser("start", legacyServiceName)
			return true, fmt.Errorf("failed to move the %s service to %s, so %s was kept: %w",
				legacyServiceName, serviceName, legacyServiceName, err)
		}
	}
	return true, units.removeLegacy()
}

// migrateServiceOnStart moves the daemon from deskconn-userd to deskconn.service when it
// runs as deskconn-userd: started by an updater from before the rename, which installs
// this binary and restarts deskconn-userd, or by a login or boot since. It can't stop
// itself and wait, so it has systemd swap the services (see migrationDropIn) and waits to
// be stopped. If that doesn't happen, the change is taken back and this process runs on as
// deskconn-userd, to try again at its next start.
//
// Running as deskconn.service instead, it removes what's left of deskconn-userd.
func migrateServiceOnStart() {
	homeDir, err := os.UserHomeDir()
	if err != nil {
		return
	}
	units := newServiceUnits(homeDir)

	if runningInUnit(serviceName) {
		installed, _ := units.legacyInstalled()
		_, dropInErr := os.Stat(filepath.Join(units.dropIns, migrationDropIn))
		if (installed || dropInErr == nil) && systemctlUser("is-active", "--quiet", legacyServiceName) != nil {
			if err := units.removeLegacy(); err != nil {
				log.Warnf("service: %v", err)
			}
		}
		return
	}
	// Its main process, not anything else in its cgroup, such as a daemon run by hand from a
	// desk shell on this machine, which is in it too.
	if !runningInUnit(legacyServiceName) || serviceMainPID(legacyServiceName) != os.Getpid() {
		return
	}
	if installed, _ := units.legacyInstalled(); !installed {
		return
	}

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, os.Interrupt, syscall.SIGTERM)
	rollback, err := prepareServiceSwap(units)
	if err != nil {
		signal.Stop(sigCh)
		log.Warnf("service: staying %s: %v", legacyServiceName, err)
		return
	}
	log.Printf("service: moving to %s, waiting for systemd to switch over", serviceName)
	// Its error doesn't tell: stopping this service also stops systemctl, which runs in it.
	// A restart, as in migrateService.
	_ = systemctlUser("restart", "--no-block", serviceName)
	select {
	case <-sigCh:
		os.Exit(0)
	case <-time.After(serviceSwapTimeout):
	}
	signal.Stop(sigCh)
	rollback()
	log.Warnf("service: systemd didn't start %s, staying %s", serviceName, legacyServiceName)
}

// prepareServiceSwap readies deskconn.service to take over from deskconn-userd: it gets
// deskconn-userd's unit and drop-ins, the migration drop-in, and its enablement. Starting
// it then stops deskconn-userd first. rollback takes all of that back.
func prepareServiceSwap(units serviceUnits) (rollback func(), err error) {
	wasEnabled := systemctlUser("is-enabled", "--quiet", legacyServiceName) == nil
	newWasEnabled := systemctlUser("is-enabled", "--quiet", serviceName) == nil

	undo, err := units.copyFromLegacy()
	if err != nil {
		return nil, err
	}
	dropIn := filepath.Join(units.dropIns, migrationDropIn)
	createdDir := false
	if _, err := os.Stat(units.dropIns); errors.Is(err, fs.ErrNotExist) {
		createdDir = true
	}
	if err := os.MkdirAll(units.dropIns, 0755); err != nil {
		undo()
		return nil, err
	}
	rollback = func() {
		if !newWasEnabled {
			_ = systemctlUser("disable", serviceName)
		}
		_ = os.Remove(dropIn)
		if createdDir {
			_ = os.Remove(units.dropIns)
		}
		undo()
		_ = systemctlUser("daemon-reload")
		if wasEnabled {
			_ = systemctlUser("enable", legacyServiceName)
		}
	}
	if err := os.WriteFile(dropIn, []byte(migrationDropInContents), 0644); err != nil { // nolint: gosec
		rollback()
		return nil, err
	}
	if err := systemctlUser("daemon-reload"); err != nil {
		rollback()
		return nil, err
	}
	if wasEnabled {
		if err := systemctlUser("enable", serviceName); err != nil {
			rollback()
			return nil, err
		}
		_ = systemctlUser("disable", legacyServiceName)
	}
	return rollback, nil
}

// runningInUnit reports whether this process runs in the systemd unit name.service.
func runningInUnit(name string) bool {
	data, err := os.ReadFile(procSelfCgroup)
	if err != nil {
		return false
	}
	for _, line := range strings.Split(string(data), "\n") {
		if strings.HasSuffix(line, "/"+name+".service") {
			return true
		}
	}
	return false
}
