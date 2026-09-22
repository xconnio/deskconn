package deskconn

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

// AppLayerProcedures lists every procedure deskconnd's Deskconn.Register
// exposes -- everything xlink needs to bridge from its cloud-facing
// realm into deskconnd's local one (see the procedure-bridge design).
// ProcedureFileCat is bridged too (it's declared in filecat.go, since it's
// also called directly, unproxied, by the CLI's ModeQUIC/ModeP2P paths).
// VPN start/stop aren't here: they're only ever reached through the local
// CLI-facing proxy (ProcedureProxyVPNStart/Stop), which now calls deskconnd's
// Arm/DisarmVPNServing directly in-process rather than over a bridged
// device-facing procedure.
//
//nolint:gochecknoglobals // a fixed policy list, not mutable state
var AppLayerProcedures = []string{
	ProcedureKeyExchange,
	ProcedureScreenBrightnessGet,
	ProcedureScreenBrightnessSet,
	ProcedureScreenLock,
	ProcedureScreenIsLocked,
	ProcedureShellIsBusy,
	ProcedureFileBrowse,
	ProcedurePrinterList,
	ProcedurePrinterPrint,
	ProcedureFileRename,
	ProcedureFileDelete,
	ProcedureFileCopy,
	ProcedureFileEdit,
	ProcedureFileSearch,
	ProcedurePing,
	ProcedureIndexQuery,
	ProcedureWallpaperGet,
	ProcedureWallpaperChecksum,
	ProcedureMPRISPlayers,
	ProcedureMPRISPlayPause,
	ProcedureMPRISPlay,
	ProcedureMPRISPause,
	ProcedureMPRISNext,
	ProcedureMPRISPrevious,
	ProcedureAudioMute,
	ProcedureAudioUnmute,
	ProcedureAudioToggleMute,
	ProcedureAudioIsMuted,
	ProcedureScreenshot,
	ProcedureScreenshotPermission,
	ProcedureDeviceInfo,
	ProcedureDeviceIsDesktop,
	ProcedureProcessList,
	ProcedureProcessSignal,
	ProcedureAppList,
	ProcedureAppIcon,
	ProcedureAISessionList,
	ProcedureAISessionPull,
	ProcedureGitStatus,
	ProcedureGitOriginal,
	ProcedureFileCat,
}
