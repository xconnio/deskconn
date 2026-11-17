//go:build linux

package main

import (
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// fakeSystemd stands in for "systemctl --user", keeping which units are enabled and active.
type fakeSystemd struct {
	enabled, active map[string]bool
	calls           []string
	failStart       string // a unit whose start fails
	// migrationDropIn is the drop-in that makes starting deskconn.service stop
	// deskconn-userd, as its Conflicts= does.
	migrationDropIn string
	failCommand     string // a command, such as "daemon-reload", that fails
	mainPID         map[string]int
}

func newFakeSystemd(t *testing.T) *fakeSystemd {
	f := &fakeSystemd{enabled: map[string]bool{}, active: map[string]bool{}, mainPID: map[string]int{}}
	previousCtl, previousPID := systemctlUser, serviceMainPID
	systemctlUser = f.run
	serviceMainPID = func(name string) int { return f.mainPID[name] }
	t.Cleanup(func() { systemctlUser, serviceMainPID = previousCtl, previousPID })
	return f
}

func (f *fakeSystemd) run(args ...string) error {
	f.calls = append(f.calls, strings.Join(args, " "))
	if args[0] == f.failCommand {
		return errors.New(f.failCommand + " failed")
	}
	unit := args[len(args)-1]
	ok := func(b bool) error {
		if b {
			return nil
		}
		return errors.New("no")
	}
	switch args[0] {
	case "is-enabled":
		return ok(f.enabled[unit])
	case "is-active":
		return ok(f.active[unit])
	case "enable":
		f.enabled[unit] = true
	case "disable":
		f.enabled[unit] = false
	case "start", "restart":
		if unit == serviceName && f.migrationDropIn != "" {
			if _, err := os.Stat(f.migrationDropIn); err == nil && f.active[legacyServiceName] {
				f.active[legacyServiceName] = false
				f.calls = append(f.calls, "(conflict) stop "+legacyServiceName)
			}
		}
		if unit == f.failStart {
			return errors.New("start failed")
		}
		f.active[unit] = true
	case "stop":
		f.active[unit] = false
	}
	return nil
}

func (f *fakeSystemd) index(call string) int { return slices.Index(f.calls, call) }

// legacyInstall writes an install from before the rename: deskconn-userd's unit with the
// environment the installer captured, a drop-in with the user's flags, and the executable.
func legacyInstall(t *testing.T, systemd *fakeSystemd) (homeDir string, units serviceUnits, exe string) {
	homeDir = t.TempDir()
	units = newServiceUnits(homeDir)
	require.NoError(t, os.MkdirAll(units.legacyDropIns, 0755))
	require.NoError(t, os.WriteFile(units.legacyUnit, []byte(legacyUnitContents), 0644))
	require.NoError(t, os.WriteFile(filepath.Join(units.legacyDropIns, "override.conf"),
		[]byte(legacyDropInContents), 0644))
	_, execDir := installDirs(homeDir)
	require.NoError(t, os.MkdirAll(execDir, 0755))
	exe = filepath.Join(execDir, binaryName)
	require.NoError(t, os.WriteFile(exe, []byte("binary"), 0755))
	systemd.migrationDropIn = filepath.Join(units.dropIns, migrationDropIn)
	return homeDir, units, exe
}

const (
	legacyUnitContents   = "[Service]\nExecStart=/x/deskconn serve --mode user\nEnvironment=DISPLAY=:1\n"
	legacyDropInContents = "[Service]\nExecStart=\nExecStart=/x/deskconn serve --mode standalone\n" +
		"Environment=FOO=bar\n"
)

func requireContents(t *testing.T, path, want string) {
	t.Helper()
	data, err := os.ReadFile(path)
	require.NoError(t, err)
	require.Equal(t, want, string(data))
}

func TestMigrateServiceMovesRunningService(t *testing.T) {
	systemd := newFakeSystemd(t)
	homeDir, units, exe := legacyInstall(t, systemd)
	systemd.enabled[legacyServiceName], systemd.active[legacyServiceName] = true, true

	migrated, err := migrateService(homeDir)
	require.NoError(t, err)
	require.True(t, migrated)

	requireContents(t, units.unit, legacyUnitContents)
	requireContents(t, filepath.Join(units.dropIns, "override.conf"), legacyDropInContents)
	require.NoFileExists(t, units.legacyUnit)
	require.NoDirExists(t, units.legacyDropIns)
	requireContents(t, exe, "binary")

	require.True(t, systemd.enabled[serviceName])
	require.True(t, systemd.active[serviceName])
	require.False(t, systemd.enabled[legacyServiceName])
	require.False(t, systemd.active[legacyServiceName])
	require.Equal(t, -1, systemd.index("stop "+legacyServiceName),
		"stopping the old service on its own would stop this process too when it runs in it")
	require.NotEqual(t, -1, systemd.index("(conflict) stop "+legacyServiceName),
		"starting the new service stops the old one, in the same transaction")
	require.NoFileExists(t, filepath.Join(units.dropIns, migrationDropIn))

	// Done once, there's nothing left to do.
	systemd.calls = nil
	migrated, err = migrateService(homeDir)
	require.NoError(t, err)
	require.False(t, migrated)
	require.Empty(t, systemd.calls)
}

func TestMigrateServiceKeepsStoppedServiceStopped(t *testing.T) {
	systemd := newFakeSystemd(t)
	homeDir, _, _ := legacyInstall(t, systemd)
	systemd.enabled[legacyServiceName] = true

	migrated, err := migrateService(homeDir)
	require.NoError(t, err)
	require.True(t, migrated)
	require.True(t, systemd.enabled[serviceName])
	require.False(t, systemd.active[serviceName])
	require.Equal(t, -1, systemd.index("restart "+serviceName))
}

func TestMigrateServiceWithoutLegacyService(t *testing.T) {
	systemd := newFakeSystemd(t)
	migrated, err := migrateService(t.TempDir())
	require.NoError(t, err)
	require.False(t, migrated)
	require.Empty(t, systemd.calls)
}

func TestMigrateServiceRestoresLegacyServiceOnFailure(t *testing.T) {
	systemd := newFakeSystemd(t)
	homeDir, units, exe := legacyInstall(t, systemd)
	systemd.enabled[legacyServiceName], systemd.active[legacyServiceName] = true, true
	systemd.failStart = serviceName

	migrated, err := migrateService(homeDir)
	require.Error(t, err)
	require.True(t, migrated)

	requireContents(t, units.legacyUnit, legacyUnitContents)
	requireContents(t, filepath.Join(units.legacyDropIns, "override.conf"), legacyDropInContents)
	require.NoFileExists(t, units.unit)
	require.NoDirExists(t, units.dropIns)
	requireContents(t, exe, "binary")
	require.True(t, systemd.enabled[legacyServiceName])
	require.True(t, systemd.active[legacyServiceName])
	require.False(t, systemd.enabled[serviceName])
}

func TestMigrateServiceKeepsExistingNewUnit(t *testing.T) {
	systemd := newFakeSystemd(t)
	homeDir, units, _ := legacyInstall(t, systemd)
	require.NoError(t, os.MkdirAll(units.dropIns, 0755))
	require.NoError(t, os.WriteFile(units.unit, []byte("new"), 0644))
	require.NoError(t, os.WriteFile(filepath.Join(units.dropIns, "override.conf"), []byte("mine"), 0644))

	_, err := migrateService(homeDir)
	require.NoError(t, err)
	requireContents(t, units.unit, "new")
	requireContents(t, filepath.Join(units.dropIns, "override.conf"), "mine")
}

func TestPrepareServiceSwap(t *testing.T) {
	systemd := newFakeSystemd(t)
	_, units, exe := legacyInstall(t, systemd)
	systemd.enabled[legacyServiceName], systemd.active[legacyServiceName] = true, true

	rollback, err := prepareServiceSwap(units)
	require.NoError(t, err)
	requireContents(t, units.unit, legacyUnitContents)
	requireContents(t, filepath.Join(units.dropIns, migrationDropIn), migrationDropInContents)
	require.True(t, systemd.enabled[serviceName])
	require.False(t, systemd.enabled[legacyServiceName])
	require.True(t, systemd.active[legacyServiceName], "nothing is stopped until deskconn.service starts")
	// The legacy unit stays until the new daemon runs: it's the way back.
	requireContents(t, units.legacyUnit, legacyUnitContents)

	rollback()
	require.NoFileExists(t, units.unit)
	require.NoDirExists(t, units.dropIns)
	require.True(t, systemd.enabled[legacyServiceName])
	require.False(t, systemd.enabled[serviceName])
	requireContents(t, units.legacyUnit, legacyUnitContents)
	requireContents(t, exe, "binary")
}

func TestMigrateServiceOnStartFinishesAsNewService(t *testing.T) {
	systemd := newFakeSystemd(t)
	homeDir, units, exe := legacyInstall(t, systemd)
	t.Setenv("HOME", homeDir)
	_, err := prepareServiceSwap(units)
	require.NoError(t, err)
	systemd.active[legacyServiceName], systemd.active[serviceName] = false, true

	setCgroup(t, "0::/user.slice/user-1000.slice/user@1000.service/app.slice/"+serviceName+".service\n")
	migrateServiceOnStart()

	require.NoFileExists(t, units.legacyUnit)
	require.NoDirExists(t, units.legacyDropIns)
	require.NoFileExists(t, filepath.Join(units.dropIns, migrationDropIn))
	requireContents(t, filepath.Join(units.dropIns, "override.conf"), legacyDropInContents)
	requireContents(t, units.unit, legacyUnitContents)
	requireContents(t, exe, "binary")

	// Later starts have nothing to do.
	systemd.calls = nil
	migrateServiceOnStart()
	require.Empty(t, systemd.calls)
}

func TestMigrateServiceOnStartOutsideServices(t *testing.T) {
	systemd := newFakeSystemd(t)
	homeDir, units, _ := legacyInstall(t, systemd)
	t.Setenv("HOME", homeDir)
	setCgroup(t, "0::/user.slice/user-1000.slice/session-2.scope\n")

	migrateServiceOnStart()
	require.Empty(t, systemd.calls, "a daemon run by hand leaves the services alone")
	requireContents(t, units.legacyUnit, legacyUnitContents)
}

func TestRunningInUnit(t *testing.T) {
	setCgroup(t, "0::/user.slice/user-1000.slice/user@1000.service/app.slice/deskconn-userd.service\n")
	require.True(t, runningInUnit(legacyServiceName))
	require.False(t, runningInUnit(serviceName), "deskconn-userd.service isn't deskconn.service")
}

func setCgroup(t *testing.T, contents string) {
	path := filepath.Join(t.TempDir(), "cgroup")
	require.NoError(t, os.WriteFile(path, []byte(contents), 0644))
	previous := procSelfCgroup
	procSelfCgroup = path
	t.Cleanup(func() { procSelfCgroup = previous })
}

// snapshotOutsideUnits records every path in homeDir outside the systemd user unit directory,
// with its contents or link target, to check that a migration changes nothing else (the
// executable above all).
func snapshotOutsideUnits(t *testing.T, homeDir string) map[string]string {
	t.Helper()
	unitDir := filepath.Join(homeDir, ".config", "systemd", "user")
	snapshot := map[string]string{}
	require.NoError(t, filepath.WalkDir(homeDir, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if path == unitDir {
			return filepath.SkipDir
		}
		switch {
		case entry.Type()&os.ModeSymlink != 0:
			target, err := os.Readlink(path)
			snapshot[path] = "link:" + target
			return err
		case entry.IsDir():
			snapshot[path] = "dir"
		default:
			data, err := os.ReadFile(path)
			snapshot[path] = string(data)
			return err
		}
		return nil
	}))
	return snapshot
}

