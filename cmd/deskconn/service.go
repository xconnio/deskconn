package main

// The service the user daemon ("deskconn serve --mode user") runs as.
const (
	// serviceName is its systemd user unit on Linux: deskconn.service.
	serviceName = "deskconn"

	// launchdLabel is the launchd agent it runs as on macOS.
	launchdLabel = "com.deskconn.deskconn"
)
