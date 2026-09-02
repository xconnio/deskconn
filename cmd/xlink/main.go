package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/signal"
	"path/filepath"
	"sync"
	"syscall"
	"time"

	"github.com/pion/webrtc/v4"
	log "github.com/sirupsen/logrus"

	"github.com/xconnio/deskconn"
	"github.com/xconnio/wampproto-go"
	"github.com/xconnio/wampproto-go/auth"
	"github.com/xconnio/wampproto-go/serializers"
	"github.com/xconnio/xconn-go"
	xconnwebrtc "github.com/xconnio/xconn-webrtc-go"
)

const (
	port = 18080

	xconnURIPrefix = "io.xconn."
)

func main() {
	cfgDirectory, err := deskconn.CfgDirectory()
	if err != nil {
		log.Fatal(err)
	}

	host, _ := os.Hostname()

	for runDeviceSession(cfgDirectory, host) {
	}
}

// runDeviceSession runs one connect/serve cycle: it sets up the local
// app-layer bridge, the LAN-facing realm, and the cloud reconnect loop,
// then blocks until either a shutdown signal or a detach event. It returns
// true if the caller should start another cycle (detach happened), false
// to shut down.
func runDeviceSession(cfgDirectory, host string) bool {
	cred, err := EnsureCredentials()
	if err != nil {
		log.Fatal(err)
	}

	machineIDStr, err := deskconn.MachineID()
	if err != nil {
		log.Fatalln("failed to read machine-id: ", err)
	}

	// appRouter/appSession serve deskconn.LocalRealm on deskconn.sock:
	// deskconnd and the CLI both dial in here, and appSession is the
	// in-memory session xlink uses to forward bridged calls (see
	// RegisterBridge).
	appRouter, err := xconn.NewRouter(xconn.DefaultRouterConfig())
	if err != nil {
		log.Fatalln(err)
	}
	if err := appRouter.AddRealm(deskconn.LocalRealm, &xconn.RealmConfig{
		AutoDiscloseCaller: true,
		Meta:               true,
		Roles: []xconn.RealmRole{{
			Name: "anonymous",
			Permissions: []xconn.Permission{{
				URI:            "",
				MatchPolicy:    wampproto.MatchPrefix,
				AllowCall:      true,
				AllowRegister:  true,
				AllowSubscribe: true,
			}},
		}},
	}); err != nil {
		log.Fatalln(err)
	}
	appServer := xconn.NewServer(appRouter, nil, &xconn.ServerConfig{})
	appListener, err := appServer.ListenAndServeRawSocket(xconn.NetworkUnix,
		filepath.Join(cfgDirectory, "deskconn.sock"))
	if err != nil {
		log.Fatalln(err)
	}
	defer appListener.Close()

	appSession, err := xconn.ConnectInMemory(appRouter, deskconn.LocalRealm)
	if err != nil {
		log.Fatal(err)
	}

	// xlinkStreamSock is where deskconnd listens for relayed raw streams.
	xlinkStreamSock := filepath.Join(cfgDirectory, "xlink-streams.sock")

	router, err := xconn.NewRouter(xconn.DefaultRouterConfig())
	if err != nil {
		log.Fatalln(err)
	}

	err = router.AddRealm(cred.Realm, &xconn.RealmConfig{
		AutoDiscloseCaller: true,
		Meta:               true,
		Roles: []xconn.RealmRole{
			{Name: "owner", Permissions: []xconn.Permission{
				{
					URI:         xconnURIPrefix,
					MatchPolicy: wampproto.MatchPrefix,
					AllowCall:   true,
				},
			}},
			{Name: "admin", Permissions: []xconn.Permission{
				{
					URI:         xconnURIPrefix,
					MatchPolicy: wampproto.MatchPrefix,
					AllowCall:   true,
				},
			}},
			{Name: "member", Permissions: []xconn.Permission{
				{
					URI:         xconnURIPrefix,
					MatchPolicy: wampproto.MatchPrefix,
					AllowCall:   true,
				},
			}},
		},
	})
	if err != nil {
		log.Fatalln(err)
	}

	principals, err := ReadPrincipalsFromFile()
	if err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			log.Fatal(err)
		}
	}

	authenticator := NewAuthenticator(principals)
	server := xconn.NewServer(router, authenticator, &xconn.ServerConfig{})
	listener, err := server.ListenAndServeWebSocket(xconn.NetworkTCP, "0.0.0.0:18080")
	if err != nil {
		log.Fatalln(err)
	}
	defer listener.Close()

	localSession, err := xconn.ConnectInMemory(router, cred.Realm)
	if err != nil {
		log.Fatal(err)
	}

	// Bridge deskconnd's app-layer procedures onto the LAN-facing realm --
	// any client reaching this device directly (mDNS discovery + WebSocket,
	// no cloud hop) gets the same procedures as a cloud caller.
	if err := RegisterBridge(localSession, appSession); err != nil {
		log.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	detachChan := make(chan struct{}, 1)

	var cloudConnMu sync.Mutex
	var activeDeviceSess, activeCloudSess *xconn.QUICSession

	deskconn.SafeGo(func() {
		retryDelay := 1 * time.Second
		maxDelay := 30 * time.Second
		for {
			select {
			case <-ctx.Done():
				return
			default:
			}

			cryptosignAuth, err := auth.NewCryptoSignAuthenticator(cred.AuthID, cred.PrivateKey, nil)
			if err != nil {
				log.Printf("failed to initialize cryptosign authenticator: %v", err)
				retryDelay = min(retryDelay*2, maxDelay)
				time.Sleep(retryDelay)
				continue
			}

			// Open the QUIC connection and the first WAMP session on the device realm.
			deviceSess, err := xconn.ConnectQUIC(ctx, deskconn.CloudQUICAddress(), cred.Realm,
				&xconn.QUICDialerConfig{Authenticator: cryptosignAuth, TLSConfig: deskconn.CloudQUICTLSConfig()})
			if err != nil {
				if err.Error() == "wamp.error.no_such_realm" {
					select {
					case detachChan <- struct{}{}:
					default:
					}
				}
				log.Printf("failed to connect to cloud, will retry in %v: %v", retryDelay, err)
				retryDelay = min(retryDelay*2, maxDelay)
				time.Sleep(retryDelay)
				continue
			}

			// Open a second WAMP session on the cloud realm over the same QUIC connection.
			cloudSess, err := deviceSess.OpenSession(ctx, deskconn.CloudRealm,
				&xconn.QUICDialerConfig{Authenticator: cryptosignAuth})
			if err != nil {
				log.Printf("failed to open cloud realm session, will retry in %v: %v", retryDelay, err)
				_ = deviceSess.Close()
				retryDelay = min(retryDelay*2, maxDelay)
				time.Sleep(retryDelay)
				continue
			}

			deviceSession := deviceSess.Session
			cloudSession := cloudSess.Session

			cloudConnMu.Lock()
			activeDeviceSess = deviceSess
			activeCloudSess = cloudSess
			cloudConnMu.Unlock()

			log.Println("connected to cloud")

			// Accept and classify streams relayed from CLI clients.
			deskconn.SafeGo(func() { acceptQUICStreams(deviceSess, xlinkStreamSock) })

			// Bridge deskconnd's app-layer procedures onto the cloud-facing realm.
			if err := RegisterBridge(deviceSession, appSession); err != nil {
				log.Printf("failed to register procedures on cloud, will retry in %v: %v", retryDelay, err)
				_ = deviceSess.Connection().Close()
				retryDelay = min(retryDelay*2, maxDelay)
				time.Sleep(retryDelay)
				continue
			}

			// Fetch and maintain authorized principals via the cloud realm session.
			callResp := cloudSession.Call(ProcedureListKeys).Do()
			if callResp.Err != nil {
				log.Println("failed to list keys:", callResp.Err)
				_ = deviceSess.Connection().Close()
				retryDelay = min(retryDelay*2, maxDelay)
				time.Sleep(retryDelay)
				continue
			}

			if len(callResp.Args()) == 0 {
				log.Println("unexpected response from list keys: no args")
				_ = deviceSess.Connection().Close()
				retryDelay = min(retryDelay*2, maxDelay)
				time.Sleep(retryDelay)
				continue
			}

			jsonData, err := json.MarshalIndent(callResp.Args()[0], "", "  ")
			if err != nil {
				log.Println(err)
				_ = deviceSess.Connection().Close()
				retryDelay = min(retryDelay*2, maxDelay)
				time.Sleep(retryDelay)
				continue
			}

			var cryptosignPrincipals []*CryptosignPrincipal
			if err = json.Unmarshal(jsonData, &cryptosignPrincipals); err != nil {
				log.Println(err)
				_ = deviceSess.Connection().Close()
				retryDelay = min(retryDelay*2, maxDelay)
				time.Sleep(retryDelay)
				continue
			}

			jsonData = append(jsonData, '\n')
			if err = os.WriteFile(filepath.Join(cfgDirectory, "principals.json"), jsonData, 0600); err != nil {
				log.Println(err)
			}

			authenticator.SetPrincipals(cryptosignPrincipals)
			if err := authenticator.SubscribeEvents(cloudSession, machineIDStr); err != nil {
				log.Println(err)
			}

			subResp := cloudSession.Subscribe(fmt.Sprintf(deskconn.TopicDeskconnDesktopDetachFormat, machineIDStr),
				func(event *xconn.Event) {
					select {
					case detachChan <- struct{}{}:
					default:
					}
				}).Do()
			if subResp.Err != nil {
				log.Println(subResp.Err)
			}

			webRtcManager := xconnwebrtc.NewWebRTCHandler()
			cfg := &xconnwebrtc.ProviderConfig{
				Session:                     deviceSession,
				ProcedureHandleOffer:        deskconn.ProcedureWebRTCOffer,
				TopicHandleRemoteCandidates: deskconn.TopicAnswererOnCandidate,
				TopicPublishLocalCandidate:  deskconn.TopicOffererOnCandidate,
				Serializer:                  &serializers.CBORSerializer{},
				Authenticator:               authenticator,
				Router:                      router,
				ICEServers: []xconnwebrtc.ICEServer{
					{URLs: []string{deskconn.StunServerURL}},
				},
			}
			if err := webRtcManager.Setup(cfg); err != nil {
				log.Printf("failed to setup webRtc provider, will retry in %v: %v", retryDelay, err)
				_ = deviceSess.Connection().Close()
				retryDelay = min(retryDelay*2, maxDelay)
				time.Sleep(retryDelay)
				continue
			}

			webRtcManager.OnDataChannel(handleAuxDataChannel(xlinkStreamSock))

			// Reset backoff after successful connection.
			retryDelay = 1 * time.Second

			// Both sessions share the QUIC connection; either ending means reconnect.
			select {
			case <-deviceSession.Done():
			case <-cloudSession.Done():
			}

			cloudConnMu.Lock()
			activeDeviceSess = nil
			activeCloudSess = nil
			cloudConnMu.Unlock()

			_ = deviceSess.Connection().Close()
			log.Println("disconnected from cloud, retrying...")
		}
	})

	zeroconfServer, err := deskconn.AdvertiseService(host, port, cred.Realm)
	if err != nil {
		log.Fatal(err)
	}
	defer zeroconfServer.Shutdown()

	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, os.Interrupt, syscall.SIGTERM)
	defer signal.Stop(sigChan)

	select {
	case <-sigChan:
		cancel()

		cloudConnMu.Lock()
		if activeCloudSess != nil {
			_ = activeCloudSess.Close()
		}
		if activeDeviceSess != nil {
			_ = activeDeviceSess.Close()
			_ = activeDeviceSess.Connection().Close()
		}
		cloudConnMu.Unlock()

		router.Close()
		appRouter.Close()
		return false
	case <-detachChan:
		cancel()
		_ = os.Remove(filepath.Join(cfgDirectory, "credentials.json"))

		cloudConnMu.Lock()
		if activeCloudSess != nil {
			_ = activeCloudSess.Close()
		}
		if activeDeviceSess != nil {
			_ = activeDeviceSess.Close()
			_ = activeDeviceSess.Connection().Close()
		}
		cloudConnMu.Unlock()

		router.Close()
		appRouter.Close()
		return true
	}
}

