package deskconn

import (
	_ "image/gif"
	_ "image/png"
	"path/filepath"
	"strings"

	"github.com/sergi/go-diff/diffmatchpatch"

	"github.com/xconnio/deskconn/common"
)

// IsEditableExtension reports whether the CLI's `file edit` should treat
// remotePath as text rather than refusing it outright -- mirrors the same
// image/video/pdf/document extension classification host's file indexer
// uses to skip thumbnailing/full-text indexing for these categories.
func IsEditableExtension(name string) bool {
	switch strings.ToLower(filepath.Ext(name)) {
	case ".jpg", ".jpeg", ".png", ".gif", ".bmp", ".webp", ".ico", ".tiff", ".tif", ".heic", ".heif",
		common.ExtMp4, common.ExtWebm, common.ExtMov, common.ExtAvi, common.ExtMkv, common.ExtOgv, common.ExtFlv,
		common.ExtWmv, common.ExtM4v, common.Ext3gp,
		common.ExtPdf,
		".doc", ".docx", ".odt", ".rtf", ".xls", ".xlsx", ".ods",
		".ppt", ".pptx", ".odp", ".pages", ".numbers", ".key", ".epub":
		return false
	default:
		return true
	}
}

// BuildEditPatch computes a unified diff between original and edited,
// formatted for ProcedureFileEdit to apply on the device.
func BuildEditPatch(original, edited []byte) string {
	dmp := diffmatchpatch.New()
	diffs := dmp.DiffMain(string(original), string(edited), false)
	patches := dmp.PatchMake(string(original), diffs)
	return dmp.PatchToText(patches)
}
