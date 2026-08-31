package deskconn

import (
	"fmt"
	"os/exec"
)

func (s *Screen) Lock() error {
	cgSession := "/System/Library/CoreServices/Menu Extras/User.menu/Contents/Resources/CGSession"
	return exec.Command(cgSession, "-suspend").Run() // #nosec G204
}

func (s *Screen) IsLocked() (bool, error) {
	return false, fmt.Errorf("lock state query is not supported on macos")
}
