package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/alecthomas/kingpin/v2"
	"github.com/stretchr/testify/require"

	"github.com/xconnio/deskconn/internal/common"
	"github.com/xconnio/deskconn/internal/userd"
)

func writeServeConfig(t *testing.T, yaml string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "deskconn.yaml")
	require.NoError(t, os.WriteFile(path, []byte(yaml), 0o600))
	return path
}

const (
	alice, bob, carol = "alice", "bob", "carol"
	testPassword      = "s3cret"

	testKeyA = "31f195e2b9f3b05d48fc4e4f7dde83bd97582f42d53e6174f66140a039594830"
	testKeyB = "65160c38f1c3fa1d0e9cb1c0d8a7bc2f3b2a4fd4bd1bd2f6e2b9cfb1c3a4d5e6"
)

func TestLoadServeConfig(t *testing.T) {
	path := writeServeConfig(t, `
mode: standalone
listen:
  quic: 127.0.0.1:28080
  webtransport: 127.0.0.1:28081
  tls-cert: cert.pem
  tls-key: /etc/deskconn/key.pem
users:
  - username: alice
    password: s3cret
    role: admin
  - username: bob
    authorized-keys:
      - `+testKeyA+`
      - `+testKeyB+`
    role: member
  - username: carol
    password: "with:colon"
`)
	config, err := loadServeConfig(path)
	require.NoError(t, err)
	require.Equal(t, serveModeStandalone, config.Mode)
	require.Equal(t, "127.0.0.1:28080", config.Listen.QUIC)
	require.Equal(t, filepath.Join(filepath.Dir(path), "cert.pem"), config.Listen.TLSCert, "relative to the file")
	require.Equal(t, "/etc/deskconn/key.pem", config.Listen.TLSKey)
	require.Equal(t, []serveConfigUser{
		{Username: alice, Password: testPassword, Role: common.RoleAdmin},
		{Username: bob, AuthorizedKeys: []string{testKeyA, testKeyB}, Role: common.RoleMember},
		{Username: carol, Password: "with:colon"},
	}, config.Users)

	empty, err := loadServeConfig(writeServeConfig(t, ""))
	require.NoError(t, err)
	require.Equal(t, serveConfig{}, *empty)
}

func TestServeConfigListenTransport(t *testing.T) {
	require.Equal(t, "", serveConfigListen{}.transport())
	require.Equal(t, transportQUIC, serveConfigListen{QUIC: "a"}.transport())
	require.Equal(t, transportWebTransport, serveConfigListen{WebTransport: "b"}.transport())
	require.Equal(t, transportBoth, serveConfigListen{QUIC: "a", WebTransport: "b"}.transport())
}

func TestLoadServeConfigRejects(t *testing.T) {
	for name, test := range map[string]struct{ yaml, want string }{
		"unknown field":      {"mode: user\nlisten:\n  transport: quic\n", "field transport not found"},
		"old layout":         {"standalone:\n  quic-address: 0.0.0.0:1\n", "field standalone not found"},
		"root mode":          {"mode: root\n", "mode root is started by desk vpn"},
		"bad mode":           {"mode: cloud\n", `invalid mode "cloud"`},
		"cert without key":   {"listen:\n  tls-cert: c.pem\n", "tls-cert and tls-key go together"},
		"no username":        {"users:\n  - password: x\n", "user 1: needs a username"},
		"no password or key": {"users:\n  - username: alice\n", "needs a password or authorized-keys"},
		"empty keys":         {"users:\n  - username: a\n    authorized-keys: []\n", "needs a password or authorized-keys"},
		"colon username":     {"users:\n  - username: a:b\n    password: x\n", "can't contain ':'"},
		"bad role": {"users:\n  - {username: a, password: x, role: owner}\n",
			`invalid role "owner": want admin or member`},
		"same user twice": {"users:\n  - {username: a, password: x}\n" +
			"  - {username: a, authorized-keys: [" + testKeyA + "]}\n", "listed twice"},
		"key not hex":     {"users:\n  - username: a\n    authorized-keys: [zz]\n", "isn't a public key"},
		"key too short":   {"users:\n  - username: a\n    authorized-keys: [31f1]\n", "isn't a public key"},
		"keys not a list": {"users:\n  - username: a\n    authorized-keys: " + testKeyA + "\n", "into []string"},
		"plain keys":      {"users:\n  - username: a\n    keys: [" + testKeyA + "]\n", "field keys not found"},
		"old single key":  {"users:\n  - username: a\n    key: " + testKeyA + "\n", "field key not found"},
		"key for two users": {"users:\n  - {username: a, authorized-keys: [" + testKeyA + "]}\n" +
			"  - {username: b, authorized-keys: [" + strings.ToUpper(testKeyA) + "]}\n", `is already "a"'s`},
		"not a mapping":    {"- mode\n", "cannot unmarshal !!seq"},
		"users not a list": {"users: alice\n", "cannot unmarshal !!str"},
	} {
		_, err := loadServeConfig(writeServeConfig(t, test.yaml))
		require.ErrorContains(t, err, test.want, name)
	}

	_, err := loadServeConfig(filepath.Join(t.TempDir(), "missing.yaml"))
	require.Error(t, err)
}