func TestMigrateServiceOnStartIgnoresOtherProcessesInLegacyUnit(t *testing.T) {
	systemd := newFakeSystemd(t)
	homeDir, units, _ := legacyInstall(t, systemd)
	t.Setenv("HOME", homeDir)
	systemd.active[legacyServiceName] = true
	systemd.mainPID[legacyServiceName] = os.Getpid() + 1 // the service's daemon, not this process
	setCgroup(t, "0::/user.slice/user-1000.slice/user@1000.service/app.slice/"+legacyServiceName+".service\n")

	migrateServiceOnStart()
	require.Empty(t, systemd.calls, "a process that isn't the service's main one leaves it alone")
	require.NoFileExists(t, units.unit)
	requireContents(t, units.legacyUnit, legacyUnitContents)
}

func TestMigrateServiceKeepsSymlinkedDropIns(t *testing.T) {
	systemd := newFakeSystemd(t)
	homeDir, units, _ := legacyInstall(t, systemd)
	dotfiles := filepath.Join(homeDir, "dotfiles")
	require.NoError(t, os.MkdirAll(dotfiles, 0755))
	require.NoError(t, os.WriteFile(filepath.Join(dotfiles, "turn.conf"), []byte("[Service]\nEnvironment=T=1\n"), 0644))
	require.NoError(t, os.Symlink(filepath.Join(dotfiles, "turn.conf"), filepath.Join(units.legacyDropIns, "abs.conf")))
	require.NoError(t, os.Symlink("../../../../dotfiles/turn.conf", filepath.Join(units.legacyDropIns, "rel.conf")))
	require.NoError(t, os.Symlink("/nowhere/gone.conf", filepath.Join(units.legacyDropIns, "dangling.conf")))
	before := snapshotOutsideUnits(t, homeDir)

	_, err := migrateService(homeDir)
	require.NoError(t, err)
	for _, name := range []string{"abs.conf", "rel.conf", "dangling.conf"} {
		link := filepath.Join(units.dropIns, name)
		info, err := os.Lstat(link)
		require.NoError(t, err, name)
		require.NotZero(t, info.Mode()&os.ModeSymlink, "%s should still be a symlink", name)
	}
	requireContents(t, filepath.Join(units.dropIns, "rel.conf"), "[Service]\nEnvironment=T=1\n")
	requireContents(t, filepath.Join(units.dropIns, "abs.conf"), "[Service]\nEnvironment=T=1\n")
	require.Equal(t, before, snapshotOutsideUnits(t, homeDir), "the symlinks' targets must survive")
}

