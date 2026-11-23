package main

import (
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
		URL: "https://host:443", Username: alice}))
	lab, err := deskconn.FindStandaloneMachine(dir, testLab)
	require.NoError(t, err)
	require.Equal(t, common.CurrentUsername(), lab.Username, "the username defaults to the current user's")
	require.ErrorContains(t, addStandaloneMachine(dir, common.StandaloneMachine{Name: testLab,
		URL: "quic://other:18080"}), "already exists")
}

func TestStandaloneMachineResolution(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, deskconn.CacheMachines(dir, []common.Machine{
		{Authid: testOfficeID, Name: testOffice, Alias: "work", Realm: testRealm},
	}))
	require.NoError(t, deskconn.AddStandaloneMachine(dir, common.StandaloneMachine{Name: testLab,
		URL: testMachineURL, Username: alice}))
	t.Cleanup(func() { common.SetStandaloneTargetFor(common.StandaloneMachineRealm(testLab), nil) })

	realm, err := machineRealm(testOffice, dir)
	require.NoError(t, err)
	require.Equal(t, testRealm, realm)
	require.Equal(t, "", defaultMode(realm, ""), "managed machines go through the daemon by default")

	realm, err = machineRealm(testLab, dir)
	require.NoError(t, err)
	require.Equal(t, common.StandaloneMachineRealm(testLab), realm)
	target, ok := common.StandaloneTargetFor(realm)
	require.True(t, ok)
	require.Equal(t, alice, target.AuthID)
	require.Equal(t, testMachineURL, target.URL)
	require.Equal(t, ModeRouted, defaultMode(realm, ""), "the daemon doesn't know standalone machines")
	require.Equal(t, ModeDirect, defaultMode(realm, ModeDirect))

	require.Contains(t, machineNames(dir), testOffice)
	require.Contains(t, machineNames(dir), testLab)

	_, err = machineRealm("nope", dir)
	require.ErrorContains(t, err, "machine not found")
	_, err = machineRealm("", dir)
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

	_, err := machineRealm(testLab, dir)
	require.ErrorContains(t, err, "refers to both a standalone machine and a managed machine")
	require.ErrorContains(t, err, "desk machine remove lab")

	realm, err := machineRealm("aaaa1111", dir)
	require.NoError(t, err, "the managed machine is still reachable by its ID")
	require.Equal(t, "realm.lab", realm)

	require.NoError(t, removeStandaloneMachine(dir, testLab), "and the standalone one can still be removed")
	realm, err = machineRealm(testLab, dir)
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

	_, err := machineRealm("aaaa", dir)
	require.ErrorContains(t, err, "refers to both", "a short ID meant for the managed machine isn't taken over")
	require.ErrorContains(t, err, testOfficeID, "the full ID tells them apart")
	realm, err := machineRealm(testOfficeID, dir)
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

	office, err := machineRealm(testOffice, dir)
	require.NoError(t, err)
	lab, err := machineRealm(testLab, dir)
	require.NoError(t, err)
	lab2, err := machineRealm("lab2", dir)
	require.NoError(t, err)

	require.False(t, isStandaloneRealm(office), "a managed machine never gets a standalone target")
	require.True(t, isStandaloneRealm(lab))
	require.NotEqual(t, lab, lab2)
	t1, _ := common.StandaloneTargetFor(lab)
	t2, _ := common.StandaloneTargetFor(lab2)
	require.Equal(t, alice, t1.AuthID)
	require.Equal(t, "bob", t2.AuthID)

	for _, mode := range []string{"", ModeRouted, ModeDirect} {
		require.Equal(t, mode, defaultMode(office, mode), "managed machines keep their mode")
	}
	require.Equal(t, ModeRouted, defaultMode(lab, ""), "standalone machines never go through the daemon")

	require.ErrorContains(t, connectMachine(dir, testLab), "persistent connections are for managed machines")
	require.ErrorContains(t, updateMachineAlias(dir, testLab, "x"), "aliases are for managed machines")
	require.ErrorContains(t, updateMachineAlias(dir, testOffice, testLab), "already names a standalone machine")
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
