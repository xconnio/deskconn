// Client interoperability fixture: real DeskConn handlers behind a yamux bridge.
package main

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"math/big"
	"os"
	"path/filepath"
	"time"

	"github.com/xconnio/deskconn/internal/connectivity"
	"github.com/xconnio/deskconn/internal/userd"
	api "github.com/xconnio/xconn-core/bindings/go"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func certificate() (tls.Certificate, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return tls.Certificate{}, err
	}
	template := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "localhost"},
		DNSNames: []string{"localhost"}, NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(7 * 24 * time.Hour),
		KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		return tls.Certificate{}, err
	}
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}, nil
}

func run() error {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	root, err := os.MkdirTemp("", "deskconn-client-peer-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(root)
	local, listener, target, err := connectivity.StartAppLayer("local", filepath.Join(root, "app.sock"))
	if err != nil {
		return err
	}
	defer local.Close()
	defer listener.Close()
	defer target.Close()
	application, err := connectivity.ConnectApp(ctx, filepath.Join(root, "app.sock"), "local")
	if err != nil {
		return err
	}
	defer application.Close()
	backend := userd.NewDeskconn(false, root)
	defer backend.CloseStreams()
	if err = backend.Register(application); err != nil {
		return err
	}
	router, err := api.NewRouter(nil)
	if err != nil {
		return err
	}
	defer router.Close()
	if err = router.AddRealm("machine", &api.RealmConfig{AutoDiscloseCaller: true,
		Roles: []api.RealmRole{{Name: "anonymous", Permissions: []api.Permission{
			{URI: "io.xconn.deskconn.", MatchPolicy: "prefix", AllowCall: true},
			{URI: "offer", AllowCall: true},
			{URI: "to-provider", AllowPublish: true, AllowSubscribe: true},
			{URI: "to-client", AllowPublish: true, AllowSubscribe: true},
		}}}}); err != nil {
		return err
	}
	host, err := api.ConnectInMemory(router, "machine")
	if err != nil {
		return err
	}
	defer host.Close()
	bridge, err := host.Bridge(ctx, api.BridgeConfig{Target: target, Routes: []api.BridgeRoute{{
		Procedure: "io.xconn.deskconn.", Options: api.Options{"match": "prefix"},
	}}})
	if err != nil {
		return err
	}
	defer bridge.Close()
	quic, err := router.ListenQUIC(ctx, "127.0.0.1:0")
	if err != nil {
		return err
	}
	defer quic.Close()
	cert, err := certificate()
	if err != nil {
		return err
	}
	server := api.NewServer(router, nil, nil)
	defer server.Close()
	wt, err := server.ListenWebTransport(ctx, "127.0.0.1:0", "/wamp", cert)
	if err != nil {
		return err
	}
	defer wt.Close()
	provider := api.NewWebRTCHandler()
	if err = provider.SetupContext(ctx, &api.WebRTCProviderConfig{Session: host, Router: router,
		ProcedureHandleOffer: "offer", TopicHandleRemoteCandidates: "to-provider",
		TopicPublishLocalCandidate: "to-client"}); err != nil {
		return err
	}
	defer provider.Close()
	path := filepath.Join(root, "binary.dat")
	if err = os.WriteFile(path, bytes.Repeat([]byte{0, 255, 42, 128}, 350_000), 0600); err != nil {
		return err
	}
	hash := sha256.Sum256(cert.Certificate[0])
	if err = json.NewEncoder(os.Stdout).Encode(map[string]any{
		"webtransport": "https://" + wt.Address + "/wamp", "hash": base64.StdEncoding.EncodeToString(hash[:]),
		"quic":        quic.Address,
		"certificate": string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: quic.Certificate})),
		"realm":       "machine", "source": path, "directory": root,
	}); err != nil {
		return err
	}
	_, err = io.Copy(io.Discard, os.Stdin)
	return err
}
