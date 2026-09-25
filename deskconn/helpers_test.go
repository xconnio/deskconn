package deskconn_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
	"gopkg.in/yaml.v3"

	"github.com/xconnio/deskconn/common"
	"github.com/xconnio/deskconn/deskconn"
)

func TestCacheDevicesPreservesOtherSections(t *testing.T) {
	tmpDir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(tmpDir, "config.yml"),
		[]byte("devices:\n  - authid: old\nprinting:\n  mode: accept\nscreenshot:\n  enabled: true\n"), 0600))

	require.NoError(t, deskconn.CacheDevices(tmpDir, []common.Device{{Authid: "new"}}))

	data, err := os.ReadFile(filepath.Join(tmpDir, "config.yml"))
	require.NoError(t, err)
	var config common.Config
	require.NoError(t, yaml.Unmarshal(data, &config))
	require.Len(t, config.Devices, 1)
	require.Equal(t, "new", config.Devices[0].Authid)
	require.Equal(t, common.PrintModeAccept, config.Printing.Mode)
	require.True(t, config.Screenshot.Enabled)
}

func TestCacheDevicesKeepsDirectDevices(t *testing.T) {
	tmpDir := t.TempDir()
	direct := common.Device{Name: testDirectDevice, Realm: common.DirectRealmPrefix + testDirectDevice,
		Address: "203.0.113.5:18080"}
	require.NoError(t, deskconn.CacheDevices(tmpDir, []common.Device{{Authid: "cloud1"}, direct}))

	require.NoError(t, deskconn.CacheDevices(tmpDir, []common.Device{{Authid: "cloud2"}}))

	devices, err := common.DevicesFromCfg(tmpDir)
	require.NoError(t, err)
	require.Len(t, devices, 2)
	require.Equal(t, "cloud2", devices[0].Authid)
	require.Equal(t, direct, devices[1])
}

func TestCacheDevicesMissingFile(t *testing.T) {
	tmpDir := t.TempDir()
	require.NoError(t, deskconn.CacheDevices(tmpDir, []common.Device{{Authid: "new"}}))

	devices, err := common.DevicesFromCfg(tmpDir)
	require.NoError(t, err)
	require.Len(t, devices, 1)
}

func TestRemoveCredentialsFilesKeepsDirectDevices(t *testing.T) {
	tmpDir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(tmpDir, "id_ed25519"), []byte("x"), 0600))
	require.NoError(t, os.WriteFile(filepath.Join(tmpDir, "config.yml"), []byte(`devices:
  - authid: cloud
  - name: web1
    realm: direct.web1
    address: 203.0.113.5:18080
`), 0600))

	require.NoError(t, deskconn.RemoveCredentialsFiles(tmpDir))

	_, err := os.Stat(filepath.Join(tmpDir, "id_ed25519"))
	require.True(t, os.IsNotExist(err))
	devices, err := common.DevicesFromCfg(tmpDir)
	require.NoError(t, err)
	require.Len(t, devices, 1)
	require.Equal(t, testDirectDevice, devices[0].Name)
}

func TestRemoveCredentialsFilesRemovesAll(t *testing.T) {
	tmpDir := t.TempDir()
	for _, name := range []string{"id_ed25519", "id_ed25519.pub", "config.yml"} {
		require.NoError(t, os.WriteFile(filepath.Join(tmpDir, name), []byte("x"), 0600))
	}

	require.NoError(t, deskconn.RemoveCredentialsFiles(tmpDir))

	for _, name := range []string{"id_ed25519", "id_ed25519.pub", "config.yml"} {
		_, err := os.Stat(filepath.Join(tmpDir, name))
		require.True(t, os.IsNotExist(err), "expected %s to be removed", name)
	}
}

func TestRemoveCredentialsFilesEmptyDirectory(t *testing.T) {
	// Calling on an empty directory must not error.
	require.NoError(t, deskconn.RemoveCredentialsFiles(t.TempDir()))
}

func TestRemoveCredentialsFilesPartiallyPresent(t *testing.T) {
	tmpDir := t.TempDir()
	// Only create two of the three files.
	require.NoError(t, os.WriteFile(filepath.Join(tmpDir, "id_ed25519"), []byte("x"), 0600))
	require.NoError(t, os.WriteFile(filepath.Join(tmpDir, "config.yml"), []byte("x"), 0600))

	require.NoError(t, deskconn.RemoveCredentialsFiles(tmpDir))
}
