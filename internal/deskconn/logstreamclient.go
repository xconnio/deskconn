package deskconn

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"sync"
	"time"

	"github.com/xconnio/deskconn/internal/common"
)

// RunLogs is the client entry point for `desk machine logs`. It validates since
// up front, opens one RPC-associated duplex stream, sends the one-time log
// request, then writes every received chunk straight to out until the
// machine finishes or ctx is canceled. mode picks "routed"/"direct" itself, or
// "" uses userd's persistent connection, falling back (without userd)
// to trying P2P first, then QUIC.
func RunLogs(ctx context.Context, mode, realm, cfgDirectory, source string,
	follow bool, tailN int64, since string, out io.Writer) error {
	if since != "" {
		if _, err := time.ParseDuration(since); err != nil {
			return fmt.Errorf("invalid since value %q: %w", since, err)
		}
	}

	switch mode {
	case ModeDirect:
		p2pSess, err := ConnectMachineRealmP2PSession(ctx, realm, cfgDirectory)
		if err != nil {
			return err
		}
		defer func() { _ = p2pSess.Close() }()
		return runLogsOnStream(ctx, p2pSess, source, follow, tailN, since, out)
	case ModeRouted:
		return runLogsQUIC(ctx, realm, cfgDirectory, source, follow, tailN, since, out)
	default:
		daemon, err := DialDaemonStreams(ctx, realm, cfgDirectory)
		if err == nil {
			defer func() { _ = daemon.Close() }()
			return runLogsOnStream(ctx, daemon, source, follow, tailN, since, out)
		}
		if !errors.Is(err, ErrDaemonUnavailable) {
			return err
		}
		p2pSess, err := ConnectMachineRealmP2PSession(ctx, realm, cfgDirectory)
		if err == nil {
			defer func() { _ = p2pSess.Close() }()
			return runLogsOnStream(ctx, p2pSess, source, follow, tailN, since, out)
		}
		fmt.Fprintln(os.Stderr, "direct connection unavailable, falling back to routed")
		return runLogsQUIC(ctx, realm, cfgDirectory, source, follow, tailN, since, out)
	}
}

func runLogsQUIC(ctx context.Context, realm, cfgDirectory, source string,
	follow bool, tailN int64, since string, out io.Writer) error {
	quicSess, err := common.ConnectMachineRealmQUIC(ctx, realm, cfgDirectory)
	if err != nil {
		return err
	}
	defer func() { _ = quicSess.Connection().Close() }()
	return runLogsOnStream(ctx, quicSess, source, follow, tailN, since, out)
}

// runLogsOnStream runs the log request on an RPC stream returned by sess.
func runLogsOnStream(ctx context.Context, sess common.StreamSession, source string,
	follow bool, tailN int64, since string, out io.Writer) error {
	stream, err := common.OpenRPCStream(ctx, sess, common.ProcedureLogsStream, false)
	if err != nil {
		return err
	}
	defer stream.Close()

	sendKey, receiveKey, err := StreamClientKeyExchange(stream)
	if err != nil {
		return err
	}

	env, err := common.BuildPortEnvelope(common.LogMsgControl,
		common.MustJSON(common.LogControlMsg{Source: source, Follow: follow, TailN: tailN, Since: since}), sendKey)
	if err != nil {
		return err
	}
	if err := common.WriteFrame(stream, env); err != nil {
		return err
	}

	done := make(chan struct{})
	var doneOnce sync.Once
	closeAll := func() { doneOnce.Do(func() { close(done); _ = stream.Close() }) }
	defer closeAll()
	common.SafeGo(func() {
		select {
		case <-ctx.Done():
			closeAll()
		case <-done:
		}
	})

	common.SafeGo(func() {
		ticker := time.NewTicker(common.ShellPingInterval)
		defer ticker.Stop()
		for {
			select {
			case <-done:
				return
			case <-ticker.C:
				env, err := common.BuildPortEnvelope(common.LogMsgPing, nil, sendKey)
				if err != nil {
					continue
				}
				if err := common.WriteFrame(stream, env); err != nil {
					closeAll()
					return
				}
			}
		}
	})

	for {
		_ = stream.SetReadDeadline(time.Now().Add(common.ShellIdleTimeout))
		frame, err := common.ReadFrame(stream)
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			return nil
		}
		kind, plaintext, err := common.DecryptEnvelope(frame, receiveKey)
		if err != nil {
			continue
		}
		if kind == common.LogMsgData {
			_, _ = out.Write(plaintext)
		}
	}
}