// acceptQUICStreams runs an accept loop on sess, relaying each
// server-initiated stream to deskconnd over xlinkStreamSock.
func acceptQUICStreams(sess *xconn.QUICSession, xlinkStreamSock string) {
	for {
		stream, err := sess.AcceptStream()
		if err != nil {
			return
		}
		deskconn.SafeGo(func() { relayQUICStream(stream, xlinkStreamSock) })
	}
}

// relayQUICStream classifies a freshly accepted QUIC stream and splices its
// remaining bytes, untouched, to deskconnd's stream-relay listener.
func relayQUICStream(stream net.Conn, xlinkStreamSock string) {
	defer stream.Close()

	op, err := deskconn.ReadStreamOp(stream)
	if err != nil {
		return
	}

	conn, err := net.Dial("unix", xlinkStreamSock)
	if err != nil {
		log.Printf("relay: failed to dial deskconnd stream socket: %v", err)
		return
	}
	defer conn.Close()

	if err := deskconn.WriteRelayHeader(conn, deskconn.RelayHeader{Kind: deskconn.RelayKindQUIC, Op: op}); err != nil {
		return
	}

	done := make(chan struct{})
	deskconn.SafeGo(func() {
		_, _ = io.Copy(conn, stream)
		close(done)
	})
	_, _ = io.Copy(stream, conn)
	<-done
}

