package xlink

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	log "github.com/sirupsen/logrus"

	"github.com/xconnio/deskconn/common"
	"github.com/xconnio/wampproto-go/auth"
	"github.com/xconnio/xconn-go"
)

// lanPort is where the device realm is served to clients on the local network.
const lanPort = 18080

// Run runs xlink for the cloud-attached device until ctx is done: it serves the device
// realm on the LAN and to the cloud, first waiting for the device to be attached and
// starting over after a detach. appSession is the in-memory session on the app layer
// (see StartAppLayer) whose procedures get bridged onto the device realm.
func Run(ctx context.Context, cfgDirectory string, appSession *xconn.Session) {
	host, _ := os.Hostname()
	for runDeviceSession(ctx, cfgDirectory, host, appSession) {
	}
}

// runDeviceSession runs one connect/serve cycle: it bridges appSession onto the
// LAN-facing realm and runs the cloud reconnect loop, then blocks until either
// parent is done or a detach event. It returns true if the caller should start
// another cycle (detach happened), false to shut down.
func runDeviceSession(parent context.Context, cfgDirectory, host string, appSession *xconn.Session) bool {
	cred, err := EnsureCredentials(parent)
	if err != nil {
		if parent.Err() != nil {
			return false
		}
		log.Fatal(err)
	}

	machineIDStr, err := common.MachineID()
	if err != nil {
		log.Fatalln("failed to read machine-id: ", err)
	}

	// xlinkStreamSock is where deskconnd listens for relayed raw streams.
	xlinkStreamSock := filepath.Join(cfgDirectory, "xlink-streams.sock")

	router := NewDeviceRouter(cred.Realm)

	principals, err := ReadPrincipalsFromFile()
	if err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			log.Fatal(err)
		}
	}

	authenticator := NewAuthenticator(principals)
	server := xconn.NewServer(router, authenticator, &xconn.ServerConfig{})
	listener, err := server.ListenAndServeWebSocket(xconn.NetworkTCP, fmt.Sprintf("0.0.0.0:%d", lanPort))
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

	ctx, cancel := context.WithCancel(parent)
	defer cancel()

	detachChan := make(chan struct{}, 1)

	var cloudConnMu sync.Mutex
	var activeDeviceSess, activeCloudSess *xconn.QUICSession

	common.SafeGo(func() {
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
			deviceSess, err := xconn.ConnectQUIC(ctx, common.CloudQUICAddress(), cred.Realm,
				&xconn.QUICDialerConfig{Authenticator: cryptosignAuth, TLSConfig: common.CloudQUICTLSConfig()})
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
			cloudSess, err := deviceSess.OpenSession(ctx, common.CloudRealm,
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
			common.SafeGo(func() { acceptQUICStreams(deviceSess, xlinkStreamSock) })

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

			subResp := cloudSession.Subscribe(fmt.Sprintf(common.TopicDeskconnDesktopDetachFormat, machineIDStr),
				func(event *xconn.Event) {
					select {
					case detachChan <- struct{}{}:
					default:
					}
				}).Do()
			if subResp.Err != nil {
				log.Println(subResp.Err)
			}

			if err := SetupWebRTC(deviceSession, router, authenticator, xlinkStreamSock); err != nil {
				log.Printf("failed to setup webRtc provider, will retry in %v: %v", retryDelay, err)
				_ = deviceSess.Connection().Close()
				retryDelay = min(retryDelay*2, maxDelay)
				time.Sleep(retryDelay)
				continue
			}

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

	zeroconfServer, err := AdvertiseService(host, lanPort, cred.Realm)
	if err != nil {
		log.Fatal(err)
	}
	defer zeroconfServer.Shutdown()

	select {
	case <-parent.Done():
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
		common.SafeGo(func() { RelayQUICStream(stream, xlinkStreamSock) })
	}
}