func TestMigrateServiceWithSymlinkedDropInDirectory(t *testing.T) {
	systemd := newFakeSystemd(t)
	homeDir, units, _ := legacyInstall(t, systemd)
	dotfiles := filepath.Join(homeDir, "dotfiles", "deskconn-userd.service.d")
	require.NoError(t, os.MkdirAll(filepath.Dir(dotfiles), 0755))
	require.NoError(t, os.Rename(units.legacyDropIns, dotfiles))
	require.NoError(t, os.Symlink(dotfiles, units.legacyDropIns))
	before := snapshotOutsideUnits(t, homeDir)

	_, err := migrateService(homeDir)
	require.NoError(t, err)
	requireContents(t, filepath.Join(units.dropIns, "override.conf"), legacyDropInContents)
	_, err = os.Lstat(units.legacyDropIns)
	require.ErrorIs(t, err, os.ErrNotExist)
	require.Equal(t, before, snapshotOutsideUnits(t, homeDir), "removing the old directory's link keeps its target")
}

func TestMigrateServiceChangesNothingOutsideUnits(t *testing.T) {
	for name, setup := range map[string]func(*fakeSystemd){
		"running":      func(f *fakeSystemd) { f.enabled[legacyServiceName], f.active[legacyServiceName] = true, true },
		"stopped":      func(f *fakeSystemd) {},
		"start fails":  func(f *fakeSystemd) { f.active[legacyServiceName], f.failStart = true, serviceName },
		"reload fails": func(f *fakeSystemd) { f.failCommand = "daemon-reload" },
	} {
		t.Run(name, func(t *testing.T) {
			systemd := newFakeSystemd(t)
			homeDir, _, _ := legacyInstall(t, systemd)
			setup(systemd)
			before := snapshotOutsideUnits(t, homeDir)
			_, _ = migrateService(homeDir)
			_, _ = migrateService(homeDir)
			require.Equal(t, before, snapshotOutsideUnits(t, homeDir))
		})
	}
}

