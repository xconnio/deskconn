package common

import (
	"time"
)

// Standard WAMP error URIs used by every invocation handler, client and
// server side alike.
const (
	ErrInvalidArgument = "wamp.error.invalid_argument"
	ErrOperationFailed = "wamp.error.operation_failed"
	ErrNotAuthorized   = "wamp.error.not_authorized"
)

// These WebRTC data-channel labels identify which feature a freshly opened
// channel belongs to. They're checked both by the client side (when dialing
// out, see shellclient.go/portstreamclient.go/agentforwardclient.go/
// logstreamclient.go) and by xlink's channel classification, which
// relays the channel to whichever local backend handles that feature -- a
// file-stream channel has no label of its own (checked last, as the
// fallback case) since its first message -- a plaintext public key -- is
// content-indistinguishable from these features' first message.
const (
	ShellChannelLabel        = "shell"
	PortForwardChannelLabel  = "portforward"
	PortReverseChannelLabel  = "portreverse"
	AgentForwardChannelLabel = "agentforward"
	LogChannelLabel          = "logs"
)

// These are the procedures the local CLI-facing realm exposes (LocalRealm,
// hosted by xlink on deskconn.sock, with deskconnd registering the
// handlers) -- wire-protocol identifiers the CLI (root package) calls
// directly and deskconnd's proxy handlers implement, so both sides need
// the same string constants regardless of which process implements which
// half.
const (
	ProcedureProxyFileOp       = "io.xconn.deskconn.deskconnd.proxy.file.op"
	ProcedureProxyDeviceInfo   = "io.xconn.deskconn.deskconnd.proxy.device.info"
	ProcedureProxyPing         = "io.xconn.deskconn.deskconnd.proxy.ping"
	ProcedureProxyCat          = "io.xconn.deskconn.deskconnd.proxy.file.cat"
	ProcedureProxyPrinterList  = "io.xconn.deskconn.deskconnd.proxy.printer.list"
	ProcedureProxyPrinterPrint = "io.xconn.deskconn.deskconnd.proxy.printer.print"
	ProcedureProxyVPNStart     = "io.xconn.deskconn.deskconnd.proxy.vpn.start"
	ProcedureProxyVPNStop      = "io.xconn.deskconn.deskconnd.proxy.vpn.stop"
	ProcedureLogin             = "io.xconn.deskconn.login"
	ProcedureLogout            = "io.xconn.deskconn.logout"
	ProcedureConnect           = "io.xconn.deskconn.connect"
	ProcedureDisconnect        = "io.xconn.deskconn.disconnect"
	ProcedureDisconnectAll     = "io.xconn.deskconn.disconnect_all"
	ProcedureConnectedDevices  = "io.xconn.deskconn.connected_devices"

	LocalRealm = "io.xconn.deskconn.local"

	// StandaloneRealm is the device realm xlink serves in standalone mode.
	StandaloneRealm = "io.xconn.deskconn.standalone"
)

