package common_test

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"gopkg.in/yaml.v3"

	"github.com/xconnio/deskconn/common"
)

func TestCredentialsFilePath(t *testing.T) {
	path, err := common.CredentialsFilePath()
	require.NoError(t, err)

	homeDir, err := os.UserHomeDir()
	require.NoError(t, err)

	require.Equal(t, filepath.Join(homeDir, ".deskconn", "credentials.json"), path)
}

func TestCfgDirectory(t *testing.T) {
	dir, err := common.CfgDirectory()
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

	devices, err := common.DevicesFromCfg(tmpDir)
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

	devices, err := common.DevicesFromCfg(tmpDir)
	require.NoError(t, err)
	require.Empty(t, devices)
}

func TestDevicesFromCfgMissingFile(t *testing.T) {
	_, err := common.DevicesFromCfg(t.TempDir())
	require.Error(t, err)
}

func TestDevicesFromCfgInvalidYAML(t *testing.T) {
	tmpDir := t.TempDir()
	require.NoError(t, os.WriteFile(
		filepath.Join(tmpDir, "config.yml"), []byte("not: [valid yaml\n"), 0600))

	_, err := common.DevicesFromCfg(tmpDir)
	require.Error(t, err)
}

func TestReadCredentialsSuccess(t *testing.T) {
	tmpDir := t.TempDir()
	require.NoError(t, os.WriteFile(
		filepath.Join(tmpDir, "id_ed25519"),
		[]byte("myprivatekey myauthid\n"),
		0600,
	))

	authid, privKey, err := common.ReadCredentials(tmpDir)
	require.NoError(t, err)
	require.Equal(t, "myauthid", authid)
	require.Equal(t, "myprivatekey", privKey)
}

func TestReadCredentialsNotLoggedIn(t *testing.T) {
	_, _, err := common.ReadCredentials(t.TempDir())
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

	authid, privKey, err := common.ReadCredentials(tmpDir)
	require.NoError(t, err)
	require.Equal(t, "myauthid", authid)
	require.Equal(t, "myprivkey", privKey)
}

func TestReadCredentialsValidExpiry(t *testing.T) {
	tmpDir := t.TempDir()
	expiresAt := time.Now().Add(30 * 24 * time.Hour).Format(time.RFC3339Nano)
	err := os.WriteFile(filepath.Join(tmpDir, "id_ed25519"), []byte("myprivatekey myauthid "+expiresAt+"\n"), 0600)
	require.NoError(t, err)

	authid, privKey, err := common.ReadCredentials(tmpDir)
	require.NoError(t, err)
	require.Equal(t, "myauthid", authid)
	require.Equal(t, "myprivatekey", privKey)
}

func TestReadCredentialsExpired(t *testing.T) {
	tmpDir := t.TempDir()
	expiresAt := time.Now().Add(-1 * time.Hour).Format(time.RFC3339Nano)
	err := os.WriteFile(filepath.Join(tmpDir, "id_ed25519"), []byte("myprivatekey myauthid "+expiresAt+"\n"), 0600)
	require.NoError(t, err)

	_, _, err = common.ReadCredentials(tmpDir)
	require.ErrorIs(t, err, common.ErrKeyExpired)
}

func TestReadCredentialsLegacyNoExpiry(t *testing.T) {
	tmpDir := t.TempDir()
	err := os.WriteFile(filepath.Join(tmpDir, "id_ed25519"), []byte("myprivatekey myauthid\n"), 0600)
	require.NoError(t, err)

	authid, privKey, err := common.ReadCredentials(tmpDir)
	require.NoError(t, err)
	require.Equal(t, "myauthid", authid)
	require.Equal(t, "myprivatekey", privKey)
}