func TestPrepareServiceSwapUndoesFailure(t *testing.T) {
	for name, fail := range map[string]func(*fakeSystemd, serviceUnits){
		"daemon-reload fails": func(f *fakeSystemd, _ serviceUnits) { f.failCommand = "daemon-reload" },
		"enable fails":        func(f *fakeSystemd, _ serviceUnits) { f.failCommand = "enable" },
		"drop-in unreadable": func(_ *fakeSystemd, u serviceUnits) {
			require.NoError(t, os.Chmod(filepath.Join(u.legacyDropIns, "override.conf"), 0))
		},
	} {
		t.Run(name, func(t *testing.T) {
			systemd := newFakeSystemd(t)
			_, units, _ := legacyInstall(t, systemd)
			systemd.enabled[legacyServiceName], systemd.active[legacyServiceName] = true, true
			fail(systemd, units)

			_, err := prepareServiceSwap(units)
			require.Error(t, err)
			require.NoFileExists(t, units.unit)
			require.NoDirExists(t, units.dropIns)
			require.True(t, systemd.enabled[legacyServiceName])
			require.False(t, systemd.enabled[serviceName])
			requireContents(t, units.legacyUnit, legacyUnitContents)
		})
	}
}

// deskconn-userd reinstalled beside a deskconn.service that runs already, by an installer from
// before the rename: it goes, and deskconn.service stays as it was.
func TestMigrateServiceBesideRunningService(t *testing.T) {
	systemd := newFakeSystemd(t)
	homeDir, units, _ := legacyInstall(t, systemd)
	require.NoError(t, os.WriteFile(units.unit, []byte("mine"), 0644))
	systemd.enabled[serviceName], systemd.active[serviceName] = true, true
	systemd.enabled[legacyServiceName], systemd.active[legacyServiceName] = true, true

	migrated, err := migrateService(homeDir)
	require.NoError(t, err)
	require.True(t, migrated)
	require.True(t, systemd.enabled[serviceName])
	require.True(t, systemd.active[serviceName])
	require.False(t, systemd.active[legacyServiceName], "two daemons can't run")
	require.NoFileExists(t, units.legacyUnit)
	requireContents(t, units.unit, "mine")
}

func TestPrepareServiceSwapRollbackKeepsEnabledService(t *testing.T) {
	systemd := newFakeSystemd(t)
	_, units, _ := legacyInstall(t, systemd)
	systemd.enabled[serviceName] = true
	systemd.enabled[legacyServiceName] = true

	rollback, err := prepareServiceSwap(units)
	require.NoError(t, err)
	rollback()
	require.True(t, systemd.enabled[serviceName], "it was enabled before, so it stays enabled")
	require.True(t, systemd.enabled[legacyServiceName])
}
