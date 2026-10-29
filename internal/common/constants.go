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
	ErrNoSuchProcedure = "wamp.error.no_such_procedure"
)

// These WebRTC data-channel labels identify which feature a freshly opened
// channel belongs to. They're checked both by the client side (when dialing
// out, see shellclient.go/portstreamclient.go/agentforwardclient.go/
// logstreamclient.go) and by userd's relay dispatch (see its streamrelay.go), which
// hands the channel xconn relays to whichever local backend handles that feature -- a
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
// hosted on deskconn.sock by xconn's app layer, with userd registering the
// handlers) -- wire-protocol identifiers the CLI (root package) calls
// directly and userd's proxy handlers implement, so both sides need
// the same string constants regardless of which process implements which
// half. They're under LocalProcedurePrefix, outside ProcedurePrefix and the app prefixes,
// so xconn doesn't expose them remotely.
const (
	LocalProcedurePrefix = "io.xconn.local."

	ProcedureProxyFileOp       = LocalProcedurePrefix + "proxy.file.op"
	ProcedureProxyMachineInfo  = LocalProcedurePrefix + "proxy.machine.info"
	ProcedureProxyPing         = LocalProcedurePrefix + "proxy.ping"
	ProcedureProxyCat          = LocalProcedurePrefix + "proxy.file.cat"
	ProcedureProxyPrinterList  = LocalProcedurePrefix + "proxy.printer.list"
	ProcedureProxyPrinterPrint = LocalProcedurePrefix + "proxy.printer.print"
	ProcedureProxyVPNStart     = LocalProcedurePrefix + "proxy.vpn.start"
	ProcedureProxyVPNStop      = LocalProcedurePrefix + "proxy.vpn.stop"
	ProcedureLogin             = LocalProcedurePrefix + "login"
	ProcedureLogout            = LocalProcedurePrefix + "logout"
	ProcedureConnect           = LocalProcedurePrefix + "connect"
	ProcedureDisconnect        = LocalProcedurePrefix + "disconnect"
	ProcedureDisconnectAll     = LocalProcedurePrefix + "disconnect_all"
	ProcedureConnectedMachines = LocalProcedurePrefix + "connected_machines"

	LocalRealm = "io.xconn.deskconn.local"

	// StandaloneRealm is the machine realm served in standalone mode.
	StandaloneRealm = "io.xconn.deskconn.standalone"
)

// These are the machine-facing procedures userd's app-layer handlers
// register (see userd's Deskconn.Register), and that the CLI (this
// root package) calls itself in --mode routed/direct. They're wire-protocol
// identifiers shared between the CLI and userd regardless of which
// process implements the handler, so they live here rather than in the
// host package.
const (
	ProcedureKeyExchange    = "io.xconn.deskconn.key.exchange"
	ProcedureShellIsBusy    = "io.xconn.deskconn.shell.isbusy"
	ProcedureFileBrowse     = "io.xconn.deskconn.file.browse"
	ProcedurePrinterList    = "io.xconn.deskconn.printer.list"
	ProcedurePrinterPrint   = "io.xconn.deskconn.printer.print"
	ProcedureFileRename     = "io.xconn.deskconn.file.rename"
	ProcedureFileDelete     = "io.xconn.deskconn.file.delete"
	ProcedureFileCopy       = "io.xconn.deskconn.file.copy"
	ProcedureFileEdit       = "io.xconn.deskconn.file.edit"
	ProcedureFileSearch     = "io.xconn.deskconn.file.search"
	ProcedureFileThumbnails = "io.xconn.deskconn.file.thumbnails"
	ProcedurePing           = "io.xconn.deskconn.ping"

	ProcedureMachineInfo      = "io.xconn.deskconn.machine.info"
	ProcedureMachineIsDesktop = "io.xconn.deskconn.machine.is_desktop"
	ProcedureProcessList      = "io.xconn.deskconn.process.list"
	ProcedureProcessSignal    = "io.xconn.deskconn.process.signal"
	ProcedureAppList          = "io.xconn.deskconn.app.list"
	ProcedureAppIcon          = "io.xconn.deskconn.app.icon"

	ProcedureClaudeSessionList = "io.xconn.deskconn.claude.session.list"
	ProcedureClaudeSessionPull = "io.xconn.deskconn.claude.session.pull"
	ProcedureClaudeProjectList = "io.xconn.deskconn.claude.project.list"

	ProcedureCodexSessionList = "io.xconn.deskconn.codex.session.list"
	ProcedureCodexSessionPull = "io.xconn.deskconn.codex.session.pull"
	ProcedureCodexProjectList = "io.xconn.deskconn.codex.project.list"

	ProcedureGitStatus   = "io.xconn.deskconn.git.status"
	ProcedureGitOriginal = "io.xconn.deskconn.git.original"
)