// These are the device-facing procedures deskconnd's app-layer handlers
// register (see deskconnd's Deskconn.Register), and that the CLI (this
// root package) calls directly in --mode quic/p2p. They're wire-protocol
// identifiers shared between the CLI and deskconnd regardless of which
// process implements the handler, so they live here rather than in the
// host package.
const (
	ProcedureKeyExchange         = "io.xconn.deskconn.deskconnd.key.exchange"
	ProcedureScreenBrightnessGet = "io.xconn.deskconn.deskconnd.screen.brightness.get"
	ProcedureScreenBrightnessSet = "io.xconn.deskconn.deskconnd.screen.brightness.set"
	ProcedureScreenLock          = "io.xconn.deskconn.deskconnd.screen.lock"
	ProcedureScreenIsLocked      = "io.xconn.deskconn.deskconnd.screen.islocked"
	ProcedureShellIsBusy         = "io.xconn.deskconn.deskconnd.shell.isbusy"
	ProcedureFileBrowse          = "io.xconn.deskconn.deskconnd.file.browse"
	ProcedurePrinterList         = "io.xconn.deskconn.deskconnd.printer.list"
	ProcedurePrinterPrint        = "io.xconn.deskconn.deskconnd.printer.print"
	ProcedureFileRename          = "io.xconn.deskconn.deskconnd.file.rename"
	ProcedureFileDelete          = "io.xconn.deskconn.deskconnd.file.delete"
	ProcedureFileCopy            = "io.xconn.deskconn.deskconnd.file.copy"
	ProcedureFileEdit            = "io.xconn.deskconn.deskconnd.file.edit"
	ProcedureFileSearch          = "io.xconn.deskconn.deskconnd.file.search"
	ProcedurePing                = "io.xconn.deskconn.deskconnd.ping"
	ProcedureIndexQuery          = "io.xconn.deskconn.deskconnd.index.query"
	ProcedureWallpaperGet        = "io.xconn.deskconn.deskconnd.wallpaper.get"
	ProcedureWallpaperChecksum   = "io.xconn.deskconn.deskconnd.wallpaper.checksum"

	ProcedureMPRISPlayers   = "io.xconn.deskconn.deskconnd.mpris.players"
	ProcedureMPRISPlayPause = "io.xconn.deskconn.deskconnd.mpris.playpause"
	ProcedureMPRISPlay      = "io.xconn.deskconn.deskconnd.mpris.play"
	ProcedureMPRISPause     = "io.xconn.deskconn.deskconnd.mpris.pause"
	ProcedureMPRISNext      = "io.xconn.deskconn.deskconnd.mpris.next"
	ProcedureMPRISPrevious  = "io.xconn.deskconn.deskconnd.mpris.previous"

	ProcedureAudioMute       = "io.xconn.deskconn.deskconnd.audio.mute"
	ProcedureAudioUnmute     = "io.xconn.deskconn.deskconnd.audio.unmute"
	ProcedureAudioToggleMute = "io.xconn.deskconn.deskconnd.audio.togglemute"
	ProcedureAudioIsMuted    = "io.xconn.deskconn.deskconnd.audio.ismuted"

	ProcedureScreenshot           = "io.xconn.deskconn.deskconnd.screenshot"
	ProcedureScreenshotPermission = "io.xconn.deskconn.deskconnd.screenshot.permission"

	ProcedureDeviceInfo      = "io.xconn.deskconn.deskconnd.device.info"
	ProcedureDeviceIsDesktop = "io.xconn.deskconn.deskconnd.device.is_desktop"
	ProcedureProcessList     = "io.xconn.deskconn.deskconnd.process.list"
	ProcedureProcessSignal   = "io.xconn.deskconn.deskconnd.process.signal"
	ProcedureAppList         = "io.xconn.deskconn.deskconnd.app.list"
	ProcedureAppIcon         = "io.xconn.deskconn.deskconnd.app.icon"

	ProcedureAISessionList = "io.xconn.deskconn.deskconnd.ai.session.list"
	ProcedureAISessionPull = "io.xconn.deskconn.deskconnd.ai.session.pull"

	ProcedureGitStatus   = "io.xconn.deskconn.deskconnd.git.status"
	ProcedureGitOriginal = "io.xconn.deskconn.deskconnd.git.original"
)

// Envelope kind bytes, own namespace like every other raw-stream feature's.
const (
	AgentFwdMsgControl byte = iota // encrypted JSON AgentForwardMsg
	AgentFwdMsgData                // encrypted connID-tagged bytes, see EncodeConnData
)

const (
	AgentFwdOpStart   agentFwdOp = "start"   // client->device: begin forwarding, self-reporting AuthID
	AgentFwdOpConnect agentFwdOp = "connect" // device->client: connID accepted
	AgentFwdOpClose   agentFwdOp = "close"
)

const (
	Realm                            = "io.xconn.deskconn"
	ProcedureDeskconnAttachDesktop   = "io.xconn.deskconn.desktop.attach"
	ProcedureDeskconnDetachDesktop   = "io.xconn.deskconn.desktop.detach"
	TopicDeskconnDesktopDetachFormat = "io.xconn.deskconn.desktop.%s.detach"
	MachineIDPath                    = "/etc/machine-id"
)

const (
	// DirectRealmPrefix marks the realm of a device reached directly (standalone
	// xlink) instead of through the cloud. The part after it is the device name;
	// it is only a local lookup key, the device itself serves StandaloneRealm.
	DirectRealmPrefix = "direct."
)

const ProcedureFileCat = "io.xconn.deskconn.deskconnd.file.cat"

const (
	ExtJpg  = ".jpg"
	ExtJpeg = ".jpeg"
	ExtPng  = ".png"
	ExtGif  = ".gif"
	ExtBmp  = ".bmp"
	ExtWebp = ".webp"
	ExtAvi  = ".avi"
	ExtPdf  = ".pdf"
	ExtFlv  = ".flv"
	ExtMkv  = ".mkv"
	ExtMp4  = ".mp4"
	ExtWebm = ".webm"
	ExtMov  = ".mov"
	ExtOgv  = ".ogv"
	ExtWmv  = ".wmv"
	ExtM4v  = ".m4v"
	Ext3gp  = ".3gp"
)

