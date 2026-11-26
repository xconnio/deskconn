package common

import (
	"crypto/tls"
	"net"
	"os"
)

// cloudQUICAddress is the default cloud router. Self-hosted builds override it with
// -ldflags "-X github.com/xconnio/deskconn/common.cloudQUICAddress=host:port".
var cloudQUICAddress = "api.deskconn.com:8081" //nolint:gochecknoglobals // set at build time via -ldflags

func CloudQUICAddress() string {
	if v, ok := os.LookupEnv("DESKCONN_CLOUD_QUIC_ADDRESS"); ok {
		return v
	}
	return cloudQUICAddress
}

func CloudQUICTLSConfig() *tls.Config {
	host, _, err := net.SplitHostPort(CloudQUICAddress())
	if err == nil {
		switch host {
		case "0.0.0.0", "127.0.0.1", "::1", "localhost":
			return &tls.Config{InsecureSkipVerify: true} //nolint:gosec
		}
	}
	return nil
}
