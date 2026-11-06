//go:build windows

package userd

import (
	"encoding/binary"
	"fmt"
	"net"
	"unsafe"

	log "github.com/sirupsen/logrus"
	"golang.org/x/sys/windows"
	"gvisor.dev/gvisor/pkg/buffer"
	"gvisor.dev/gvisor/pkg/tcpip/stack"
)

// The netstack only carries TCP and UDP, so the exit node answers pings itself: it sends each
// echo request on with IcmpSendEcho, which unlike a raw socket needs no elevation, and writes the
// reply back as the packet a Linux exit node would have forwarded.

const (
	icmpProtocol    = 1
	icmpEchoReply   = 0
	icmpEchoRequest = 8

	icmpTimeoutMillis = 4000

	// icmpMaxInFlight bounds the pings waiting on a reply at once, as each holds a goroutine.
	icmpMaxInFlight = 64

	ipSuccess = 0
)

//nolint:gochecknoglobals
var (
	modIphlpapi         = windows.NewLazySystemDLL("iphlpapi.dll")
	procIcmpCreateFile  = modIphlpapi.NewProc("IcmpCreateFile")
	procIcmpCloseHandle = modIphlpapi.NewProc("IcmpCloseHandle")
	procIcmpSendEcho    = modIphlpapi.NewProc("IcmpSendEcho")
)

// ipOptionInformation is IP_OPTION_INFORMATION.
type ipOptionInformation struct {
	TTL         uint8
	TOS         uint8
	Flags       uint8
	OptionsSize uint8
	OptionsData uintptr
}

// icmpEchoReplyHeader is ICMP_ECHO_REPLY, which the reply's data follows in the reply buffer.
type icmpEchoReplyHeader struct {
	Address       uint32
	Status        uint32
	RoundTripTime uint32
	DataSize      uint16
	Reserved      uint16
	Data          uintptr
	Options       ipOptionInformation
}

// echoRequest is an ICMP echo request from the client.
type echoRequest struct {
	dst      net.IP
	ttl      uint8
	id, seq  uint16
	payload  []byte
	clientIP net.IP
}

// parseEchoRequest returns packet as an echo request, if it's an unfragmented one.
func parseEchoRequest(packet []byte) (*echoRequest, bool) {
	if len(packet) < 20 || packet[0]>>4 != 4 || packet[9] != icmpProtocol {
		return nil, false
	}
	headerLen := int(packet[0]&0x0f) * 4
	totalLen := int(binary.BigEndian.Uint16(packet[2:]))
	fragment := binary.BigEndian.Uint16(packet[6:]) & 0x3fff // more fragments, and the offset
	if headerLen < 20 || totalLen > len(packet) || totalLen < headerLen+8 || fragment != 0 {
		return nil, false
	}
	icmp := packet[headerLen:totalLen]
	if icmp[0] != icmpEchoRequest || icmp[1] != 0 {
		return nil, false
	}
	return &echoRequest{
		dst:      net.IP(append([]byte(nil), packet[16:20]...)),
		ttl:      packet[8],
		id:       binary.BigEndian.Uint16(icmp[4:]),
		seq:      binary.BigEndian.Uint16(icmp[6:]),
		payload:  append([]byte(nil), icmp[8:]...),
		clientIP: net.IP(append([]byte(nil), packet[12:16]...)),
	}, true
}

// checksum is the internet checksum of data.
func checksum(data []byte) uint16 {
	var sum uint32
	for i := 0; i+1 < len(data); i += 2 {
		sum += uint32(binary.BigEndian.Uint16(data[i:]))
	}
	if len(data)%2 == 1 {
		sum += uint32(data[len(data)-1]) << 8
	}
	for sum>>16 != 0 {
		sum = sum&0xffff + sum>>16
	}
	return ^uint16(sum)
}

