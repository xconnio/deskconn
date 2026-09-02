package deskconn_test

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/xconnio/deskconn"
	"github.com/xconnio/xconn-go"
)

func TestClientSessionsFanOutsUpgradeToAllSubscribers(t *testing.T) {
	r, err := xconn.NewRouter(&xconn.RouterConfig{})
	require.NoError(t, err)
	require.NoError(t, r.AddRealm("realm1", xconn.DefaultRealmConfig()))

	initial, err := xconn.ConnectInMemory(r, "realm1")
	require.NoError(t, err)

	cs := deskconn.NewClientSessions()
	initialCtx, initialCancel := context.WithCancel(context.Background())
	t.Cleanup(initialCancel)
	cs.StoreDeviceSession("device-realm", initial, nil, initialCtx, initialCancel)

	// Two concurrent callers sharing the cached connection — e.g. deskconn shell -A's shell
	// call and its agent-forward call — both ask for the current session and subscribe.
	sess1, upgradeCh1, err := cs.EnsureDeviceSessionWithUpgrade(context.Background(), "device-realm", "")
	require.NoError(t, err)
	require.Same(t, initial, sess1)

	sess2, upgradeCh2, err := cs.EnsureDeviceSessionWithUpgrade(context.Background(), "device-realm", "")
	require.NoError(t, err)
	require.Same(t, initial, sess2)

	// Simulate the background P2P upgrade replacing the cached session, exactly like
	// upgradeToWebRTC's success path does via StoreDeviceSession.
	upgraded, err := xconn.ConnectInMemory(r, "realm1")
	require.NoError(t, err)
	upgradedCtx, upgradedCancel := context.WithCancel(context.Background())
	t.Cleanup(upgradedCancel)
	cs.StoreDeviceSession("device-realm", upgraded, nil, upgradedCtx, upgradedCancel)

	select {
	case got := <-upgradeCh1:
		require.Same(t, upgraded, got)
	case <-time.After(3 * time.Second):
		t.Fatal("first subscriber was never notified of the upgrade")
	}
	select {
	case got := <-upgradeCh2:
		require.Same(t, upgraded, got)
	case <-time.After(3 * time.Second):
		t.Fatal("second subscriber was never notified of the upgrade")
	}
}
