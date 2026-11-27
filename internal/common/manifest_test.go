package common_test

import (
	"encoding/base64"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/xconnio/deskconn/internal/common"
)

// Only the PNG files the YAML names as icons are sent, not others left in the directory.
func TestReadManifestFilesOnlyNamedIcons(t *testing.T) {
	dir := t.TempDir()
	write := func(name, content string) {
		target := filepath.Join(dir, filepath.FromSlash(name))
		require.NoError(t, os.MkdirAll(filepath.Dir(target), 0700))
		require.NoError(t, os.WriteFile(target, []byte(content), 0600))
	}
	write("app.yaml", "app:\n  id: x\n  icon: icons/app.png\n")
	write("screens/home.yaml", "id: home\nchildren:\n  - icon: {on: icons/on.png, off: https://example.com/off.png}\n"+
		"  - icon: icons/missing.png\n  - icon: ../outside.png\n")
	write("icons/app.png", "app")
	write("icons/on.png", "on")
	write("screenshot.png", "screenshot")
	write("notes.txt", "notes")

	files, err := common.ReadManifestFiles(dir)
	require.NoError(t, err)
	png := func(data string) string {
		return common.ManifestPNGPrefix + base64.StdEncoding.EncodeToString([]byte(data))
	}
	require.Equal(t, map[string]string{
		"app.yaml":          "app:\n  id: x\n  icon: icons/app.png\n",
		"screens/home.yaml": files["screens/home.yaml"],
		"icons/app.png":     png("app"),
		"icons/on.png":      png("on"),
	}, files)
}
