//go:build unix

package userd

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"time"
)

// userShell is the user's login shell: $SHELL, which a service often lacks, or the account's.
func userShell() string {
	for _, shell := range []string{os.Getenv("SHELL"), accountShell()} {
		if shell != "" {
			if _, err := os.Stat(shell); err == nil { //nolint:gosec // the user's own shell setting
				return shell
			}
		}
	}
	return "/bin/sh"
}

// userShellArgs runs script in shell as an interactive login shell, reading every file users
// set PATH in (~/.profile, ~/.bashrc, ~/.zprofile, ~/.zshrc, ...), with args as its
// positional arguments: "$@" in POSIX shells, $argv in fish.
func userShellArgs(shell, posixScript, fishScript string, args ...string) []string {
	if filepath.Base(shell) == "fish" {
		return append([]string{"-lic", fishScript}, args...)
	}
	return append([]string{"-lic", posixScript, filepath.Base(shell)}, args...)
}

// userCommandOutside runs command through the user's login shell if it finds it there; else
// as is, so starting it fails with an error (a nonexistent command) instead of the shell
// starting and printing "command not found".
func userCommandOutside(command string, args []string) (string, []string, []string) {
	if !userHasCommandOutside(command) {
		return command, args, nil
	}
	shell := userShell()
	return shell, userShellArgs(shell, `exec "$@"`, `exec $argv`, append([]string{command}, args...)...), nil
}

func userHasCommandOutside(command string) bool {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	shell := userShell()
	args := userShellArgs(shell, `command -v "$1"`, `command -v $argv[1]`, command)
	return exec.CommandContext(ctx, shell, args...).Run() == nil //nolint:gosec
}
