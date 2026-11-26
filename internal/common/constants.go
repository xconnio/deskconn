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

// These are the procedures the local CLI-facing realm exposes (LocalRealm,
// hosted on deskconn.sock by xconn's app layer, with userd registering the
// handlers) -- wire-protocol identifiers the CLI (root package) calls
// directly and userd's proxy handlers implement, so both sides need
// the same string constants regardless of which process implements which
// half. They're under LocalProcedurePrefix, outside ProcedurePrefix and the app prefixes,
// so xconn doesn't expose them remotely.
const (
	LocalProcedurePrefix = "local.deskconn."

	ProcedureProxyUpload      = LocalProcedurePrefix + "proxy.file.upload"
	ProcedureProxyFileOp      = LocalProcedurePrefix + "proxy.file.op"
	ProcedureProxyMachineInfo = LocalProcedurePrefix + "proxy.machine.info"
	ProcedureProxyPing        = LocalProcedurePrefix + "proxy.ping"
	ProcedureProxyCat         = LocalProcedurePrefix + "proxy.file.cat"
	ProcedureProxyVPNStart    = LocalProcedurePrefix + "proxy.vpn.start"
	ProcedureProxyVPNStop     = LocalProcedurePrefix + "proxy.vpn.stop"
	ProcedureLogin            = LocalProcedurePrefix + "login"
	ProcedureLogout           = LocalProcedurePrefix + "logout"
	ProcedureConnect          = LocalProcedurePrefix + "connect"
	// ProcedureDisconnect takes the kwargs {"realm": "..."} to disconnect one machine, or
	// {"all": true} to disconnect them all, but not both.
	ProcedureDisconnect        = LocalProcedurePrefix + "disconnect"
	ProcedureConnectedMachines = LocalProcedurePrefix + "connected_machines"

	LocalRealm = "local.deskconn"

	// StandaloneRealm is the machine realm served in standalone mode.
	StandaloneRealm = "com.deskconn.standalone"
)

// These are the machine-facing procedures userd's app-layer handlers
// register (see userd's Deskconn.Register), and that the CLI (this
// root package) calls itself in --mode routed/direct. They're wire-protocol
// identifiers shared between the CLI and userd regardless of which
// process implements the handler, so they live here rather than in the
// host package.
// RPC endpoints select and authorize features before byte traffic starts.
const (
	ProcedureFileListStream     = "com.deskconn.file.stream.list"
	ProcedureFileReadStream     = "com.deskconn.file.stream.read"
	ProcedureFileInitStream     = "com.deskconn.file.stream.init"
	ProcedureFileWriteStream    = "com.deskconn.file.stream.write"
	ProcedureShellStream        = "com.deskconn.shell.stream"
	ProcedureTerminalStream     = "com.deskconn.terminal.stream" // persistent sessions (see TerminalControlMsg)
	ProcedurePortForwardStream  = "com.deskconn.port.forward.stream"
	ProcedurePortReverseStream  = "com.deskconn.port.reverse.stream"
	ProcedureAgentForwardStream = "com.deskconn.agent.forward.stream"
	ProcedureLogsStream         = "com.deskconn.logs.stream"
	ProcedureProxyStream        = LocalProcedurePrefix + "proxy.stream"
)

const (
	ProcedureKeyExchange    = "com.deskconn.key.exchange"
	ProcedureShellIsBusy    = "com.deskconn.shell.isbusy"
	ProcedureFileBrowse     = "com.deskconn.file.browse"
	ProcedureFileRename     = "com.deskconn.file.rename"
	ProcedureFileDelete     = "com.deskconn.file.delete"
	ProcedureFileCopy       = "com.deskconn.file.copy"
	ProcedureFileEdit       = "com.deskconn.file.edit"
	ProcedureFileSearch     = "com.deskconn.file.search"
	ProcedureFileThumbnails = "com.deskconn.file.thumbnails"
	ProcedurePing           = "com.deskconn.ping"

	ProcedureMachineInfo      = "com.deskconn.machine.info"
	ProcedureMachineIsDesktop = "com.deskconn.machine.is_desktop"
	ProcedureProcessList      = "com.deskconn.process.list"
	ProcedureProcessSignal    = "com.deskconn.process.signal"
	ProcedureAppList          = "com.deskconn.app.list"
	ProcedureAppIcon          = "com.deskconn.app.icon"

	ProcedureClaudeSessionList = "com.deskconn.claude.session.list"
	ProcedureClaudeSessionPull = "com.deskconn.claude.session.pull"
	ProcedureClaudeProjectList = "com.deskconn.claude.project.list"

	ProcedureCodexSessionList = "com.deskconn.codex.session.list"
	ProcedureCodexSessionPull = "com.deskconn.codex.session.pull"
	ProcedureCodexProjectList = "com.deskconn.codex.project.list"

	ProcedureGitStatus   = "com.deskconn.git.status"
	ProcedureGitOriginal = "com.deskconn.git.original"
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
	Realm = "com.deskconn"
)

