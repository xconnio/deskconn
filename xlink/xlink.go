package xlink

import (
	"encoding/json"
	"io"
	"net"
	"path/filepath"

	"github.com/pion/webrtc/v4"
	log "github.com/sirupsen/logrus"

	"github.com/xconnio/deskconn/common"
	"github.com/xconnio/wampproto-go"
	"github.com/xconnio/wampproto-go/serializers"
	"github.com/xconnio/xconn-go"
	xconnwebrtc "github.com/xconnio/xconn-webrtc-go"
)

const (
	xconnURIPrefix  = "io.xconn."
	webrtcURIPrefix = "io.xconn.webrtc."
)

// StartAppLayer serves deskconn.LocalRealm on deskconn.sock: deskconnd and
// the CLI both dial in here, and the returned in-memory session is what xlink
// uses to forward bridged calls (see RegisterBridge).
func StartAppLayer(cfgDirectory string) (*xconn.Router, *xconn.Listener, *xconn.Session) {
	appRouter, err := xconn.NewRouter(xconn.DefaultRouterConfig())
	if err != nil {
		log.Fatalln(err)
	}
	if err := appRouter.AddRealm(common.LocalRealm, &xconn.RealmConfig{
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

	appSession, err := xconn.ConnectInMemory(appRouter, common.LocalRealm)
	if err != nil {
		log.Fatal(err)
	}
	return appRouter, appListener, appSession
}

// SetupWebRTC answers WebRTC offers made on session (signaling for P2P), attaching the
// resulting WAMP-over-WebRTC sessions to router and relaying their raw channels to deskconnd.
func SetupWebRTC(session *xconn.Session, router *xconn.Router, authenticator *Authenticator,
	xlinkStreamSock string) error {
	webRtcManager := xconnwebrtc.NewWebRTCHandler()
	if err := webRtcManager.Setup(&xconnwebrtc.ProviderConfig{
		Session:                     session,
		ProcedureHandleOffer:        common.ProcedureWebRTCOffer,
		TopicHandleRemoteCandidates: common.TopicAnswererOnCandidate,
		TopicPublishLocalCandidate:  common.TopicOffererOnCandidate,
		Serializer:                  &serializers.CBORSerializer{},
		Authenticator:               authenticator,
		Router:                      router,
		ICEServers: []xconnwebrtc.ICEServer{
			{URLs: []string{common.StunServerURL}},
		},
	}); err != nil {
		return err
	}

	webRtcManager.OnDataChannel(handleAuxDataChannel(xlinkStreamSock))
	return nil
}

// NewDeviceRouter returns a router serving the device-facing realm that
// remote clients (cloud, LAN or standalone) call into.
func NewDeviceRouter(realm string) *xconn.Router {
	router, err := xconn.NewRouter(xconn.DefaultRouterConfig())
	if err != nil {
		log.Fatalln(err)
	}

	permissions := []xconn.Permission{
		{
			URI:         xconnURIPrefix,
			MatchPolicy: wampproto.MatchPrefix,
			AllowCall:   true,
		},
		// WebRTC signaling, for clients that reach this router directly (standalone mode).
		{
			URI:            webrtcURIPrefix,
			MatchPolicy:    wampproto.MatchPrefix,
			AllowPublish:   true,
			AllowSubscribe: true,
		},
	}
	err = router.AddRealm(realm, &xconn.RealmConfig{
		AutoDiscloseCaller: true,
		Meta:               true,
		Roles: []xconn.RealmRole{
			{Name: "owner", Permissions: permissions},
			{Name: "admin", Permissions: permissions},
			{Name: "member", Permissions: permissions},
		},
	})
	if err != nil {
		log.Fatalln(err)
	}
	return router
}

// RelayQUICStream classifies a freshly accepted QUIC stream and splices its
// remaining bytes, untouched, to deskconnd's stream-relay listener.
func RelayQUICStream(stream net.Conn, xlinkStreamSock string) {
	defer stream.Close()

	op, err := ReadStreamOp(stream)
	if err != nil {
		return
	}

	conn, err := net.Dial("unix", xlinkStreamSock)
	if err != nil {
		log.Printf("relay: failed to dial deskconnd stream socket: %v", err)
		return
	}
	defer conn.Close()

	if err := WriteRelayHeader(conn, common.RelayHeader{Kind: common.RelayKindQUIC, Op: op}); err != nil {
		return
	}

	done := make(chan struct{})
	common.SafeGo(func() {
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
		common.SafeGo(func() {
			var label string
			ordered := true
			relayFirstMessage := true

			switch channel.Label() {
			case common.ShellChannelLabel, common.PortForwardChannelLabel, common.PortReverseChannelLabel,
				common.AgentForwardChannelLabel, common.LogChannelLabel:
				label = channel.Label()
			default:
				var probe common.VPNOpenFrame
				if json.Unmarshal(firstMessage, &probe) == nil && probe.Type == common.VPNFrameOpen {
					label = common.VPNChannelLabel
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

			header := common.RelayHeader{Kind: common.RelayKindWebRTC, Label: label, Ordered: ordered}
			if err := WriteRelayHeader(conn, header); err != nil {
				_ = channel.Close()
				_ = conn.Close()
				return
			}

			var fm []byte
			if relayFirstMessage {
				fm = firstMessage
			}
			RelayWebRTCChannel(channel, conn, fm)
		})
	}
}