// Envelope kind bytes, own namespace like every other raw-stream feature's.
const (
	AgentFwdMsgControl byte = iota // encrypted JSON AgentForwardMsg
	AgentFwdMsgData                // encrypted connID-tagged bytes, see EncodeConnData
)

const (
	AgentFwdOpStart   agentFwdOp = "start"   // client->machine: begin forwarding, self-reporting AuthID
	AgentFwdOpConnect agentFwdOp = "connect" // machine->client: connID accepted
	AgentFwdOpClose   agentFwdOp = "close"
)

const (
	Realm = "io.xconn.deskconn"
)

const ProcedureFileCat = "io.xconn.deskconn.file.cat"

// ProcedureCapabilitiesList lists the apps the frontend may show and whether each is
// enabled. Apps served outside userd, like media-app's, are listed while their process
// is connected. ProcedureCapabilitiesSet enables or disables one: it's under
// ProcedureAdminPrefix, the procedures only a machine's owner and admins may call, which
// userd checks by the caller's role.
const (
	ProcedureCapabilitiesList = "io.xconn.deskconn.capabilities.list"
	ProcedureCapabilitiesSet  = "io.xconn.deskconn.admin.capabilities.set"
	// ProcedureCapabilitiesWatch sends the apps' list as a progressive result, again whenever
	// it changes.
	ProcedureCapabilitiesWatch = "io.xconn.deskconn.capabilities.watch"
	// TopicCapabilitiesChanged is published on the local realm, with the apps' list, after
	// an app is enabled or disabled.
	TopicCapabilitiesChanged = "io.xconn.deskconn.capabilities.on_change"
)

// The users of a standalone machine, whom its admins (the config file's users with role admin)
// may list, add and remove. Under ProcedureAdminPrefix, with plain arguments so any WAMP client
// can call them, and only in standalone mode: list takes nothing, add a user's username,
// password and authorized_keys as kwargs, who is a member, and remove a username.
const (
	ProcedureStandaloneUsersList   = ProcedureAdminPrefix + "users.list"
	ProcedureStandaloneUsersAdd    = ProcedureAdminPrefix + "users.add"
	ProcedureStandaloneUsersRemove = ProcedureAdminPrefix + "users.remove"
)

// Manifest apps' UI is YAML that clients render, calling procedures other apps serve.
// Owners and admins register them, with their files as JSON {"<path>": "<yaml>"}, and
// clients get an enabled one's app.yaml with screens and includes resolved.
const (
	ProcedureManifestRegister   = "io.xconn.deskconn.admin.manifest.register"
	ProcedureManifestUnregister = "io.xconn.deskconn.admin.manifest.unregister"
	ProcedureManifestGet        = "io.xconn.deskconn.manifest.get"
)

// ProcedureAppsRegister is how a process on userd's local socket registers the apps it
// serves, with a JSON list of {"id", "name", "key_exchange", "requires", "default_off"}
// (see the README's "Adding an app"). Local realm only.
const ProcedureAppsRegister = LocalProcedurePrefix + "apps.register"

// xconn exposes every procedure registered on userd's local socket under these
// prefixes to remote clients: userd's own and other apps' alike. Procedures under
// ProcedureAdminPrefix are only for a machine's owner and admins.
const (
	ProcedurePrefix      = "io.xconn.deskconn."
	ProcedureAdminPrefix = "io.xconn.deskconn.admin."
)

