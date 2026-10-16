package userd

import "os/exec"

// userCommand returns how to run command with args as the user's own shell would find it,
// and the environment to run it with (nil inherits userd's). userd runs as a service
// (systemd, launchd, a Windows logon task), whose PATH can lack what the user's has: nvm's
// bin, ~/.local/bin, Homebrew, or anything installed since it started. A command on
// userd's own PATH runs as is; any other goes through the platform's userCommandOutside.
func userCommand(command string, args []string) (string, []string, []string) {
	if resolved, err := exec.LookPath(command); err == nil {
		return resolved, args, nil
	}
	return userCommandOutside(command, args)
}

// userHasCommand reports whether the user's own shell finds command.
func userHasCommand(command string) bool {
	if _, err := exec.LookPath(command); err == nil {
		return true
	}
	return userHasCommandOutside(command)
}
