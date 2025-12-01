package deskconn_test

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/grandcat/zeroconf"
	"github.com/stretchr/testify/require"

	"github.com/xconnio/deskconn"
)

func TestAdvertiseService(t *testing.T) {
	raw, err := os.ReadFile("/etc/machine-id")
	require.NoError(t, err)

	machineID := strings.TrimSpace(string(raw))
	require.NotEmpty(t, machineID)

	// Advertise service
	port := 9876
	hostname := "test-host"
	realm := "test-realm"

	server, err := deskconn.AdvertiseService(hostname, port, realm)
	require.NoError(t, err)
	require.NotNil(t, server)

	// Discover
	resolver, err := zeroconf.NewResolver(nil)
	require.NoError(t, err)

	found := make(chan *zeroconf.ServiceEntry, 1)

	entries := make(chan *zeroconf.ServiceEntry)

	go func() {
		for e := range entries {
			if e.Instance == `deskconnd\ \(`+hostname+`\)` {
				found <- e
				return
			}
		}
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 7*time.Second)
	defer cancel()
	err = resolver.Browse(ctx, "_xconn._tcp", "local.", entries)
	require.NoError(t, err)

	var entry *zeroconf.ServiceEntry
	select {
	case entry = <-found:
	case <-ctx.Done():
		t.Fatal("Service not discovered using real machine-id")
	}

	// Validate discovered service
	require.Equal(t, port, entry.Port)
	require.Contains(t, entry.Text, "realm="+realm)
	require.Contains(t, entry.Text, "machineid="+machineID)
	require.Contains(t, entry.Text, "path=/ws")
}
