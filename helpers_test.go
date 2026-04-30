package deskconn_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

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

// savePrincipalsFile reads the current principals.json (if any) and returns a
// cleanup function that restores it after the test.
func savePrincipalsFile(t *testing.T) func() {
	t.Helper()
	cfgDir, err := deskconn.CfgDirectory()
	require.NoError(t, err)
	path := filepath.Join(cfgDir, "principals.json")

	original, readErr := os.ReadFile(path)
	return func() {
		if readErr == nil {
			_ = os.WriteFile(path, original, 0600)
		} else {
			_ = os.Remove(path)
		}
	}
}

func TestWriteAndReadPrincipalsRoundTrip(t *testing.T) {
	t.Cleanup(savePrincipalsFile(t))

	principals := []*deskconn.CryptosignPrincipal{
		{AuthID: "alice", AuthorizedKeys: []string{"key-a1", "key-a2"}, AuthRole: "admin"},
		{AuthID: "bob", AuthorizedKeys: []string{"key-b1"}, AuthRole: "user"},
	}

	require.NoError(t, deskconn.WritePrincipalsToFile(principals))

	got, err := deskconn.ReadPrincipalsFromFile()
	require.NoError(t, err)
	require.Len(t, got, 2)

	byID := make(map[string]*deskconn.CryptosignPrincipal, len(got))
	for _, p := range got {
		byID[p.AuthID] = p
	}
	require.Equal(t, []string{"key-a1", "key-a2"}, byID["alice"].AuthorizedKeys)
	require.Equal(t, "admin", byID["alice"].AuthRole)
	require.Equal(t, []string{"key-b1"}, byID["bob"].AuthorizedKeys)
}

func TestWritePrincipalsProducesValidJSON(t *testing.T) {
	t.Cleanup(savePrincipalsFile(t))

	principals := []*deskconn.CryptosignPrincipal{
		{AuthID: "charlie", AuthorizedKeys: []string{"key-c"}, AuthRole: "user"},
	}
	require.NoError(t, deskconn.WritePrincipalsToFile(principals))

	cfgDir, err := deskconn.CfgDirectory()
	require.NoError(t, err)
	data, err := os.ReadFile(filepath.Join(cfgDir, "principals.json"))
	require.NoError(t, err)

	var raw []map[string]any
	require.NoError(t, json.Unmarshal(data, &raw))
	require.Len(t, raw, 1)
	require.Equal(t, "charlie", raw[0]["authid"])
}

func TestWritePrincipalsEmptyList(t *testing.T) {
	t.Cleanup(savePrincipalsFile(t))

	require.NoError(t, deskconn.WritePrincipalsToFile([]*deskconn.CryptosignPrincipal{}))

	got, err := deskconn.ReadPrincipalsFromFile()
	require.NoError(t, err)
	require.Empty(t, got)
}
