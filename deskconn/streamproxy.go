package deskconn

import (
	"context"
	"errors"
	"fmt"
	"net"
	"path/filepath"

	"github.com/xconnio/deskconn/common"
)

// ErrDaemonUnavailable means deskconnd's stream proxy can't be reached, so a default-mode
// command should connect to the device itself.
var ErrDaemonUnavailable = errors.New("deskconnd stream proxy unavailable")

// DaemonStreams opens raw streams to a device on deskconnd's persistent connection to it:
// data channels if that connection is P2P, QUIC streams otherwise (see P2P).
type DaemonStreams struct {
	socketPath string
	realm      string
	kind       common.RelayKind
	// hold keeps the QUIC connection open for this operation; nil on P2P.
	hold net.Conn
}

// DialDaemonStreams asks deskconnd for its connection to realm's device, which it
// establishes first if needed. Close it when the operation is done.
func DialDaemonStreams(ctx context.Context, realm, cfgDirectory string) (*DaemonStreams, error) {
	d := &DaemonStreams{socketPath: filepath.Join(cfgDirectory, common.StreamProxySocket), realm: realm}
	conn, kind, err := d.open(ctx, common.StreamProxyRequest{Realm: realm})
	if err != nil {
		return nil, err
	}
	d.kind = kind
	if kind == common.RelayKindQUIC {
		d.hold = conn
	} else {
		_ = conn.Close()
	}
	return d, nil
}

// P2P reports which transport the operation has to use: OpenMessageChannel if true,
// OpenStream otherwise.
func (d *DaemonStreams) P2P() bool { return d.kind == common.RelayKindWebRTC }

func (d *DaemonStreams) Close() error {
	if d.hold == nil {
		return nil
	}
	return d.hold.Close()
}

// OpenStream opens a raw QUIC stream to the device.
func (d *DaemonStreams) OpenStream() (net.Conn, error) {
	conn, _, err := d.open(context.Background(), common.StreamProxyRequest{Realm: d.realm, Kind: common.RelayKindQUIC})
	return conn, err
}

// OpenMessageChannel opens a data channel to the device.
func (d *DaemonStreams) OpenMessageChannel(label string) (common.MessageChannel, error) {
	conn, _, err := d.open(context.Background(),
		common.StreamProxyRequest{Realm: d.realm, Kind: common.RelayKindWebRTC, Label: label})
	if err != nil {
		return nil, err
	}
	return common.NewRelayChannel(conn), nil
}

func (d *DaemonStreams) open(ctx context.Context, req common.StreamProxyRequest) (net.Conn,
	common.RelayKind, error) {
	var dialer net.Dialer
	conn, err := dialer.DialContext(ctx, "unix", d.socketPath)
	if err != nil {
		return nil, "", fmt.Errorf("%w: %w", ErrDaemonUnavailable, err)
	}
	if unixConn, ok := conn.(*net.UnixConn); ok {
		_ = unixConn.SetWriteBuffer(common.StreamProxyUpstreamBuffer)
	}
	// Lets ctx interrupt the wait while deskconnd is still connecting to the device.
	stop := context.AfterFunc(ctx, func() { _ = conn.Close() })

	resp, err := proxyHandshake(conn, req)
	if !stop() {
		_ = conn.Close()
		return nil, "", ctx.Err()
	}
	if err != nil {
		_ = conn.Close()
		return nil, "", err
	}
	return conn, resp.Kind, nil
}

func proxyHandshake(conn net.Conn, req common.StreamProxyRequest) (common.StreamProxyResponse, error) {
	if err := common.WriteMsg(conn, req); err != nil {
		return common.StreamProxyResponse{}, fmt.Errorf("deskconnd stream proxy: %w", err)
	}
	var resp common.StreamProxyResponse
	if err := common.ReadMsg(conn, &resp); err != nil {
		return common.StreamProxyResponse{}, fmt.Errorf("deskconnd stream proxy: %w", err)
	}
	if resp.Error != "" {
		return common.StreamProxyResponse{}, errors.New(resp.Error)
	}
	return resp, nil
}

// DownloadFilesDaemon downloads over d, on whichever transport it has.
func DownloadFilesDaemon(d *DaemonStreams, remotePath, localPath string, recursive bool, numWorkers int) error {
	if d.P2P() {
		return DownloadFilesP2P(d, remotePath, localPath, recursive, numWorkers)
	}
	return DownloadFilesQUIC(d, d.realm, remotePath, localPath, recursive, numWorkers)
}

// UploadFilesDaemon uploads over d, on whichever transport it has.
func UploadFilesDaemon(d *DaemonStreams, localPath, remotePath string, recursive bool, numWorkers int) error {
	if d.P2P() {
		return UploadFilesP2P(d, localPath, remotePath, recursive, numWorkers)
	}
	return UploadFilesQUIC(d, d.realm, localPath, remotePath, recursive, numWorkers)
}