const (
	P2PMsgControl byte = iota // encrypted JSON control message (FSRequest/FSResponse)
	P2PMsgData                // encrypted raw chunk bytes
)

const (
	// FileStreamChunkSize is the size of each binary data channel message
	// used while streaming a byte range, in either direction. Set just under
	// pion's default SCTP max message size (math.MaxUint16 = 65535 bytes),
	// leaving room for the 1-byte envelope kind prefix and the 28-byte
	// ChaCha20-Poly1305 nonce+tag overhead.
	FileStreamChunkSize = 65024

	// FileStreamMaxBuffered/FileStreamBufferedLow mirror the backpressure
	// thresholds used for the main WAMP peer in xconn-webrtc-go's peer.go, so a
	// slow reader can't make the send buffer grow unbounded.
	FileStreamMaxBuffered = 512 * 1024 // 512KB
	FileStreamBufferedLow = 256 * 1024 // 256KB

	// FileStreamRequestTimeout bounds how long a write handler waits between
	// binary messages before giving up on a stalled sender.
	FileStreamRequestTimeout = 10 * time.Second

	// FileStreamSessionIdleTimeout bounds how long a read/write channel
	// waits for the next chunk request from its worker before giving up and
	// closing -- normally the worker either sends another request right away
	// or closes the channel itself once its share of the transfer is done.
	FileStreamSessionIdleTimeout = 30 * time.Second
)

const (
	FSOpList         FSOp = "list"         // enumerate a remote source (download manifest)
	FSOpRead         FSOp = "read"         // fetch one byte range of one file (download)
	FSOpInit         FSOp = "init"         // create dirs/pre-size files at a remote destination (upload)
	FSOpWrite        FSOp = "write"        // send one byte range of one file (upload)
	FSOpShell        FSOp = "shell"        // interactive shell (see shell.go/shellstream.go)
	FSOpPortForward  FSOp = "portforward"  // one forwarded TCP connection (see portstream.go)
	FSOpPortReverse  FSOp = "portreverse"  // one reverse-forward session (see portstream.go)
	FSOpAgentForward FSOp = "agentforward" // one agent-forward session (see agentforwardstream.go)
	FSOpLogs         FSOp = "logs"
)

// progressPrintInterval caps how often TransferProgress.add actually prints
// an update. Without this, a fine-grained transport (e.g. one call per 16KB
// WebRTC message) prints on the same goroutine reading data off the wire,
// and a slow-to-drain stderr (tmux, an SSH session) throttles real transfer
// throughput down to terminal redraw speed.
const progressPrintInterval = int64(100 * time.Millisecond)

// P2PRequestTimeout bounds how long a client waits for a channel to open or
// for the remote side's response to a control request (list/init) or the
// first ack on a read/write chunk request.
const P2PRequestTimeout = 15 * time.Second

const (
	ProcedureWebRTCOffer     = "io.xconn.webrtc.offer"
	TopicAnswererOnCandidate = "io.xconn.webrtc.answerer.on_candidate"
	TopicOffererOnCandidate  = "io.xconn.webrtc.offerer.on_candidate"

	CloudRealm = "io.xconn.deskconn"

	StunServerURL = "stun:stun.l.google.com:19302"
)

const (
	VPNServerTUNName = "dtun0"
	VPNClientTUNName = "dtun1"

	VPNServerIP   = "10.66.0.1"
	VPNClientIP   = "10.66.0.2"
	VPNServerCIDR = VPNServerIP + "/24"
	VPNClientCIDR = VPNClientIP + "/24"
	VPNSubnetCIDR = "10.66.0.0/24"

	// VPNMTU keeps every tunneled packet inside one SCTP/DTLS/UDP datagram,
	// so one TUN read maps to exactly one DataChannel message and neither
	// side needs a reassembly protocol of its own.
	VPNMTU = 1200

	VPNChannelLabel = "vpn"

	// VPNFrameOpen  and VPNFrameReady are the "type" discriminators for the two
	// control frames exchanged -- as DataChannel *text* messages -- before
	// either side starts treating channel messages as raw binary IP
	// packets. VPNFrameOpen also lets xlink's channel classification
	// tell a VPN channel apart from a file-stream channel.
	VPNFrameOpen  = "vpn-open"
	VPNFrameReady = "vpn-ready"

	// VPNHandshakeTimeout bounds how long the client waits for the channel
	// to open and for the server's ready frame.
	VPNHandshakeTimeout = 15 * time.Second

	vpnSendBufferHigh = 512 * 1024
	vpnSendBufferLow  = 256 * 1024

	// VPNMaxRetransmits bounds how many times SCTP will retry a lost chunk
	// before giving up on it, instead of giving up immediately (0). Kept
	// small and the channel stays unordered so tunneled TCP retransmits
	// aren't needlessly duplicated and one loss can't head-of-line-block
	// later packets, but it recovers most single, independent losses on the
	// underlying path -- which otherwise pass straight through to whatever
	// is running over the tunnel (ping/ICMP most visibly, since it has no
	// retry of its own).
	VPNMaxRetransmits = 2
)

