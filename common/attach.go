package common

import (
	"crypto/tls"
	"net"
	"os"
)

func CloudQUICAddress() string {
	if v, ok := os.LookupEnv("DESKCONN_CLOUD_QUIC_ADDRESS"); ok {
		return v
	}
	return "api.deskconn.com:8081"
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

type Credentials struct {
	Realm      string `json:"realm"`
	AuthID     string `json:"authid"`
	PublicKey  string `json:"public_key"`
	PrivateKey string `json:"private_key"` // #nosec
}
