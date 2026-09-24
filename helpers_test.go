package deskconn_test

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"gopkg.in/yaml.v3"

	"github.com/xconnio/deskconn"
)

func TestCredentialsFilePath(t *testing.T) {
	path, err := deskconn.CredentialsFilePath()
	require.NoError(t, err)

	homeDir, err := os.UserHomeDir()
	require.NoError(t, err)

	require.Equal(t, filepath.Join(homeDir, ".deskconn", "credentials.json"), path)
}

func TestCfgDirectory(t *testing.T) {
	dir, err := deskconn.CfgDirectory()
	require.NoError(t, err)

	homeDir, err := os.UserHomeDir()
	require.NoError(t, err)

	require.Equal(t, filepath.Join(homeDir, ".deskconn"), dir)

	info, err := os.Stat(dir)
	require.NoError(t, err)
	require.True(t, info.IsDir())
}

func TestDevicesFromCfgSuccess(t *testing.T) {
	tmpDir := t.TempDir()
	cfg := map[string]any{
		"devices": []map[string]any{
			{
				"authid": "user1",
				"id":     "dev1",
				"name":   "My Device",
				"realm":  "realm1",
				"organization": map[string]any{
					"id":   "org1",
					"name": "My Org",
				},
			},
		},
	}
	data, err := yaml.Marshal(cfg)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(tmpDir, "config.yml"), data, 0600))

	devices, err := deskconn.DevicesFromCfg(tmpDir)
	require.NoError(t, err)
	require.Len(t, devices, 1)
	require.Equal(t, "user1", devices[0].Authid)
	require.Equal(t, "dev1", devices[0].ID)
	require.Equal(t, "My Device", devices[0].Name)
	require.Equal(t, "realm1", devices[0].Realm)
	require.Equal(t, "org1", devices[0].Organization.ID)
}

func TestDevicesFromCfgEmptyDevices(t *testing.T) {
	tmpDir := t.TempDir()
	require.NoError(t, os.WriteFile(
		filepath.Join(tmpDir, "config.yml"), []byte("devices: []\n"), 0600))

	devices, err := deskconn.DevicesFromCfg(tmpDir)
	require.NoError(t, err)
	require.Empty(t, devices)
}

func TestDevicesFromCfgMissingFile(t *testing.T) {
	_, err := deskconn.DevicesFromCfg(t.TempDir())
	require.Error(t, err)
}

func TestDevicesFromCfgInvalidYAML(t *testing.T) {
	tmpDir := t.TempDir()
	require.NoError(t, os.WriteFile(
		filepath.Join(tmpDir, "config.yml"), []byte("not: [valid yaml\n"), 0600))

	_, err := deskconn.DevicesFromCfg(tmpDir)
	require.Error(t, err)
}

func TestCacheDevicesPreservesOtherSections(t *testing.T) {
	tmpDir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(tmpDir, "config.yml"),
		[]byte("devices:\n  - authid: old\nprinting:\n  mode: accept\nscreenshot:\n  enabled: true\n"), 0600))

	require.NoError(t, deskconn.CacheDevices(tmpDir, []deskconn.Device{{Authid: "new"}}))

	data, err := os.ReadFile(filepath.Join(tmpDir, "config.yml"))
	require.NoError(t, err)
	var config deskconn.Config
	require.NoError(t, yaml.Unmarshal(data, &config))
	require.Len(t, config.Devices, 1)
	require.Equal(t, "new", config.Devices[0].Authid)
	require.Equal(t, deskconn.PrintModeAccept, config.Printing.Mode)
	require.True(t, config.Screenshot.Enabled)
}

func TestCacheDevicesKeepsDirectDevices(t *testing.T) {
	tmpDir := t.TempDir()
	direct := deskconn.Device{Name: testDirectDevice, Realm: deskconn.DirectRealmPrefix + testDirectDevice,
		Address: "203.0.113.5:18080"}
	require.NoError(t, deskconn.CacheDevices(tmpDir, []deskconn.Device{{Authid: "cloud1"}, direct}))

	require.NoError(t, deskconn.CacheDevices(tmpDir, []deskconn.Device{{Authid: "cloud2"}}))

	devices, err := deskconn.DevicesFromCfg(tmpDir)
	require.NoError(t, err)
	require.Len(t, devices, 2)
	require.Equal(t, "cloud2", devices[0].Authid)
	require.Equal(t, direct, devices[1])
}

func TestCacheDevicesMissingFile(t *testing.T) {
	tmpDir := t.TempDir()
	require.NoError(t, deskconn.CacheDevices(tmpDir, []deskconn.Device{{Authid: "new"}}))

	devices, err := deskconn.DevicesFromCfg(tmpDir)
	require.NoError(t, err)
	require.Len(t, devices, 1)
}

func TestReadCredentialsSuccess(t *testing.T) {
	tmpDir := t.TempDir()
	require.NoError(t, os.WriteFile(
		filepath.Join(tmpDir, "id_ed25519"),
		[]byte("myprivatekey myauthid\n"),
		0600,
	))

	authid, privKey, err := deskconn.ReadCredentials(tmpDir)
	require.NoError(t, err)
	require.Equal(t, "myauthid", authid)
	require.Equal(t, "myprivatekey", privKey)
}

func TestReadCredentialsNotLoggedIn(t *testing.T) {
	_, _, err := deskconn.ReadCredentials(t.TempDir())
	require.ErrorContains(t, err, "user not logged in")
}

func TestReadCredentialsLineEnding(t *testing.T) {
	tmpDir := t.TempDir()
	// Verify TrimSpace handles
	require.NoError(t, os.WriteFile(
		filepath.Join(tmpDir, "id_ed25519"),
		[]byte("myprivkey myauthid\r\n"),
		0600,
	))

	authid, privKey, err := deskconn.ReadCredentials(tmpDir)
	require.NoError(t, err)
	require.Equal(t, "myauthid", authid)
	require.Equal(t, "myprivkey", privKey)
}

func TestReadCredentialsValidExpiry(t *testing.T) {
	tmpDir := t.TempDir()
	expiresAt := time.Now().Add(30 * 24 * time.Hour).Format(time.RFC3339Nano)
	err := os.WriteFile(filepath.Join(tmpDir, "id_ed25519"), []byte("myprivatekey myauthid "+expiresAt+"\n"), 0600)
	require.NoError(t, err)

	authid, privKey, err := deskconn.ReadCredentials(tmpDir)
	require.NoError(t, err)
	require.Equal(t, "myauthid", authid)
	require.Equal(t, "myprivatekey", privKey)
}

func TestReadCredentialsExpired(t *testing.T) {
	tmpDir := t.TempDir()
	expiresAt := time.Now().Add(-1 * time.Hour).Format(time.RFC3339Nano)
	err := os.WriteFile(filepath.Join(tmpDir, "id_ed25519"), []byte("myprivatekey myauthid "+expiresAt+"\n"), 0600)
	require.NoError(t, err)

	_, _, err = deskconn.ReadCredentials(tmpDir)
	require.ErrorIs(t, err, deskconn.ErrKeyExpired)
}

func TestReadCredentialsLegacyNoExpiry(t *testing.T) {
	tmpDir := t.TempDir()
	err := os.WriteFile(filepath.Join(tmpDir, "id_ed25519"), []byte("myprivatekey myauthid\n"), 0600)
	require.NoError(t, err)

	authid, privKey, err := deskconn.ReadCredentials(tmpDir)
	require.NoError(t, err)
	require.Equal(t, "myauthid", authid)
	require.Equal(t, "myprivatekey", privKey)
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
	devices, err := deskconn.DevicesFromCfg(tmpDir)
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
