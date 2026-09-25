package xlink

// Aliases exposing unexported standalone-mode internals to the package's external tests.

const (
	StandaloneAuthRole      = standaloneAuthRole
	StandaloneDefaultListen = standaloneDefaultListen
)

var (
	LoadOrCreateCert     = loadOrCreateCert
	StandalonePrincipals = standalonePrincipals
	StartStandalone      = startStandalone
)
