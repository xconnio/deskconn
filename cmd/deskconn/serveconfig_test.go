package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/alecthomas/kingpin/v2"
	"github.com/stretchr/testify/require"
)

func writeServeConfig(t *testing.T, yaml string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "serve.yaml")
	require.NoError(t, os.WriteFile(path, []byte(yaml), 0o600))
	return path
}

func TestLoadServeConfig(t *testing.T) {
	path := writeServeConfig(t, `
mode: standalone
standalone:
  transport: both
  quic-address: 127.0.0.1:28080
  tls-cert: cert.pem
  tls-key: /etc/deskconn/key.pem
  public-keys:
    - 31f195e2b9f3b05d48fc4e4f7dde83bd97582f42d53e6174f66140a039594830
    - "1234"
  users:
    - username: alice
      password: s3cret
    - username: bob
      password: "with:colon"
`)
	config, err := loadServeConfig(path)
	require.NoError(t, err)
	require.Equal(t, serveModeStandalone, config.Mode)
	require.Equal(t, transportBoth, config.Standalone.Transport)
	require.Equal(t, "127.0.0.1:28080", config.Standalone.QUICAddress)
	require.Equal(t, filepath.Join(filepath.Dir(path), "cert.pem"), config.Standalone.TLSCert, "relative to the file")
	require.Equal(t, "/etc/deskconn/key.pem", config.Standalone.TLSKey)
	require.Len(t, config.Standalone.PublicKeys, 2)
	require.Equal(t, []serveConfigUser{{"alice", "s3cret"}, {"bob", "with:colon"}}, config.Standalone.Users)

	empty, err := loadServeConfig(writeServeConfig(t, ""))
	require.NoError(t, err)
	require.Equal(t, serveConfig{}, *empty)
}

func TestLoadServeConfigRejects(t *testing.T) {
	for name, yaml := range map[string]string{
		"unknown field":    "mode: user\nstandalon:\n  transport: quic\n",
		"root mode":        "mode: root\n",
		"bad mode":         "mode: cloud\n",
		"bad transport":    "standalone:\n  transport: tcp\n",
		"user, no pass":    "standalone:\n  users:\n    - username: alice\n",
		"colon username":   "standalone:\n  users:\n    - username: a:b\n      password: x\n",
		"not a mapping":    "- mode\n",
		"wrong value type": "standalone:\n  users: alice\n",
	} {
		_, err := loadServeConfig(writeServeConfig(t, yaml))
		require.Error(t, err, name)
	}

	_, err := loadServeConfig(filepath.Join(t.TempDir(), "missing.yaml"))
	require.Error(t, err)
}

func TestServeConfigApply(t *testing.T) {
	path := writeServeConfig(t, `
mode: standalone
standalone:
  transport: webtransport
  quic-address: 127.0.0.1:1
  webtransport-address: 127.0.0.1:2
  public-keys: [aa]
  users:
    - username: alice
      password: s3cret
`)
	parse := func(args ...string) *serveCommands {
		app := kingpin.New("desk", "")
		cmds := registerServeCommands(app)
		_, err := app.Parse(append([]string{"serve", "--config", path}, args...))
		require.NoError(t, err)
		config, err := loadServeConfig(path)
		require.NoError(t, err)
		cmds.apply(config)
		return cmds
	}

	cmds := parse()
	require.Equal(t, serveModeStandalone, *cmds.mode)
	require.Equal(t, transportWebTransport, *cmds.userd.standaloneTransport)
	require.Equal(t, "127.0.0.1:1", *cmds.userd.standaloneQUICAddr)
	require.Equal(t, "127.0.0.1:2", *cmds.userd.standaloneWTAddr)
	require.Equal(t, []string{"aa"}, *cmds.userd.standaloneKeys)
	require.Equal(t, []string{"alice:s3cret"}, *cmds.userd.standaloneUsers)

	cmds = parse("--mode", "user", "--quic-address", "0.0.0.0:9", "--public-key", "bb", "--user", "bob:pw")
	require.Equal(t, serveModeUser, *cmds.mode, "flags win")
	require.Equal(t, "0.0.0.0:9", *cmds.userd.standaloneQUICAddr, "flags win")
	require.Equal(t, transportWebTransport, *cmds.userd.standaloneTransport, "the rest still from the file")
	require.Equal(t, []string{"aa", "bb"}, *cmds.userd.standaloneKeys, "keys add up")
	require.Equal(t, []string{"alice:s3cret", "bob:pw"}, *cmds.userd.standaloneUsers, "users add up")

	cmds = parse("--transport", "quic")
	require.Equal(t, transportQUIC, *cmds.userd.standaloneTransport, "a flag set to its default still wins")
}

func TestServeCommandParsing(t *testing.T) {
	const runCmd, initCmd = "serve run", "serve init"
	for args, want := range map[string]string{
		"serve":                     runCmd,
		"serve --mode user":         runCmd,
		"serve --config x.yaml":     runCmd,
		"serve init":                initCmd,
		"serve init --force x.yaml": initCmd,
	} {
		app := kingpin.New("desk", "")
		cmds := registerServeCommands(app)
		parsed, err := app.Parse(strings.Fields(args))
		require.NoError(t, err, args)
		require.Equal(t, want, parsed, args)
		if args == initCmd {
			require.Equal(t, "serve.yaml", *cmds.initPath, "the current directory by default")
		}
	}
}

func TestWriteSampleServeConfig(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sub", "serve.yaml")
	require.NoError(t, writeSampleServeConfig(path, false))

	info, err := os.Stat(path)
	require.NoError(t, err)
	require.Equal(t, os.FileMode(0o600), info.Mode().Perm())

	config, err := loadServeConfig(path)
	require.NoError(t, err, "the sample is a valid config")
	require.Equal(t, serveModeStandalone, config.Mode)
	require.Equal(t, transportQUIC, config.Standalone.Transport)

	require.NoError(t, os.WriteFile(path, []byte("mine"), 0o644))
	require.Error(t, writeSampleServeConfig(path, false), "keeps an existing file")
	data, err := os.ReadFile(path)
	require.NoError(t, err)
	require.Equal(t, "mine", string(data))

	require.NoError(t, writeSampleServeConfig(path, true))
	info, err = os.Stat(path)
	require.NoError(t, err)
	require.Equal(t, os.FileMode(0o600), info.Mode().Perm(), "and --force tightens its mode")
}
