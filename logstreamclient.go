package deskconn

import (
	"context"
	"fmt"
	"os"
	"sync"
	"time"
)

// RunLogs is the client entry point for `deskconn logs`. It validates since
// up front, opens one raw stream/channel, sends the one-time log
// request, then writes every received chunk straight to stdout until the
// device finishes or ctx is canceled.
func RunLogs(ctx context.Context, mode, realm, cfgDirectory, source string,
	follow bool, tailN int64, since string) error {
	if since != "" {
		if _, err := time.ParseDuration(since); err != nil {
			return fmt.Errorf("invalid since value %q: %w", since, err)
		}
	}

	switch mode {
	case modeP2P:
		p2pSess, err := ConnectDeviceRealmP2PSession(ctx, realm, cfgDirectory)
		if err != nil {
			return err
		}
		defer func() { _ = p2pSess.Close() }()
		return runLogsP2P(ctx, p2pSess, source, follow, tailN, since)
	case modeQUIC:
		return runLogsQUIC(ctx, realm, cfgDirectory, source, follow, tailN, since)
	default:
		p2pSess, err := ConnectDeviceRealmP2PSession(ctx, realm, cfgDirectory)
		if err == nil {
			defer func() { _ = p2pSess.Close() }()
			return runLogsP2P(ctx, p2pSess, source, follow, tailN, since)
		}
		fmt.Fprintln(os.Stderr, "p2p unavailable, falling back to quic")
		return runLogsQUIC(ctx, realm, cfgDirectory, source, follow, tailN, since)
	}
}

func runLogsQUIC(ctx context.Context, realm, cfgDirectory, source string,
	follow bool, tailN int64, since string) error {
	quicSess, err := ConnectDeviceRealmQUIC(ctx, realm, cfgDirectory)
	if err != nil {
		return err
	}
	defer func() { _ = quicSess.Connection().Close() }()

	stream, err := quicSess.OpenStream()
	if err != nil {
		return err
	}
	defer stream.Close()
	if err := WriteMsg(stream, RoutingFrame{Realm: realm, Op: FSOpLogs}); err != nil {
		return err
	}
	sendKey, receiveKey, err := QuicClientKeyExchange(stream)
	if err != nil {
		return err
	}

	env, err := buildPortEnvelope(logMsgControl,
		mustJSON(logControlMsg{Source: source, Follow: follow, TailN: tailN, Since: since}), sendKey)
	if err != nil {
		return err
	}
	if err := WriteFrame(stream, env); err != nil {
		return err
	}

	done := make(chan struct{})
	var doneOnce sync.Once
	closeAll := func() { doneOnce.Do(func() { close(done); _ = stream.Close() }) }
	defer closeAll()
	SafeGo(func() {
		select {
		case <-ctx.Done():
			closeAll()
		case <-done:
		}
	})

	SafeGo(func() {
		ticker := time.NewTicker(shellPingInterval)
		defer ticker.Stop()
		for {
			select {
			case <-done:
				return
			case <-ticker.C:
				env, err := buildPortEnvelope(logMsgPing, nil, sendKey)
				if err != nil {
					continue
				}
				if err := WriteFrame(stream, env); err != nil {
					closeAll()
					return
				}
			}
		}
	})

	for {
		_ = stream.SetReadDeadline(time.Now().Add(shellIdleTimeout))
		frame, err := ReadFrame(stream)
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			return nil
		}
		kind, plaintext, err := DecryptEnvelope(frame, receiveKey)
		if err != nil {
			continue
		}
		if kind == logMsgData {
			_, _ = os.Stdout.Write(plaintext)
		}
	}
}

func runLogsP2P(ctx context.Context, p2pSess P2PChannelOpener, source string,
	follow bool, tailN int64, since string) error {
	channel, err := openP2PChannel(p2pSess, LogChannelLabel)
	if err != nil {
		return err
	}
	defer channel.Close()
	closed, _ := WebrtcBackpressure(channel)
	sendKey, receiveKey, err := p2pClientKeyExchange(channel, closed)
	if err != nil {
		return err
	}
	conn := newP2PClientShellConn(channel, closed)

	env, err := buildPortEnvelope(logMsgControl,
		mustJSON(logControlMsg{Source: source, Follow: follow, TailN: tailN, Since: since}), sendKey)
	if err != nil {
		return err
	}
	if err := conn.sendEnvelope(env); err != nil {
		return err
	}

	SafeGo(func() {
		select {
		case <-ctx.Done():
			_ = channel.Close()
		case <-closed:
		}
	})

	SafeGo(func() {
		ticker := time.NewTicker(shellPingInterval)
		defer ticker.Stop()
		for {
			select {
			case <-closed:
				return
			case <-ticker.C:
				env, err := buildPortEnvelope(logMsgPing, nil, sendKey)
				if err != nil {
					continue
				}
				if err := conn.sendEnvelope(env); err != nil {
					return
				}
			}
		}
	})

	for {
		frame, err := RecvPriority(conn.msgCh, closed, shellIdleTimeout)
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			return nil
		}
		kind, plaintext, err := DecryptEnvelope(frame, receiveKey)
		if err != nil {
			continue
		}
		if kind == logMsgData {
			_, _ = os.Stdout.Write(plaintext)
		}
	}
}