func TestServeConfigApply(t *testing.T) {
	path := writeServeConfig(t, `
mode: standalone
listen:
  webtransport: 127.0.0.1:2
users:
  - username: alice
    password: s3cret
  - username: bob
    authorized-keys: [`+testKeyA+`, `+testKeyB+`]
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
	require.Equal(t, transportWebTransport, *cmds.userd.standaloneTransport, "from the addresses set")
	require.Equal(t, "127.0.0.1:2", *cmds.userd.standaloneWTAddr)
	require.Equal(t, "0.0.0.0:18080", *cmds.userd.standaloneQUICAddr, "unset: the flag's default")
	require.Equal(t, []serveConfigUser{
		{Username: alice, Password: testPassword},
		{Username: bob, AuthorizedKeys: []string{testKeyA, testKeyB}},
	}, cmds.userd.configUsers)
	require.Empty(t, *cmds.userd.standaloneUsers)

	cmds = parse("--mode", "user", "--webtransport-address", "0.0.0.0:9", "--user", "eve:pw")
	require.Equal(t, serveModeUser, *cmds.mode, "flags win")
	require.Equal(t, "0.0.0.0:9", *cmds.userd.standaloneWTAddr, "flags win")
	require.Len(t, cmds.userd.configUsers, 2, "the file's users stay")
	require.Equal(t, []string{"eve:pw"}, *cmds.userd.standaloneUsers, "next to the flags'")

	cmds = parse("--transport", "both")
	require.Equal(t, transportBoth, *cmds.userd.standaloneTransport, "--transport wins over the addresses")
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
			require.Equal(t, "deskconn.yaml", *cmds.initPath, "the current directory by default")
		}
	}
}

func TestWriteSampleServeConfig(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sub", "deskconn.yaml")
	require.NoError(t, writeSampleServeConfig(path, false))

	info, err := os.Stat(path)
	require.NoError(t, err)
	require.Equal(t, os.FileMode(0o600), info.Mode().Perm())

	config, err := loadServeConfig(path)
	require.NoError(t, err, "the sample is a valid config")
	require.Equal(t, serveModeStandalone, config.Mode)
	require.Equal(t, transportQUIC, config.Listen.transport())

	// And so is everything it has commented out, once uncommented.
	uncommented := strings.NewReplacer("# webtransport:", "webtransport:", "# tls-", "tls-",
		"# - username:", "- username:", "#   password:", "  password:",
		"#   authorized-keys:", "  authorized-keys:", "#     - ", "    - ",
		"65160c38...", testKeyA).Replace(sampleServeConfig)
	config, err = loadServeConfig(writeServeConfig(t, uncommented))
	require.NoError(t, err, uncommented)
	require.Equal(t, transportBoth, config.Listen.transport())
	require.Len(t, config.Users, 2)

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

func TestParseUserFlag(t *testing.T) {
	user, err := parseUserFlag("alice:s3cret")
	require.NoError(t, err)
	require.Equal(t, serveConfigUser{Username: alice, Password: testPassword}, user)

	user, err = parseUserFlag("bob:" + testKeyA)
	require.NoError(t, err)
	require.Equal(t, serveConfigUser{Username: bob, Password: testKeyA}, user, "always a password, even a key-like one")

	user, err = parseUserFlag("carol:pa:ss")
	require.NoError(t, err)
	require.Equal(t, "pa:ss", user.Password, "the password is everything after the first ':'")

	for _, value := range []string{alice, "alice:", ":s3cret", ""} {
		_, err := parseUserFlag(value)
		require.Error(t, err, value)
	}
	_, err = parseUserFlag(alice)
	require.NotContains(t, err.Error(), alice, "doesn't echo the value, which may be a password")
}

func TestStandaloneAuth(t *testing.T) {
	flags := func(users []string, configUsers ...serveConfigUser) userdFlags {
		return userdFlags{standaloneUsers: &users, configUsers: configUsers}
	}

	users, err := standaloneAuth(flags(
		[]string{"alice:s3cret"},
		serveConfigUser{Username: bob, AuthorizedKeys: []string{testKeyA, testKeyB}},
		serveConfigUser{Username: carol, Password: "pw"},
	), false)
	require.NoError(t, err)
	require.Equal(t, []userd.StandaloneUser{
		{Username: bob, AuthorizedKeys: []string{testKeyA, testKeyB}},
		{Username: carol, Password: "pw"},
		{Username: alice, Password: testPassword},
	}, users, "the config's users, then --user's, with keys only from the config")

	_, err = standaloneAuth(flags([]string{"alice:x"}, serveConfigUser{Username: alice, Password: "y"}), false)
	require.ErrorContains(t, err, "listed twice", "a user in the config and --user")

	_, err = standaloneAuth(flags([]string{"bob:pw", "bob:pw2"}), false)
	require.ErrorContains(t, err, "listed twice", "a --user twice")
}

func TestLoginMethods(t *testing.T) {
	require.Equal(t, "a password", loginMethods(serveConfigUser{Password: "x"}))
	require.Equal(t, "a key", loginMethods(serveConfigUser{AuthorizedKeys: []string{testKeyA}}))
	require.Equal(t, "a password and 2 keys",
		loginMethods(serveConfigUser{Password: "x", AuthorizedKeys: []string{testKeyA, testKeyB}}))
}

func TestNoStandaloneUsersHelp(t *testing.T) {
	require.Contains(t, noStandaloneUsersHelp(""), "--user alice:s3cret", "without a config: the flags")

	help := noStandaloneUsersHelp("deskconn.yaml")
	require.True(t, strings.HasPrefix(help, "deskconn.yaml has no users"), help)
	require.NotContains(t, help, "--user", "with a config: the config")

	// The users it shows are a valid config, once given a real key.
	var snippet []string
	for _, line := range strings.Split(help, "\n") {
		if strings.HasPrefix(line, "  ") {
			snippet = append(snippet, strings.TrimPrefix(line, "  "))
		}
	}
	yaml := strings.ReplaceAll(strings.Join(snippet, "\n"), "<public-key>", testKeyA)
	config, err := loadServeConfig(writeServeConfig(t, yaml))
	require.NoError(t, err, yaml)
	require.Len(t, config.Users, 2)
}