// The caller roles of a machine's owners and admins, who may call the procedures under
// ProcedureAdminPrefix: xconn's, for direct (P2P and standalone) callers, and
// deskconn-router's, for cloud ones.
const (
	RoleOwner      = "owner"
	RoleAdmin      = "admin"
	CloudRoleOwner = "user:owner"
	CloudRoleAdmin = "user:admin"
)

// RoleMember is the caller role of a standalone machine's users other than its admins.
const RoleMember = "member"

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
	// packets. VPNFrameOpen also lets userd's relay dispatch tell a
	// VPN channel apart from a file-stream channel.
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

// VPNFallbackDNS returns the resolvers a VPN client uses, through the
// tunnel, when the exit node didn't send any of its own that it can use.
func VPNFallbackDNS() []string {
	return []string{"1.1.1.1", "8.8.8.8"}
}

// Envelope kind bytes, own namespace like every other raw-stream feature's.
const (
	LogMsgControl byte = iota // encrypted JSON LogControlMsg, client's one-time request
	LogMsgData                // encrypted log bytes, machine -> client
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
	PortRevOpListen  portReverseOp = "listen"  // client->machine: start listening on RemotePort
	PortRevOpConnect portReverseOp = "connect" // machine->client: connID accepted, client should dial its local port
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

// StreamProxySocket is userd's stream proxy socket, in the config directory.
const StreamProxySocket = "deskconn-streams.sock"

// StreamProxyConnectTimeout bounds connecting to the machine for a stream proxy request.
const StreamProxyConnectTimeout = 30 * time.Second

// Socket and channel buffer sizes for the stream proxy, chosen so a proxied stream
// buffers about as much as a direct one in each direction.
const (
	StreamProxyUpstreamBuffer   = 64 * 1024 // CLI -> userd
	StreamProxyDownstreamBuffer = 2 << 20   // userd -> CLI
	StreamProxyBufferedHigh     = 256 * 1024
	StreamProxyBufferedLow      = 128 * 1024
)

const (
	ClaudeTool = "claude"
	CodexTool  = "codex"

	// AgentTitleScanLines caps how many leading lines of a session file are inspected for a title.
	AgentTitleScanLines = 50

	// AgentMaxTitleLen truncates a session title for display.
	AgentMaxTitleLen = 70

	// AgentSessionTailBytes is how much of a session file's end is read for its recent
	// messages, title and cwd.
	AgentSessionTailBytes = 256 * 1024
	// AgentRecentMessages is how many of a session's last chat messages are listed.
	AgentRecentMessages = 6
	AgentMaxMessageLen  = 500

	// AgentRoleUser is both a session record's type and its message's role for a user's message.
	AgentRoleUser      = "user"
	agentRoleAssistant = "assistant"
)

const (
	VPNOpOpenTUN             VPNHelperOp = "open_tun"
	VPNOpConfigureTUN        VPNHelperOp = "configure_tun"
	VPNOpAddHostRoute        VPNHelperOp = "add_host_route"
	VPNOpDelHostRoute        VPNHelperOp = "del_host_route"
	VPNOpReplaceDefaultRoute VPNHelperOp = "replace_default_route"
	VPNOpRestoreDefaultRoute VPNHelperOp = "restore_default_route"
	VPNOpBlockIPv6Default    VPNHelperOp = "block_ipv6_default"
	VPNOpRestoreIPv6Default  VPNHelperOp = "restore_ipv6_default"
	VPNOpSetLinkDNS          VPNHelperOp = "set_link_dns"
	VPNOpRevertLinkDNS       VPNHelperOp = "revert_link_dns"

	// The remaining ops are only used by the exit-node (server) role.
	VPNOpSetSysctl             VPNHelperOp = "set_sysctl"
	VPNOpRestoreSysctl         VPNHelperOp = "restore_sysctl"
	VPNOpAddMasquerade         VPNHelperOp = "add_masquerade"
	VPNOpDelMasquerade         VPNHelperOp = "del_masquerade"
	VPNOpAddForwardAccept      VPNHelperOp = "add_forward_accept"
	VPNOpDelForwardAccept      VPNHelperOp = "del_forward_accept"
	VPNOpAddForwardEstablished VPNHelperOp = "add_forward_established"
	VPNOpDelForwardEstablished VPNHelperOp = "del_forward_established"
)

const VPNHelperBufSize = 64 * 1024
