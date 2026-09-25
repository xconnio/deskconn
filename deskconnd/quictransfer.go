package deskconnd

import (
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"time"

	"github.com/xconnio/deskconn/common"
)

// DispatchQUICOp resumes handling a QUIC stream that deskconnd has already
// classified and relayed locally: op is the routingFrame.Op deskconnd read
// before relaying (see ReadStreamOp), and stream is everything
// after that routing frame, untouched -- exactly what deskconnd's old,
// single-process HandleQUICStream saw right after its own read of it.
func (d *Deskconn) DispatchQUICOp(op common.FSOp, stream net.Conn) {
	defer stream.Close()

	switch op {
	case common.FSOpShell:
		d.handleQUICShellStream(stream)
		return
	case common.FSOpPortForward:
		d.handleQUICPortForwardStream(stream)
		return
	case common.FSOpPortReverse:
		d.handleQUICPortReverseStream(stream)
		return
	case common.FSOpAgentForward:
		d.handleQUICAgentForwardStream(stream)
		return
	case common.FSOpLogs:
		d.handleQUICLogsStream(stream)
		return
	}

	sendKey, receiveKey, err := quicServerKeyExchange(stream)
	if err != nil {
		return
	}

	var req common.FSRequest
	if err := common.ReadEncryptedMsg(stream, &req, receiveKey); err != nil {
		return
	}

	switch req.Op {
	case common.FSOpList:
		quicServeList(stream, req, sendKey)
	case common.FSOpInit:
		quicServeInit(stream, req, sendKey)
	case common.FSOpRead, common.FSOpWrite:
		quicServeSession(stream, req, sendKey, receiveKey)
	}
}

// quicServerKeyExchange is the client's quicClientKeyExchange's server-side
// counterpart: receive the client's ephemeral public key, derive session
// keys, send back our own public key.
func quicServerKeyExchange(stream net.Conn) (sendKey, receiveKey []byte, err error) {
	var clientKey common.KeyExchangeMsg
	if err := common.ReadMsg(stream, &clientKey); err != nil {
		return nil, nil, err
	}
	if len(clientKey.PublicKey) != 32 {
		return nil, nil, fmt.Errorf("invalid client public key length: %d", len(clientKey.PublicKey))
	}

	publicKey, sendKey, receiveKey, err := ServerKeyExchange(clientKey.PublicKey)
	if err != nil {
		return nil, nil, err
	}
	if err := common.WriteMsg(stream, common.KeyExchangeMsg{PublicKey: publicKey}); err != nil {
		return nil, nil, err
	}
	return sendKey, receiveKey, nil
}

// quicServeSession serves req and then any further read/write requests the
// client sends on the same stream, one at a time, until the client closes
// its side (a normal end of session, surfaced here as a read error) or a
// transport failure occurs.
func quicServeSession(stream net.Conn, req common.FSRequest, sendKey, receiveKey []byte) {
	for {
		var err error
		switch req.Op {
		case common.FSOpRead:
			err = quicServeReadOnce(stream, req, sendKey)
		case common.FSOpWrite:
			err = quicServeWriteOnce(stream, req, sendKey, receiveKey)
		default:
			return
		}
		if err != nil {
			return
		}

		_ = stream.SetReadDeadline(time.Now().Add(common.FileStreamSessionIdleTimeout))
		if err := common.ReadEncryptedMsg(stream, &req, receiveKey); err != nil {
			return
		}
	}
}

func quicServeList(stream net.Conn, req common.FSRequest, sendKey []byte) {
	resolvedRoot, _, err := remoteRootAndBase(req.Path)
	if err != nil {
		_ = common.WriteEncryptedMsg(stream, common.FSResponse{Err: err.Error()}, sendKey)
		return
	}
	entries, err := common.BuildManifest(resolvedRoot, req.Recursive)
	if err != nil {
		_ = common.WriteEncryptedMsg(stream, common.FSResponse{Err: err.Error()}, sendKey)
		return
	}
	_ = common.WriteEncryptedMsg(stream, common.FSResponse{OK: true, Entries: entries}, sendKey)
}

// quicServeReadOnce serves one byte-range read. It only returns a non-nil
// error for a transport failure that should end the session; an
// application-level failure (bad path, etc.) is reported to the client via
// FSResponse.Err and treated as handled.
func quicServeReadOnce(stream net.Conn, req common.FSRequest, sendKey []byte) error {
	_, basePath, err := remoteRootAndBase(req.Path)
	if err != nil {
		return common.WriteEncryptedMsg(stream, common.FSResponse{Err: err.Error()}, sendKey)
	}
	absPath := filepath.Join(basePath, filepath.FromSlash(req.RelPath))

	f, err := os.Open(absPath) //nolint:gosec
	if err != nil {
		return common.WriteEncryptedMsg(stream, common.FSResponse{Err: err.Error()}, sendKey)
	}
	defer f.Close()

	if err := common.WriteEncryptedMsg(stream, common.FSResponse{OK: true}, sendKey); err != nil {
		return err
	}

	section := io.NewSectionReader(f, req.Offset, req.Length)
	return common.CopyEncrypted(stream, section, req.Length, sendKey, nil)
}

func quicServeInit(stream net.Conn, req common.FSRequest, sendKey []byte) {
	resolvedRoot, _, err := remoteRootAndBase(req.Path)
	if err != nil {
		_ = common.WriteEncryptedMsg(stream, common.FSResponse{Err: err.Error()}, sendKey)
		return
	}
	rootIsDir := common.IsRootDir(resolvedRoot, req.SourceIsDir, req.TargetIsDirHint)
	if err := common.MaterializeTargets(req.Entries, resolvedRoot, rootIsDir); err != nil {
		_ = common.WriteEncryptedMsg(stream, common.FSResponse{Err: err.Error()}, sendKey)
		return
	}
	_ = common.WriteEncryptedMsg(stream, common.FSResponse{OK: true}, sendKey)
}

// quicServeWriteOnce serves one byte-range write. Same error-return
// convention as quicServeReadOnce.
func quicServeWriteOnce(stream net.Conn, req common.FSRequest, sendKey, receiveKey []byte) error {
	resolvedRoot, _, err := remoteRootAndBase(req.Path)
	if err != nil {
		return common.WriteEncryptedMsg(stream, common.FSResponse{Err: err.Error()}, sendKey)
	}
	rootIsDir := common.IsRootDir(resolvedRoot, req.SourceIsDir, req.TargetIsDirHint)
	// See serveWebRTCWriteOnce for why passing req.RelPath as sourceRoot is safe.
	dest := common.ResolveDestPath(resolvedRoot, rootIsDir, req.RelPath, req.RelPath)

	f, err := os.OpenFile(dest, os.O_WRONLY, 0) //nolint:gosec
	if err != nil {
		return common.WriteEncryptedMsg(stream, common.FSResponse{Err: err.Error()}, sendKey)
	}
	defer f.Close()

	if err := common.WriteEncryptedMsg(stream, common.FSResponse{OK: true}, sendKey); err != nil {
		return err
	}

	ow := io.NewOffsetWriter(f, req.Offset)
	if err := common.CopyDecrypted(ow, stream, req.Length, receiveKey, nil); err != nil {
		return err
	}
	return common.WriteEncryptedMsg(stream, common.FSResponse{OK: true}, sendKey)
}
