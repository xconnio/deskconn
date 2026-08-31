package deskconn

import "fmt"

func (s *Screen) Lock() error {
	if !s.lockInitialized || s.lockProvider == nil {
		return fmt.Errorf("screen lock provider not initialized")
	}

	if locked, err := s.IsLocked(); err == nil && locked {
		return nil
	}

	obj := s.sessionBus.Object(s.lockProvider.service, s.lockProvider.path)
	return obj.Call(s.lockProvider.iface+"."+s.lockProvider.lock, 0).Err
}

func (s *Screen) IsLocked() (bool, error) {
	if !s.lockInitialized || s.lockProvider == nil {
		return false, fmt.Errorf("screen lock provider not initialized")
	}
	if s.lockProvider.active == "" {
		return false, fmt.Errorf("provider does not support isLocked")
	}

	obj := s.sessionBus.Object(s.lockProvider.service, s.lockProvider.path)
	var active bool
	err := obj.Call(s.lockProvider.iface+"."+s.lockProvider.active, 0).Store(&active)
	return active, err
}
