//go:build windows

package userd

import (
	"context"
	"encoding/binary"
	"io"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"gvisor.dev/gvisor/pkg/buffer"
	"gvisor.dev/gvisor/pkg/tcpip"
	"gvisor.dev/gvisor/pkg/tcpip/adapters/gonet"
	"gvisor.dev/gvisor/pkg/tcpip/header"
	"gvisor.dev/gvisor/pkg/tcpip/link/channel"
	"gvisor.dev/gvisor/pkg/tcpip/network/ipv4"
	"gvisor.dev/gvisor/pkg/tcpip/stack"
	"gvisor.dev/gvisor/pkg/tcpip/transport/tcp"
	"gvisor.dev/gvisor/pkg/tcpip/transport/udp"

	"github.com/xconnio/deskconn/internal/common"
	"github.com/xconnio/xconn/bindings/go/webrtc"
)

// fakeChannel hands packets the server sends to onSend.
type fakeChannel struct {
	mu        sync.Mutex
	onMessage func(webrtc.DataChannelMessage)
	onSend    func([]byte)
	text      chan string
}

func (c *fakeChannel) Send(data []byte) error               { c.onSend(data); return nil }
func (c *fakeChannel) SendText(s string) error              { c.text <- s; return nil }
func (c *fakeChannel) OnClose(func())                       {}
func (c *fakeChannel) OnError(func(error))                  {}
func (c *fakeChannel) Close() error                         { return nil }
func (c *fakeChannel) BufferedAmount() uint64               { return 0 }
func (c *fakeChannel) SetBufferedAmountLowThreshold(uint64) {}
func (c *fakeChannel) OnBufferedAmountLow(func())           {}
func (c *fakeChannel) OnMessage(f func(webrtc.DataChannelMessage)) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.onMessage = f
}

func (c *fakeChannel) deliver(data []byte) {
	c.mu.Lock()
	f := c.onMessage
	c.mu.Unlock()
	f(webrtc.DataChannelMessage{Data: data})
}

// startClient runs a tunnel server and a netstack playing the VPN client on its far side.
func startClient(t *testing.T, allowed func(net.IP) bool) *stack.Stack {
	t.Helper()
	client := stack.New(stack.Options{
		NetworkProtocols:   []stack.NetworkProtocolFactory{ipv4.NewProtocol},
		TransportProtocols: []stack.TransportProtocolFactory{tcp.NewProtocol, udp.NewProtocol},
	})
	endpoint := channel.New(1024, 1200, "")
	require.Nil(t, client.CreateNIC(1, endpoint))
	require.Nil(t, client.AddProtocolAddress(1, tcpip.ProtocolAddress{
		Protocol:          header.IPv4ProtocolNumber,
		AddressWithPrefix: tcpip.AddrFrom4([4]byte{10, 66, 0, 2}).WithPrefix(),
	}, stack.AddressProperties{}))
	client.SetRouteTable([]tcpip.Route{{Destination: header.IPv4EmptySubnet, NIC: 1}})

	fake := &fakeChannel{text: make(chan string, 1)}
	fake.onSend = func(data []byte) {
		pkt := stack.NewPacketBuffer(stack.PacketBufferOptions{Payload: buffer.MakeWithData(data)})
		endpoint.InjectInbound(header.IPv4ProtocolNumber, pkt)
		pkt.DecRef()
	}
	sess, err := startVPNTunnelServer(fake, allowed)
	require.NoError(t, err)
	require.Contains(t, <-fake.text, `"vpn-ready"`)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			pkt := endpoint.ReadContext(ctx)
			if pkt == nil {
				return
			}
			view := pkt.ToView()
			fake.deliver(append([]byte(nil), view.AsSlice()...))
			view.Release()
			pkt.DecRef()
		}
	}()
	t.Cleanup(func() {
		cancel()
		<-done
		sess.close()
		client.Close()
		client.Wait()
	})
	return client
}

// hostIP returns the address this machine's default route uses, a test target that a packet can
// address, which the netstack wouldn't accept for 127.0.0.1. Dialing UDP sends nothing.
func hostIP(t *testing.T) net.IP {
	t.Helper()
	conn, err := net.Dial("udp4", "1.1.1.1:53")
	if err != nil {
		t.Skipf("no default route: %v", err)
	}
	defer func() { _ = conn.Close() }()
	return conn.LocalAddr().(*net.UDPAddr).IP.To4()
}

func allowAll(net.IP) bool { return true }

func fullAddress(ip net.IP, port int) tcpip.FullAddress {
	return tcpip.FullAddress{NIC: 1, Addr: tcpip.AddrFromSlice(ip), Port: uint16(port)}
}

func TestVPNForwardsTCP(t *testing.T) {
	ip := hostIP(t)
	listener, err := net.Listen("tcp4", net.JoinHostPort(ip.String(), "0"))
	require.NoError(t, err)
	defer func() { _ = listener.Close() }()
	go func() {
		conn, err := listener.Accept()
		if err != nil {
			return
		}
		defer func() { _ = conn.Close() }()
		_, _ = io.Copy(conn, conn)
	}()

	client := startClient(t, allowAll)
	port := listener.Addr().(*net.TCPAddr).Port
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	conn, err := gonet.DialContextTCP(ctx, client, fullAddress(ip, port), header.IPv4ProtocolNumber)
	require.NoError(t, err)
	defer func() { _ = conn.Close() }()

	// Bigger than the MTU and the send buffers, so it has to survive segmenting.
	payload := make([]byte, 1<<20)
	for i := range payload {
		payload[i] = byte(i * 7)
	}
	go func() { _, _ = conn.Write(payload) }()
	require.NoError(t, conn.SetReadDeadline(time.Now().Add(30*time.Second)))
	got := make([]byte, len(payload))
	_, err = io.ReadFull(conn, got)
	require.NoError(t, err)
	require.Equal(t, payload, got)
}

