package main

import (
	"net"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/xconnio/deskconn/internal/common"
	"github.com/xconnio/deskconn/internal/deskconn"
	auth "github.com/xconnio/xconn/bindings/go/auth"
)

const (
	testLab        = "lab"
	testOfficeID   = "aaaa1111bbbb"
	testMachineURL = "quic://host:18080"
	testOffice     = "office"
	testRealm      = "realm.office"
)

func TestAddStandaloneMachine(t *testing.T) {
	dir := t.TempDir()
	_, privateKey, err := auth.GenerateCryptoSignKeyPair()
	require.NoError(t, err)

	for _, bad := range []common.StandaloneMachine{
		{Name: "", URL: testMachineURL},
		{Name: "my lab", URL: testMachineURL},
		{Name: "lab:1", URL: testMachineURL},
		{Name: testLab, URL: "tcp://host:18080"},
		{Name: testLab, URL: "quic://host"},
		{Name: testLab, URL: testMachineURL, CertHash: "not-hex"},
		{Name: testLab, URL: testMachineURL, PrivateKey: "not-a-key"},
	} {
		require.Error(t, addStandaloneMachine(dir, bad), "%+v", bad)
	}
	machines, err := deskconn.StandaloneMachines(dir)
	require.NoError(t, err)
	require.Empty(t, machines, "nothing invalid is saved")

	require.NoError(t, addStandaloneMachine(dir, common.StandaloneMachine{Name: testLab, URL: testMachineURL,
		PrivateKey: privateKey}))
	require.NoError(t, addStandaloneMachine(dir, common.StandaloneMachine{Name: "web",
		URL: "https://host:443", Username: alice, PrivateKey: privateKey}))
	lab, err := deskconn.FindStandaloneMachine(dir, testLab)
	require.NoError(t, err)
	require.Equal(t, common.CurrentUsername(), lab.Username, "the username defaults to the current user's")
	require.ErrorContains(t, addStandaloneMachine(dir, common.StandaloneMachine{Name: testLab,
		URL: "quic://other:18080", PrivateKey: privateKey}), "already exists")
}

// Without a private key, desk machine add asks for the password to save, so that connecting
// never has to: where it can't ask (not a terminal, as here), it refuses the machine.
func TestAddStandaloneMachineNeedsKeyOrPassword(t *testing.T) {
	dir := t.TempDir()
	err := addStandaloneMachine(dir, common.StandaloneMachine{Name: testLab, URL: testMachineURL})
	require.ErrorContains(t, err, "needs a private key or a password")
	machines, err := deskconn.StandaloneMachines(dir)
	require.NoError(t, err)
	require.Empty(t, machines)
}

func TestStandaloneMachineResolution(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, deskconn.CacheMachines(dir, []common.Machine{
		{Authid: testOfficeID, Name: testOffice, Alias: "work", Realm: testRealm},
	}))
	require.NoError(t, deskconn.AddStandaloneMachine(dir, common.StandaloneMachine{Name: testLab,
		URL: testMachineURL, Username: alice}))
	t.Cleanup(func() { common.SetStandaloneTargetFor(common.StandaloneMachineRealm(testLab), nil) })

	realm, err := deskconn.MachineRealm(testOffice, dir)
	require.NoError(t, err)
	require.Equal(t, testRealm, realm)

	realm, err = deskconn.MachineRealm(testLab, dir)
	require.NoError(t, err)
	require.Equal(t, common.StandaloneMachineRealm(testLab), realm)
	target, ok := common.StandaloneTargetFor(realm)
	require.True(t, ok)
	require.Equal(t, alice, target.AuthID)
	require.Equal(t, testMachineURL, target.URL)

	require.Contains(t, machineNames(dir), testOffice)
	require.Contains(t, machineNames(dir), testLab)

	_, err = deskconn.MachineRealm("nope", dir)
	require.ErrorContains(t, err, "machine not found")
	_, err = deskconn.MachineRealm("", dir)
	require.ErrorContains(t, err, "no machine given", "not the first machine without an alias")
}

func TestStandaloneMachineCollision(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, deskconn.AddStandaloneMachine(dir, common.StandaloneMachine{Name: testLab,
		URL: testMachineURL}))
	// A managed machine named lab shows up later, as the account's machines are refreshed.
	require.NoError(t, deskconn.CacheMachines(dir, []common.Machine{
		{Authid: testOfficeID, Name: testLab, Realm: "realm.lab"},
	}))

	_, err := deskconn.MachineRealm(testLab, dir)
	require.ErrorContains(t, err, "refers to both a standalone machine and a managed machine")
	require.ErrorContains(t, err, "desk machine remove lab")

	realm, err := deskconn.MachineRealm("aaaa1111", dir)
	require.NoError(t, err, "the managed machine is still reachable by its ID")
	require.Equal(t, "realm.lab", realm)

	require.NoError(t, removeStandaloneMachine(dir, testLab), "and the standalone one can still be removed")
	realm, err = deskconn.MachineRealm(testLab, dir)
	require.NoError(t, err)
	require.Equal(t, "realm.lab", realm)
}

