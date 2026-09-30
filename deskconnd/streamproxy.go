package deskconnd

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"time"

	log "github.com/sirupsen/logrus"

	"github.com/xconnio/deskconn/common"
)

// ServeStreamProxy serves the CLI's raw streams on the persistent device connections:
// see common.StreamProxyRequest.
func ServeStreamProxy(ln net.Listener, clientSessions *ClientSessions, cfgDirectory string) {
	for {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		common.SafeGo(func() { handleStreamProxyConn(conn, clientSessions, cfgDirectory) })
	}
}

func handleStreamProxyConn(conn net.Conn, clientSessions *ClientSessions, cfgDirectory string) {
	defer conn.Close()
	if unixConn, ok := conn.(*net.UnixConn); ok {
		_ = unixConn.SetWriteBuffer(common.StreamProxyDownstreamBuffer)
	}

	_ = conn.SetDeadline(time.Now().Add(common.StreamProxyConnectTimeout))
	var req common.StreamProxyRequest
	if err := common.ReadMsg(conn, &req); err != nil {
		return
	}
	reply := func(kind common.RelayKind) bool {
		err := common.WriteMsg(conn, common.StreamProxyResponse{Kind: kind})
		_ = conn.SetDeadline(time.Time{})
		return err == nil
	}
	fail := func(err error) {
		log.Debugf("stream proxy %s: %v", req.Realm, err)
		_ = common.WriteMsg(conn, common.StreamProxyResponse{Error: err.Error()})
	}

	ctx, cancel := context.WithTimeout(context.Background(), common.StreamProxyConnectTimeout)
	defer cancel()
	quic, rtc, err := clientSessions.deviceTransports(ctx, req.Realm, cfgDirectory)
	if err != nil {
		fail(err)
		return
	}
	errNoQUIC := errors.New("the connection to the device no longer uses QUIC")

	switch req.Kind {
	case "":
		// P2P whenever the connection has it. Otherwise QUIC, held open for the CLI until
		// it closes this connection, so its operation can finish on QUIC even if the
		// upgrade to P2P lands meanwhile.
		if rtc != nil {
			reply(common.RelayKindWebRTC)
			return
		}
		if quic == nil || !quic.acquire() {
			fail(errNoQUIC)
			return
		}
		defer quic.release()
		if reply(common.RelayKindQUIC) {
			_, _ = io.Copy(io.Discard, conn)
		}

	case common.RelayKindQUIC:
		if quic == nil || !quic.acquire() {
			fail(errNoQUIC)
			return
		}
		defer quic.release()
		stream, err := quic.OpenStream()
		if err != nil {
			fail(err)
			return
		}
		if !reply(req.Kind) {
			_ = stream.Close()
			return
		}
		common.SpliceConns(conn, stream)

	case common.RelayKindWebRTC:
		if rtc == nil {
			fail(errors.New("the connection to the device is no longer P2P"))
			return
		}
		channel, err := common.OpenDataChannel(rtc, req.Label)
		if err != nil {
			fail(err)
			return
		}
		if !reply(req.Kind) {
			_ = channel.Close()
			return
		}
		common.RelayWebRTCChannelBuffered(channel, conn, nil, common.StreamProxyBufferedHigh,
			common.StreamProxyBufferedLow)

	default:
		fail(fmt.Errorf("unknown stream kind %q", req.Kind))
	}
}