func TestVPNForwardsUDP(t *testing.T) {
	ip := hostIP(t)
	server, err := net.ListenUDP("udp4", &net.UDPAddr{IP: ip})
	require.NoError(t, err)
	defer func() { _ = server.Close() }()
	go func() {
		buf := make([]byte, 1500)
		for {
			n, from, err := server.ReadFromUDP(buf)
			if err != nil {
				return
			}
			_, _ = server.WriteToUDP(buf[:n], from)
		}
	}()

	client := startClient(t, allowAll)
	remote := fullAddress(ip, server.LocalAddr().(*net.UDPAddr).Port)
	conn, err := gonet.DialUDP(client, nil, &remote, header.IPv4ProtocolNumber)
	require.NoError(t, err)
	defer func() { _ = conn.Close() }()

	_, err = conn.Write([]byte("ping"))
	require.NoError(t, err)
	require.NoError(t, conn.SetReadDeadline(time.Now().Add(10*time.Second)))
	buf := make([]byte, 16)
	n, err := conn.Read(buf)
	require.NoError(t, err)
	require.Equal(t, "ping", string(buf[:n]))
}

func TestVPNRefusesThisMachine(t *testing.T) {
	ip := hostIP(t)
	listener, err := net.Listen("tcp4", net.JoinHostPort(ip.String(), "0"))
	require.NoError(t, err)
	defer func() { _ = listener.Close() }()

	client := startClient(t, vpnDestinationAllowed)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	port := listener.Addr().(*net.TCPAddr).Port
	_, err = gonet.DialContextTCP(ctx, client, fullAddress(ip, port), header.IPv4ProtocolNumber)
	require.Error(t, err)
	require.NoError(t, ctx.Err(), "should be refused right away, not time out")
}

func TestVPNDestinationAllowed(t *testing.T) {
	for _, blocked := range []string{"127.0.0.1", "0.0.0.0", "224.0.0.1", "255.255.255.255", "10.66.0.1"} {
		require.False(t, vpnDestinationAllowed(net.ParseIP(blocked)), blocked)
	}
	require.True(t, vpnDestinationAllowed(net.ParseIP("1.1.1.1")))
}

// echoRequestPacket builds an ICMP echo request from the VPN client to dst.
func echoRequestPacket(dst net.IP, id, seq uint16, payload []byte) []byte {
	packet := make([]byte, 28+len(payload))
	packet[0] = 0x45
	binary.BigEndian.PutUint16(packet[2:], uint16(len(packet)))
	packet[8] = 64
	packet[9] = icmpProtocol
	copy(packet[12:], net.ParseIP(common.VPNClientIP).To4())
	copy(packet[16:], dst.To4())
	binary.BigEndian.PutUint16(packet[10:], checksum(packet[:20]))
	icmp := packet[20:]
	icmp[0] = icmpEchoRequest
	binary.BigEndian.PutUint16(icmp[4:], id)
	binary.BigEndian.PutUint16(icmp[6:], seq)
	copy(icmp[8:], payload)
	binary.BigEndian.PutUint16(icmp[2:], checksum(icmp))
	return packet
}

// startRawServer runs a tunnel server whose outgoing packets come out of the returned channel.
func startRawServer(t *testing.T, allowed func(net.IP) bool) (*fakeChannel, <-chan []byte) {
	t.Helper()
	out := make(chan []byte, 64)
	fake := &fakeChannel{text: make(chan string, 1)}
	fake.onSend = func(data []byte) {
		select {
		case out <- append([]byte(nil), data...):
		default:
		}
	}
	sess, err := startVPNTunnelServer(fake, allowed)
	require.NoError(t, err)
	<-fake.text
	t.Cleanup(sess.close)
	return fake, out
}

func TestVPNAnswersPing(t *testing.T) {
	target := net.ParseIP("127.0.0.1")
	fake, out := startRawServer(t, allowAll)
	payload := []byte("deskconn ping payload")
	fake.deliver(echoRequestPacket(target, 0x1234, 7, payload))

	select {
	case reply := <-out:
		require.GreaterOrEqual(t, len(reply), 28)
		require.Zero(t, checksum(reply[:20]), "ip header checksum")
		require.Equal(t, byte(icmpProtocol), reply[9])
		require.True(t, net.IP(reply[12:16]).Equal(target), "from the target")
		require.True(t, net.IP(reply[16:20]).Equal(net.ParseIP(common.VPNClientIP)), "to the client")
		icmp := reply[20:]
		require.Equal(t, byte(icmpEchoReply), icmp[0])
		require.Zero(t, checksum(icmp), "icmp checksum")
		require.Equal(t, uint16(0x1234), binary.BigEndian.Uint16(icmp[4:]))
		require.Equal(t, uint16(7), binary.BigEndian.Uint16(icmp[6:]))
		require.Equal(t, payload, icmp[8:])
	case <-time.After(10 * time.Second):
		t.Fatal("no echo reply")
	}
}

func TestVPNRefusesPingToThisMachine(t *testing.T) {
	fake, out := startRawServer(t, vpnDestinationAllowed)
	fake.deliver(echoRequestPacket(net.ParseIP("127.0.0.1"), 1, 1, []byte("x")))
	select {
	case <-out:
		t.Fatal("answered a ping to this machine")
	case <-time.After(2 * time.Second):
	}
}
