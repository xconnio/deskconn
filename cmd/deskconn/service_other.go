//go:build !linux

package main

// migrateService is Linux only: there are no macOS releases for desk self update to install,
// and Windows installs update through install.ps1, which migrates the scheduled task itself.
func migrateService(string) (bool, error) { return false, nil }

// migrateServiceOnStart is Linux only, for the same reason as migrateService.
func migrateServiceOnStart() {}