// Envelope kind bytes, own namespace like every other raw-stream feature's.
const (
	LogMsgControl byte = iota // encrypted JSON LogControlMsg, client's one-time request
	LogMsgData                // encrypted log bytes, device -> client
	LogMsgPing                // no payload, either direction, keeps ShellIdleTimeout from firing
)

// Envelope kind bytes, mirroring ShellMsgControl/ShellMsgData but in their
// own namespace since these features never share a connection with
// shell/file-transfer.
const (
	PortMsgControl byte = iota // encrypted JSON control message
	PortMsgData                // encrypted relayed bytes (port reverse: connID-tagged, see EncodeConnData)
)

const (
	PortRevOpListen  portReverseOp = "listen"  // client->device: start listening on RemotePort
	PortRevOpConnect portReverseOp = "connect" // device->client: connID accepted, client should dial its local port
	PortRevOpClose   portReverseOp = "close"
)

const (
	PrintModeDisabled PrintMode = "disabled"
	PrintModeAccept   PrintMode = "accept"
	PrintModeHost     PrintMode = "host"
)

// maxMsgSize bounds ReadFrame's allocation. Control frames (FSRequest/
// FSResponse, possibly encrypted) are small; encrypted file-data chunks are
// bounded by encChunkSize plus a little AEAD overhead. Either way this is
// generous headroom rather than a tight fit.
const maxMsgSize = 1 << 20 // 1 MiB

// encChunkSize is the plaintext size of each encrypted file-data message,
// set near the maxMsgSize ceiling QUIC streams have no hard transport limit
// requiring anything smaller.
const encChunkSize = 1000000

// ShellPingInterval is how often shellPingLoop sends ShellOpPing while
// otherwise idle. Must stay comfortably under ShellIdleTimeout.
const ShellPingInterval = 10 * time.Second

const (
	// ShellOpSize creates a new PTY if this connection has none yet,
	// otherwise resizes the existing one.
	ShellOpSize shellControlOp = "size"
	// ShellOpMigrate claims an existing PTY from a different transport --
	// sent as the first message on a freshly opened connection.
	ShellOpMigrate shellControlOp = "migrate"
	// ShellOpPing is sent periodically by the client whenever otherwise
	// idle, purely so the connection keeps producing traffic -- it needs no
	// server-side handling beyond having been read (see ShellIdleTimeout).
	ShellOpPing shellControlOp = "ping"
)

// ShellIdleTimeout bounds how long a shell/exec (or port-forward/reverse,
// agent-forward, logs) connection may go without any traffic before it's
// considered dead. QUIC streams have no equivalent of WebRTC's own
// peer-connectivity checks, so without this an abruptly disconnected
// client (crash, killed process, dropped network) leaves its remote
// command running forever instead of being cleaned up -- the client sends
// ShellOpPing on a timer (ShellPingInterval, shellclient.go) specifically
// to keep this from firing during genuine silence (e.g. a user just
// reading output, not typing).
const ShellIdleTimeout = FileStreamSessionIdleTimeout

const (
	ShellMsgControl byte = iota // encrypted JSON ShellControlMsg
	ShellMsgData                // encrypted raw PTY input/output bytes
)

const (
	RelayKindQUIC   RelayKind = "quic"
	RelayKindWebRTC RelayKind = "webrtc"
)

const (
	relayFrameBinary relayFrameKind = 0
	relayFrameText   relayFrameKind = 1
)

// maxRelayFrameSize bounds ReadRelayFrame's allocation.
const maxRelayFrameSize = 1 << 20 // 1 MiB

// relayBufferedHigh/Low bound how much data RelayWebRTCChannel lets pile up
// in the real channel's own SCTP send buffer before pausing reads from conn.
const (
	RelayBufferedHigh = 512 * 1024
	RelayBufferedLow  = 256 * 1024
)
