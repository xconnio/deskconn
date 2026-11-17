package main

// The service the user daemon ("deskconn serve --mode user") runs as. It ran as deskconn-userd
// before; installs from then move to the new names on their own (see service_linux.go,
// install.sh and install.ps1).
const (
	// serviceName is its systemd user unit on Linux: deskconn.service.
	serviceName = "deskconn"
	// legacyServiceName is the systemd user unit it ran as before.
	legacyServiceName = "deskconn-userd"

	// launchdLabel is the launchd agent it runs as on macOS.
	launchdLabel = "com.deskconn.deskconn"
	// legacyLaunchdLabel is the launchd agent it ran as before.
	legacyLaunchdLabel = "com.deskconn.deskconn-userd"
)
