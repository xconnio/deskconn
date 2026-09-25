package xlink

import (
	"github.com/xconnio/deskconn/common"
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
	common.ProcedureKeyExchange,
	common.ProcedureScreenBrightnessGet,
	common.ProcedureScreenBrightnessSet,
	common.ProcedureScreenLock,
	common.ProcedureScreenIsLocked,
	common.ProcedureShellIsBusy,
	common.ProcedureFileBrowse,
	common.ProcedurePrinterList,
	common.ProcedurePrinterPrint,
	common.ProcedureFileRename,
	common.ProcedureFileDelete,
	common.ProcedureFileCopy,
	common.ProcedureFileEdit,
	common.ProcedureFileSearch,
	common.ProcedurePing,
	common.ProcedureIndexQuery,
	common.ProcedureWallpaperGet,
	common.ProcedureWallpaperChecksum,
	common.ProcedureMPRISPlayers,
	common.ProcedureMPRISPlayPause,
	common.ProcedureMPRISPlay,
	common.ProcedureMPRISPause,
	common.ProcedureMPRISNext,
	common.ProcedureMPRISPrevious,
	common.ProcedureAudioMute,
	common.ProcedureAudioUnmute,
	common.ProcedureAudioToggleMute,
	common.ProcedureAudioIsMuted,
	common.ProcedureScreenshot,
	common.ProcedureScreenshotPermission,
	common.ProcedureDeviceInfo,
	common.ProcedureDeviceIsDesktop,
	common.ProcedureProcessList,
	common.ProcedureProcessSignal,
	common.ProcedureAppList,
	common.ProcedureAppIcon,
	common.ProcedureAISessionList,
	common.ProcedureAISessionPull,
	common.ProcedureGitStatus,
	common.ProcedureGitOriginal,
	common.ProcedureFileCat,
}
