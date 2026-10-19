package userd

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"golang.org/x/sys/windows/registry"
)

// registryPath is the user's PATH as it is now, not as it was when userd started:
// installers (npm, Claude Code, ...) add themselves to the system or user Path in the
// registry, which processes already running don't see.
func registryPath() string {
	var dirs []string
	for _, key := range []struct {
		root registry.Key
		path string
	}{
		{registry.LOCAL_MACHINE, `SYSTEM\CurrentControlSet\Control\Session Manager\Environment`},
		{registry.CURRENT_USER, `Environment`},
	} {
		k, err := registry.OpenKey(key.root, key.path, registry.QUERY_VALUE)
		if err != nil {
			continue
		}
		value, _, err := k.GetStringValue("Path")
		_ = k.Close()
		if err != nil {
			continue
		}
		if expanded, err := registry.ExpandString(value); err == nil {
			value = expanded
		}
		dirs = append(dirs, value)
	}
	return strings.Join(dirs, string(os.PathListSeparator))
}

// lookPathIn finds command in pathList's directories as exec.LookPath does in PATH's: as is
// if it has an extension, else with each of PATHEXT's (codex.cmd, claude.exe, ...).
func lookPathIn(command, pathList string) string {
	exts := []string{""}
	if filepath.Ext(command) == "" {
		pathExt := os.Getenv("PATHEXT")
		if pathExt == "" {
			pathExt = ".COM;.EXE;.BAT;.CMD"
		}
		exts = strings.Split(strings.ToLower(pathExt), ";")
	}
	for _, dir := range filepath.SplitList(pathList) {
		for _, ext := range exts {
			path := filepath.Join(dir, command+ext)
			if info, err := os.Stat(path); err == nil && !info.IsDir() { //nolint:gosec // the user's own PATH
				return path
			}
		}
	}
	return ""
}

// userCommand returns how to run command with args, and the environment to run it with (nil
// inherits userd's). A command on userd's own PATH runs as is; any other runs from the
// registry's PATH, as it is now rather than when userd started, with that PATH, so what it
// runs in turn (node, for npm's codex.cmd) is found too.
func userCommand(command string, args []string) (string, []string, []string) {
	if resolved, err := exec.LookPath(command); err == nil {
		return resolved, args, nil
	}
	pathList := registryPath()
	resolved := lookPathIn(command, pathList)
	if resolved == "" {
		return command, args, nil
	}
	env := []string{"PATH=" + pathList}
	for _, kv := range os.Environ() {
		if name, _, _ := strings.Cut(kv, "="); !strings.EqualFold(name, "PATH") {
			env = append(env, kv)
		}
	}
	return resolved, args, env
}

// userHasCommand reports whether userd's own PATH or the registry's finds command.
func userHasCommand(command string) bool {
	if _, err := exec.LookPath(command); err == nil {
		return true
	}
	return lookPathIn(command, registryPath()) != ""
}