// The kwargs of ProcedureDisconnect: DisconnectKwargRealm names one machine's realm, and
// DisconnectKwargAll set to true disconnects them all.
const (
	DisconnectKwargRealm = "realm"
	DisconnectKwargAll   = "all"
)

const ProcedureFileCat = "com.deskconn.file.cat"

// ProcedureCapabilitiesList lists the apps the frontend may show and whether each is
// enabled. Apps served outside userd, like media-app's, are listed while their process
// is connected. ProcedureCapabilitiesSet enables or disables one: it's under
// ProcedureAdminPrefix, the procedures only a machine's owner and admins may call, which
// userd checks by the caller's role.
const (
	ProcedureCapabilitiesList = "com.deskconn.capabilities.list"
	ProcedureCapabilitiesSet  = "com.deskconn.admin.capabilities.set"
	// ProcedureCapabilitiesWatch sends the apps' list as a progressive result, again whenever
	// it changes.
	ProcedureCapabilitiesWatch = "com.deskconn.capabilities.watch"
	// TopicCapabilitiesChanged is published on the local realm, with the apps' list, after
	// an app is enabled or disabled.
	TopicCapabilitiesChanged = "com.deskconn.capabilities.on_change"
)

// Each user's settings on the machine, as their frontend keeps them: list returns the
// caller's as a dict of key to string value, save takes a key and its string value.
const (
	ProcedureSettingsList = "com.deskconn.settings.list"
	ProcedureSettingsSave = "com.deskconn.settings.save"
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
	ProcedureManifestRegister   = "com.deskconn.admin.manifest.register"
	ProcedureManifestUnregister = "com.deskconn.admin.manifest.unregister"
	ProcedureManifestGet        = "com.deskconn.manifest.get"
)

// ProcedureAppRegister is how a process on userd's local socket registers the apps it
// serves, with a JSON list of {"id", "name", "key_exchange", "requires", "default_off"}
// (see the README's "Adding an app"). Local realm only.
const ProcedureAppRegister = LocalProcedurePrefix + "app.register"

// xconn exposes every procedure registered on userd's local socket under these
// prefixes to remote clients: userd's own and other apps' alike. Procedures under
// ProcedureAdminPrefix are only for a machine's owner and admins.
const (
	ProcedurePrefix      = "com.deskconn."
	ProcedureAdminPrefix = "com.deskconn.admin."
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
	// FileStreamSessionIdleTimeout bounds how long a read/write stream
	// waits for the next chunk request from its worker before giving up and
	// closing -- normally the worker either sends another request right away
	// or closes the stream once its share of the transfer is done.
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
	FSOpTerminal     FSOp = "terminal" // persistent terminal sessions (see terminalstream.go)
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
	ProcedureWebRTCOffer     = "com.deskconn.webrtc.offer"
	TopicAnswererOnCandidate = "com.deskconn.webrtc.answerer.on_candidate"
	TopicOffererOnCandidate  = "com.deskconn.webrtc.offerer.on_candidate"

	CloudRealm = "com.deskconn"

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
	// ShellOpExit is the server's last message on a connection, sent after the
	// command's last output: ExitCode carries its exit status.
	ShellOpExit shellControlOp = "exit"
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
	ClaudeTool = "claude"
	CodexTool  = "codex"
)

const (
	VPNOpOpenTUN             VPNHelperOp = "open_tun"
	VPNOpConfigureTUN        VPNHelperOp = "configure_tun"
	VPNOpAddHostRoute        VPNHelperOp = "add_host_route"
	VPNOpDelHostRoute        VPNHelperOp = "del_host_route"
	VPNOpAddTunnelRoute      VPNHelperOp = "add_tunnel_route"
	VPNOpDelTunnelRoute      VPNHelperOp = "del_tunnel_route"
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

// AppPrefixes are the external app procedures exposed through a machine connection.
func AppPrefixes() []string {
	return []string{"com.deskconn.remotecontrol.", "com.deskconn.media."}
}