// handleAuxDataChannel is the callback wired to the WebRTC provider's
// OnDataChannel; it classifies each channel by label (or, for VPN/file-
// stream channels, by sniffing the first message) and relays it to
// deskconnd over xlinkStreamSock -- xlink only needs to know enough
// about a channel to route it, never its payload.
//
// The provider invokes this synchronously from the channel's own message
// dispatch goroutine (it has to: it's the one sniffing the first message),
// so the actual relay work must happen on its own goroutine -- RelayWebRTCChannel
// blocks until the channel closes, and until this callback returns, the
// provider can't dispatch this channel's next message to the OnMessage
// handler RelayWebRTCChannel registers.
func handleAuxDataChannel(xlinkStreamSock string) func(sessionID string, channel *webrtc.DataChannel,
	firstMessage []byte) {
	return func(_ string, channel *webrtc.DataChannel, firstMessage []byte) {
		deskconn.SafeGo(func() {
			var label string
			ordered := true
			relayFirstMessage := true

			switch channel.Label() {
			case deskconn.ShellChannelLabel, deskconn.PortForwardChannelLabel, deskconn.PortReverseChannelLabel,
				deskconn.AgentForwardChannelLabel, deskconn.LogChannelLabel:
				label = channel.Label()
			default:
				var probe deskconn.VPNOpenFrame
				if json.Unmarshal(firstMessage, &probe) == nil && probe.Type == deskconn.VPNFrameOpen {
					label = deskconn.VPNChannelLabel
					ordered = false
					relayFirstMessage = false
				}
				// else: label stays "" (file-stream), the default case in deskconnd's dispatch.
			}

			conn, err := net.Dial("unix", xlinkStreamSock)
			if err != nil {
				log.Printf("relay: failed to dial deskconnd stream socket: %v", err)
				_ = channel.Close()
				return
			}

			header := deskconn.RelayHeader{Kind: deskconn.RelayKindWebRTC, Label: label, Ordered: ordered}
			if err := deskconn.WriteRelayHeader(conn, header); err != nil {
				_ = channel.Close()
				_ = conn.Close()
				return
			}

			var fm []byte
			if relayFirstMessage {
				fm = firstMessage
			}
			deskconn.RelayWebRTCChannel(channel, conn, fm)
		})
	}
}
