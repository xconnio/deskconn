package deskconn

import (
	"fmt"

	"golang.org/x/sys/windows"
)

var user32 = windows.NewLazySystemDLL("user32.dll") //nolint: gochecknoglobals

var procLockWorkStation = user32.NewProc("LockWorkStation") //nolint: gochecknoglobals

func (s *Screen) Lock() error {
	ret, _, err := procLockWorkStation.Call()
	if ret == 0 {
		return fmt.Errorf("failed to lock workstation: %w", err)
	}
	return nil
}

func (s *Screen) IsLocked() (bool, error) {
	return false, fmt.Errorf("lock state query is not supported on windows")
}