func TestStandaloneMachineIDPrefixCollision(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, deskconn.AddStandaloneMachine(dir, common.StandaloneMachine{Name: "aaaa",
		URL: testMachineURL}))
	// Later, the account gets a machine whose ID starts with the standalone machine's name.
	require.NoError(t, deskconn.CacheMachines(dir, []common.Machine{
		{Authid: testOfficeID, Name: testOffice, Realm: testRealm},
	}))

	_, err := deskconn.MachineRealm("aaaa", dir)
	require.ErrorContains(t, err, "refers to both", "a short ID meant for the managed machine isn't taken over")
	require.ErrorContains(t, err, testOfficeID, "the full ID tells them apart")
	realm, err := deskconn.MachineRealm(testOfficeID, dir)
	require.NoError(t, err)
	require.Equal(t, testRealm, realm)
}

// Standalone and managed machines resolve to realms of their own, and only the standalone
// ones have a target: what connecting to a realm uses instead of the daemon and the cloud
// login.
func TestStandaloneAndManagedStayApart(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, deskconn.CacheMachines(dir, []common.Machine{
		{Authid: testOfficeID, Name: testOffice, Realm: testRealm},
	}))
	require.NoError(t, deskconn.AddStandaloneMachine(dir, common.StandaloneMachine{Name: testLab,
		URL: testMachineURL, Username: alice}))
	require.NoError(t, deskconn.AddStandaloneMachine(dir, common.StandaloneMachine{Name: "lab2",
		URL: "quic://other:18080", Username: "bob"}))
	t.Cleanup(func() {
		common.SetStandaloneTargetFor(common.StandaloneMachineRealm(testLab), nil)
		common.SetStandaloneTargetFor(common.StandaloneMachineRealm("lab2"), nil)
	})

	office, err := deskconn.MachineRealm(testOffice, dir)
	require.NoError(t, err)
	lab, err := deskconn.MachineRealm(testLab, dir)
	require.NoError(t, err)
	lab2, err := deskconn.MachineRealm("lab2", dir)
	require.NoError(t, err)

	_, standalone := common.StandaloneTargetFor(office)
	require.False(t, standalone, "a managed machine never gets a standalone target")
	_, standalone = common.StandaloneTargetFor(lab)
	require.True(t, standalone)
	require.NotEqual(t, lab, lab2)
	t1, _ := common.StandaloneTargetFor(lab)
	t2, _ := common.StandaloneTargetFor(lab2)
	require.Equal(t, alice, t1.AuthID)
	require.Equal(t, "bob", t2.AuthID)

	require.ErrorContains(t, deskconn.UpdateMachineAlias(dir, testLab, "x"), "aliases are for managed machines")
	require.ErrorContains(t, deskconn.UpdateMachineAlias(dir, testOffice, testLab), "already names a standalone machine")
}

func TestRemoveStandaloneMachine(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, deskconn.CacheMachines(dir, []common.Machine{
		{Authid: testOfficeID, Name: testOffice, Realm: testRealm},
	}))

	require.ErrorContains(t, removeStandaloneMachine(dir, testOffice), "only removes standalone machines")
	require.ErrorContains(t, removeStandaloneMachine(dir, testLab), "no standalone machine named")

	require.NoError(t, deskconn.AddStandaloneMachine(dir, common.StandaloneMachine{Name: testLab,
		URL: testMachineURL}))
	require.NoError(t, removeStandaloneMachine(dir, testLab))
	machines, err := deskconn.StandaloneMachines(dir)
	require.NoError(t, err)
	require.Empty(t, machines)
}

// A standalone machine goes through the daemon's persistent connection while the daemon is
// running, and otherwise on a connection of its own, needing no daemon; a managed machine
// always goes through the daemon by default. A given --mode is always kept.
func TestStandaloneDefaultModeFollowsDaemon(t *testing.T) {
	dir := t.TempDir()
	lab := common.StandaloneMachineRealm(testLab)

	require.Equal(t, ModeRouted, deskconn.DefaultMode(dir, lab, ""), "no daemon")
	require.Equal(t, "", deskconn.DefaultMode(dir, testRealm, ""), "managed machines always use the daemon")
	require.Equal(t, ModeDirect, deskconn.DefaultMode(dir, lab, ModeDirect))

	socket := filepath.Join(dir, "deskconn.sock")
	listener, err := net.Listen("unix", socket)
	require.NoError(t, err)
	require.Equal(t, "", deskconn.DefaultMode(dir, lab, ""), "the daemon is running")
	require.Equal(t, ModeRouted, deskconn.DefaultMode(dir, lab, ModeRouted))

	// Left behind by a daemon that's gone: refused, so it's not mistaken for a running one.
	if l, ok := listener.(*net.UnixListener); ok {
		l.SetUnlinkOnClose(false)
	}
	require.NoError(t, listener.Close())
	require.FileExists(t, socket)
	require.Equal(t, ModeRouted, deskconn.DefaultMode(dir, lab, ""), "a stale socket")
}