// echoReplyPacket builds the reply to req, from req.dst, as received with ttl.
func echoReplyPacket(req *echoRequest, ttl uint8, payload []byte) []byte {
	packet := make([]byte, 28+len(payload))
	packet[0] = 0x45
	binary.BigEndian.PutUint16(packet[2:], uint16(len(packet))) //nolint:gosec // within the MTU
	packet[8] = ttl
	packet[9] = icmpProtocol
	copy(packet[12:], req.dst.To4())
	copy(packet[16:], req.clientIP.To4())
	binary.BigEndian.PutUint16(packet[10:], checksum(packet[:20]))

	icmp := packet[20:]
	icmp[0] = icmpEchoReply
	binary.BigEndian.PutUint16(icmp[4:], req.id)
	binary.BigEndian.PutUint16(icmp[6:], req.seq)
	copy(icmp[8:], payload)
	binary.BigEndian.PutUint16(icmp[2:], checksum(icmp))
	return packet
}

// icmpPinger sends a session's pings.
type icmpPinger struct {
	handle   uintptr
	inFlight chan struct{}
}

func newICMPPinger() (*icmpPinger, error) {
	handle, _, err := procIcmpCreateFile.Call()
	if handle == uintptr(windows.InvalidHandle) {
		return nil, fmt.Errorf("IcmpCreateFile: %w", err)
	}
	return &icmpPinger{handle: handle, inFlight: make(chan struct{}, icmpMaxInFlight)}, nil
}

// close must only be called once no ping is in flight.
func (p *icmpPinger) close() {
	_, _, _ = procIcmpCloseHandle.Call(p.handle)
}

// echo sends req and returns the reply's TTL and data, or false if none came. TTL goes down by
// one on the way through, as it would through a Linux exit node.
func (p *icmpPinger) echo(req *echoRequest) (ttl uint8, payload []byte, ok bool) {
	if req.ttl <= 1 {
		return 0, nil, false
	}
	options := ipOptionInformation{TTL: req.ttl - 1}
	replySize := int(unsafe.Sizeof(icmpEchoReplyHeader{})) + len(req.payload) + 8 + 64
	reply := make([]byte, replySize)
	var data uintptr
	if len(req.payload) > 0 {
		data = uintptr(unsafe.Pointer(&req.payload[0]))
	}

	dst := req.dst.To4()
	n, _, _ := procIcmpSendEcho.Call(
		p.handle,
		uintptr(binary.LittleEndian.Uint32(dst)), // IPAddr, in network order in memory
		data,
		uintptr(len(req.payload)),
		uintptr(unsafe.Pointer(&options)),
		uintptr(unsafe.Pointer(&reply[0])),
		uintptr(replySize),
		icmpTimeoutMillis,
	)
	if n == 0 {
		return 0, nil, false
	}
	header := (*icmpEchoReplyHeader)(unsafe.Pointer(&reply[0]))
	if header.Status != ipSuccess {
		return 0, nil, false
	}
	start := int(header.Data - uintptr(unsafe.Pointer(&reply[0])))
	end := start + int(header.DataSize)
	if start < 0 || end > len(reply) {
		return 0, nil, false
	}
	ttl = header.Options.TTL
	if ttl > 1 {
		ttl--
	}
	return ttl, append([]byte(nil), reply[start:end]...), true
}

// handleEcho answers packet if it's an echo request, reporting whether it was one. The reply goes
// out through the netstack's link, so it reaches the client the way everything else does.
func (s *vpnTunnelSession) handleEcho(packet []byte) bool {
	req, ok := parseEchoRequest(packet)
	if !ok {
		return false
	}
	if s.pinger == nil || !s.allowed(req.dst) {
		return true
	}
	select {
	case s.pinger.inFlight <- struct{}{}:
	default:
		return true // too many waiting: dropped, like on a congested link
	}

	s.echoMu.Lock()
	if s.echoClosed {
		s.echoMu.Unlock()
		<-s.pinger.inFlight
		return true
	}
	s.wg.Add(1)
	s.echoMu.Unlock()
	go func() {
		defer s.wg.Done()
		defer func() { <-s.pinger.inFlight }()

		ttl, payload, ok := s.pinger.echo(req)
		if !ok || s.ctx.Err() != nil {
			return
		}
		pkt := stack.NewPacketBuffer(stack.PacketBufferOptions{
			Payload: buffer.MakeWithData(echoReplyPacket(req, ttl, payload)),
		})
		var list stack.PacketBufferList
		list.PushBack(pkt)
		if _, err := s.endpoint.WritePackets(list); err != nil {
			log.Debugf("iptunnel: could not send echo reply: %v", err)
		}
		pkt.DecRef()
	}()
	return true
}
