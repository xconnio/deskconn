//go:build windows

package vpnwin

import (
	"encoding/binary"
	"net"
	"os"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"golang.org/x/sys/windows"
)

const (
	testIP      = "10.77.0.2"
	testPeerIP  = "10.77.0.9"
	testPinned  = "203.0.113.7"
	testRouted  = "198.51.100.7"
	testTimeout = 10 * time.Second
)

func readUntil(t *testing.T, packets <-chan []byte, dst string) []byte {
	t.Helper()
	want := net.ParseIP(dst).To4()
	timeout := time.After(testTimeout)
	for {
		select {
		case packet, ok := <-packets:
			require.True(t, ok, "device closed waiting for a packet to %s", dst)
			if len(packet) >= 20 && net.IP(packet[16:20]).Equal(want) {
				return packet
			}
		case <-timeout:
			t.Fatalf("no packet to %s came out of the tunnel", dst)
		}
	}
}

// udpPacket builds an IPv4 UDP packet, with no UDP checksum, which IPv4 allows.
func udpPacket(src, dst net.IP, srcPort, dstPort int, payload []byte) []byte {
	packet := make([]byte, 28+len(payload))
	packet[0] = 0x45
	binary.BigEndian.PutUint16(packet[2:], uint16(len(packet)))
	packet[8] = 64
	packet[9] = 17
	copy(packet[12:], src.To4())
	copy(packet[16:], dst.To4())

	var sum uint32
	for i := 0; i < 20; i += 2 {
		sum += uint32(binary.BigEndian.Uint16(packet[i:]))
	}
	for sum>>16 != 0 {
		sum = sum&0xffff + sum>>16
	}
	binary.BigEndian.PutUint16(packet[10:], ^uint16(sum))

	binary.BigEndian.PutUint16(packet[20:], uint16(srcPort))
	binary.BigEndian.PutUint16(packet[22:], uint16(dstPort))
	binary.BigEndian.PutUint16(packet[24:], uint16(8+len(payload)))
	copy(packet[28:], payload)
	return packet
}

// TestDevice drives a real adapter. It needs elevation and wintun.dll next to the test binary, and
// routes this machine's traffic into the adapter for a moment, so it only runs when asked.
func TestDevice(t *testing.T) {
	if os.Getenv("DESKCONN_VPN_INTEGRATION") == "" {
		t.Skip("set DESKCONN_VPN_INTEGRATION=1 to run (needs elevation and wintun.dll)")
	}

	before, err := GetDefaultRoute(4)
	require.NoError(t, err)

	device, err := Open("dtuntest")
	require.NoError(t, err)
	defer device.Close()

	packets := make(chan []byte, 256)
	go func() {
		defer close(packets)
		buf := make([]byte, 65536)
		for {
			n, err := device.Read(buf)
			if err != nil {
				return
			}
			select {
			case packets <- append([]byte(nil), buf[:n]...):
			default:
			}
		}
	}()

	require.NoError(t, device.Configure(testIP+"/24", 1200))
	ifRow := windows.MibIpInterfaceRow{Family: windows.AF_INET, InterfaceIndex: device.index}
	require.NoError(t, windows.GetIpInterfaceEntry(&ifRow))
	require.Equal(t, uint32(1200), ifRow.NlMtu)

	t.Run("packets go both ways", func(t *testing.T) {
		conn, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.ParseIP(testIP)})
		require.NoError(t, err)
		defer func() { _ = conn.Close() }()

		peer := &net.UDPAddr{IP: net.ParseIP(testPeerIP), Port: 40000}
		_, err = conn.WriteToUDP([]byte("out"), peer)
		require.NoError(t, err)
		require.Equal(t, "out", string(readUntil(t, packets, testPeerIP)[28:]))

		local := conn.LocalAddr().(*net.UDPAddr)
		require.NoError(t, device.Write(udpPacket(peer.IP, local.IP, peer.Port, local.Port, []byte("back"))))
		require.NoError(t, conn.SetReadDeadline(time.Now().Add(testTimeout)))
		buf := make([]byte, 16)
		n, _, err := conn.ReadFromUDP(buf)
		require.NoError(t, err)
		require.Equal(t, "back", string(buf[:n]))
	})

	t.Run("dns", func(t *testing.T) {
		require.NoError(t, device.SetDNS([]string{"1.1.1.1", "8.8.8.8"}))
	})

	t.Run("route all, except what's pinned", func(t *testing.T) {
		require.NoError(t, device.PinHostRoute(net.ParseIP(testPinned)))
		require.NoError(t, device.PinHostRoute(net.ParseIP(testPinned)), "already there is fine")
		require.NoError(t, device.RouteAll(6))
		require.NoError(t, device.RouteAll(4))

		routed, err := net.Dial("udp4", net.JoinHostPort(testRouted, "40000"))
		require.NoError(t, err)
		defer func() { _ = routed.Close() }()
		_, err = routed.Write([]byte("routed"))
		require.NoError(t, err)
		// Removed right away rather than at the end: this machine has no way out until then.
		device.Close()
		readUntilClosed := time.After(testTimeout)
		found := false
		for !found {
			select {
			case packet, ok := <-packets:
				if !ok {
					require.Fail(t, "no routed packet came out of the tunnel")
				}
				found = len(packet) >= 20 && net.IP(packet[16:20]).Equal(net.ParseIP(testRouted).To4())
			case <-readUntilClosed:
				require.Fail(t, "no routed packet came out of the tunnel")
			}
		}
		require.Empty(t, device.hostRoutes)
	})

	require.Eventually(t, func() bool {
		_, err := net.InterfaceByName("dtuntest")
		return err != nil
	}, testTimeout, 100*time.Millisecond, "adapter still there after closing")

	after, err := GetDefaultRoute(4)
	require.NoError(t, err)
	require.Equal(t, before, after)
}
