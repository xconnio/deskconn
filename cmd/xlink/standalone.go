package main

import (
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"encoding/pem"
	"errors"
	"fmt"
	"net"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"

	log "github.com/sirupsen/logrus"
	"gopkg.in/yaml.v3"

	"github.com/xconnio/deskconn"
	"github.com/xconnio/xconn-go"
)

const (
	standaloneDefaultListen = "0.0.0.0:18080"
	standaloneAuthRole      = "owner"
)

// loadStandaloneConfig reads the standalone section of config.yml and applies the
// DESKCONN_STANDALONE (bool), DESKCONN_STANDALONE_LISTEN (host:port) and
// DESKCONN_STANDALONE_KEYS (authid:pubkey,...) environment variables on top.
func loadStandaloneConfig(cfgDirectory string) (*deskconn.StandaloneConfig, error) {
	var config deskconn.Config
	data, err := os.ReadFile(filepath.Join(cfgDirectory, "config.yml"))
	if err != nil && !os.IsNotExist(err) {
		return nil, err
	}
	if err := yaml.Unmarshal(data, &config); err != nil {
		return nil, fmt.Errorf("failed to parse config: %w", err)
	}
	standalone := config.Standalone

	if v, ok := os.LookupEnv("DESKCONN_STANDALONE"); ok {
		if standalone.Enabled, err = strconv.ParseBool(v); err != nil {
			return nil, fmt.Errorf("invalid DESKCONN_STANDALONE %q: %w", v, err)
		}
	}
	if v := os.Getenv("DESKCONN_STANDALONE_LISTEN"); v != "" {
		standalone.Listen = v
	}
	if standalone.Listen == "" {
		standalone.Listen = standaloneDefaultListen
	}
	if v := os.Getenv("DESKCONN_STANDALONE_KEYS"); v != "" {
		for entry := range strings.SplitSeq(v, ",") {
			authid, key, ok := strings.Cut(strings.TrimSpace(entry), ":")
			if !ok || authid == "" || key == "" {
				return nil, fmt.Errorf("invalid DESKCONN_STANDALONE_KEYS entry %q, want authid:pubkey", entry)
			}
			standalone.Principals = append(standalone.Principals,
				deskconn.StandalonePrincipal{AuthID: authid, AuthorizedKeys: []string{key}})
		}
	}

	return &standalone, nil
}

// standalonePrincipals converts the configured keys to authenticator principals,
// merging entries that share an authid.
func standalonePrincipals(config *deskconn.StandaloneConfig) []*CryptosignPrincipal {
	byAuthID := make(map[string]*CryptosignPrincipal)
	var principals []*CryptosignPrincipal
	for _, p := range config.Principals {
		principal, ok := byAuthID[p.AuthID]
		if !ok {
			principal = &CryptosignPrincipal{AuthID: p.AuthID, AuthRole: standaloneAuthRole}
			byAuthID[p.AuthID] = principal
			principals = append(principals, principal)
		}
		principal.AuthorizedKeys = append(principal.AuthorizedKeys, p.AuthorizedKeys...)
	}
	return principals
}

// loadOrCreateCert loads standalone mode's self-signed certificate, generating and
// saving one on first run so its fingerprint, which clients pin, survives restarts.
func loadOrCreateCert(cfgDirectory string) (tls.Certificate, error) {
	certPath := filepath.Join(cfgDirectory, "standalone.crt")
	keyPath := filepath.Join(cfgDirectory, "standalone.key")

	cert, err := tls.LoadX509KeyPair(certPath, keyPath)
	if !errors.Is(err, os.ErrNotExist) {
		return cert, err
	}

	tlsConfig, err := xconn.GenerateSelfSignedTLSConfig()
	if err != nil {
		return tls.Certificate{}, err
	}
	cert = tlsConfig.Certificates[0]

	keyDER, err := x509.MarshalPKCS8PrivateKey(cert.PrivateKey)
	if err != nil {
		return tls.Certificate{}, err
	}
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER})
	if err := os.WriteFile(keyPath, keyPEM, 0600); err != nil {
		return tls.Certificate{}, err
	}
	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: cert.Certificate[0]})
	if err := os.WriteFile(certPath, certPEM, 0600); err != nil {
		return tls.Certificate{}, err
	}

	return cert, nil
}

func certFingerprint(cert tls.Certificate) string {
	sum := sha256.Sum256(cert.Certificate[0])
	return "sha256:" + hex.EncodeToString(sum[:])
}

// runStandalone serves the device realm directly over QUIC to the configured keys,
// with no cloud account, and blocks until SIGINT/SIGTERM.
func runStandalone(cfgDirectory string, config *deskconn.StandaloneConfig) {
	listener, cert, stop, err := startStandalone(cfgDirectory, config)
	if err != nil {
		log.Fatalln(err)
	}
	defer stop()

	fingerprint := certFingerprint(cert)
	log.Printf("standalone mode: listening on %s/udp (realm %s)", listener.Addr(), deskconn.StandaloneRealm)
	log.Printf("certificate fingerprint: %s", fingerprint)
	_, port, _ := net.SplitHostPort(listener.Addr().String())
	log.Printf("add on a client with: deskconn device add <name> <public-ip>:%s --fingerprint %s",
		port, fingerprint)

	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, os.Interrupt, syscall.SIGTERM)
	<-sigChan
}

// startStandalone starts the app-layer bridge and the QUIC listener, relaying
// authenticated clients' raw streams to deskconnd. stop shuts everything down.
func startStandalone(cfgDirectory string, config *deskconn.StandaloneConfig) (*xconn.QUICListener,
	tls.Certificate, func(), error) {
	cert, err := loadOrCreateCert(cfgDirectory)
	if err != nil {
		return nil, tls.Certificate{}, nil, fmt.Errorf("failed to load standalone certificate: %w", err)
	}

	principals := standalonePrincipals(config)
	if len(principals) == 0 {
		log.Warnln("standalone mode has no keys configured; no client can connect")
	}

	router := newDeviceRouter(deskconn.StandaloneRealm)
	server := xconn.NewServer(router, NewAuthenticator(principals), &xconn.ServerConfig{})
	listener, err := server.ListenAndServeQUIC(config.Listen, &tls.Config{Certificates: []tls.Certificate{cert}})
	if err != nil {
		router.Close()
		return nil, tls.Certificate{}, nil, err
	}

	appRouter, appListener, appSession := startAppLayer(cfgDirectory)
	stop := func() {
		_ = listener.Close()
		router.Close()
		_ = appListener.Close()
		appRouter.Close()
	}

	localSession, err := xconn.ConnectInMemory(router, deskconn.StandaloneRealm)
	if err != nil {
		stop()
		return nil, tls.Certificate{}, nil, err
	}
	if err := RegisterBridge(localSession, appSession); err != nil {
		stop()
		return nil, tls.Certificate{}, nil, err
	}

	xlinkStreamSock := filepath.Join(cfgDirectory, "xlink-streams.sock")
	deskconn.SafeGo(func() {
		for range listener.AcceptSession() {
		}
	})
	deskconn.SafeGo(func() {
		for stream := range listener.AcceptStream() {
			deskconn.SafeGo(func() { relayQUICStream(stream.Conn, xlinkStreamSock) })
		}
	})

	return listener, cert, stop, nil
}
