//go:build linux

package deskconn

import (
	"encoding/binary"
	"net/netip"
	"sync"

	log "github.com/sirupsen/logrus"
	"golang.org/x/net/dns/dnsmessage"

	"github.com/xconnio/deskconn/internal/common"
)

// DNSRouter routes domains through a tunnel by their addresses, learned from the DNS answers
// coming back through it (lookups of domains are sent there, see routeDomains): each IPv4
// address answered for one of domains gets a route through the tunnel before the answer is
// passed on, so whatever asked connects through the tunnel from its first packet. Routes stay
// until Stop, rather than expiring with the answers' TTLs, so connections already open keep
// working.
type DNSRouter struct {
	domains  []string
	addRoute func(cidr string) error

	mu      sync.Mutex
	routed  map[netip.Addr]struct{}
	stopped bool
}

// NewDNSRouter returns a DNSRouter for domains, adding each route with addRoute.
func NewDNSRouter(domains []string, addRoute func(cidr string) error) *DNSRouter {
	return &DNSRouter{domains: domains, addRoute: addRoute, routed: map[netip.Addr]struct{}{}}
}

// Filter takes a packet arriving from the tunnel and returns it to be written to the TUN
// device. For a DNS answer about one of domains it routes the IPv4 addresses first, and
// leaves out the AAAA and SVCB/HTTPS records: the tunnel only carries IPv4, so those would
// send connections around it. Everything else is returned as it is.
func (r *DNSRouter) Filter(pkt []byte) []byte {
	if len(r.domains) == 0 {
		return pkt
	}
	ihl, payload, ok := dnsResponsePayload(pkt)
	if !ok {
		return pkt
	}
	var msg dnsmessage.Message
	if err := msg.Unpack(payload); err != nil || !msg.Response || len(msg.Questions) == 0 ||
		!common.MatchVPNDomain(msg.Questions[0].Name.String(), r.domains) {
		return pkt
	}

	var addrs []netip.Addr
	kept := make([]dnsmessage.Resource, 0, len(msg.Answers))
	for _, answer := range msg.Answers {
		switch answer.Header.Type {
		case dnsmessage.TypeA:
			if a, ok := answer.Body.(*dnsmessage.AResource); ok {
				addrs = append(addrs, netip.AddrFrom4(a.A))
			}
		case dnsmessage.TypeAAAA, dnsmessage.TypeSVCB, dnsmessage.TypeHTTPS:
			continue
		}
		kept = append(kept, answer)
	}
	r.route(addrs)

	if len(kept) == len(msg.Answers) {
		return pkt
	}
	msg.Answers = kept
	packed, err := msg.Pack()
	if err != nil {
		log.Debugf("iptunnel: could not repack dns answer for %s: %v", msg.Questions[0].Name, err)
		return pkt
	}
	return WithUDPPayload(pkt, ihl, packed)
}

// route adds a tunnel route for each of addrs not routed yet. One that fails (e.g. the exit
// node's own address, already pinned outside the tunnel) isn't retried.
func (r *DNSRouter) route(addrs []netip.Addr) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, addr := range addrs {
		if _, done := r.routed[addr]; done || r.stopped {
			continue
		}
		r.routed[addr] = struct{}{}
		if err := r.addRoute(netip.PrefixFrom(addr, 32).String()); err != nil {
			log.Debugf("iptunnel: could not route %s through the tunnel: %v", addr, err)
		}
	}
}

// Stop makes route do nothing from now on and returns the routes it added, for removal.
func (r *DNSRouter) Stop() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.stopped = true
	cidrs := make([]string, 0, len(r.routed))
	for addr := range r.routed {
		cidrs = append(cidrs, netip.PrefixFrom(addr, 32).String())
	}
	return cidrs
}

// dnsResponsePayload returns the IPv4 header length and UDP payload of pkt if it's an
// unfragmented IPv4 UDP packet from port 53.
func dnsResponsePayload(pkt []byte) (ihl int, payload []byte, ok bool) {
	if len(pkt) < 28 || pkt[0]>>4 != 4 || pkt[9] != 17 {
		return 0, nil, false
	}
	ihl = int(pkt[0]&0x0f) * 4
	total := int(binary.BigEndian.Uint16(pkt[2:4]))
	if ihl < 20 || total > len(pkt) || total < ihl+8 || binary.BigEndian.Uint16(pkt[6:8])&0x3fff != 0 {
		return 0, nil, false
	}
	udp := pkt[ihl:total]
	udpLen := int(binary.BigEndian.Uint16(udp[4:6]))
	if binary.BigEndian.Uint16(udp[0:2]) != 53 || udpLen < 8 || udpLen > len(udp) {
		return 0, nil, false
	}
	return ihl, udp[8:udpLen], true
}

// WithUDPPayload returns a copy of the IPv4 UDP packet pkt (header length ihl) carrying
// payload instead, with its lengths and checksums updated.
func WithUDPPayload(pkt []byte, ihl int, payload []byte) []byte {
	out := make([]byte, ihl+8+len(payload))
	copy(out, pkt[:ihl+8])
	copy(out[ihl+8:], payload)
	udpLen := 8 + len(payload)

	binary.BigEndian.PutUint16(out[2:4], uint16(len(out))) //nolint:gosec // bounded by the MTU
	binary.BigEndian.PutUint16(out[10:12], 0)
	binary.BigEndian.PutUint16(out[10:12], Checksum(0, out[:ihl]))

	udp := out[ihl:]
	binary.BigEndian.PutUint16(udp[4:6], uint16(udpLen)) //nolint:gosec // bounded by the MTU
	binary.BigEndian.PutUint16(udp[6:8], 0)
	pseudo := make([]byte, 12)
	copy(pseudo[0:8], out[12:20]) // source and destination addresses
	pseudo[9] = 17
	binary.BigEndian.PutUint16(pseudo[10:12], uint16(udpLen)) //nolint:gosec // bounded by the MTU
	sum := Checksum(Checksum(0, pseudo)^0xffff, udp)
	if sum == 0 {
		sum = 0xffff // 0 means "no checksum" in UDP over IPv4
	}
	binary.BigEndian.PutUint16(udp[6:8], sum)
	return out
}

// Checksum returns the Internet checksum (RFC 1071) of data, continuing from initial, the
// complement of an earlier checksum (0 to start).
func Checksum(initial uint16, data []byte) uint16 {
	sum := uint32(initial)
	for i := 0; i+1 < len(data); i += 2 {
		sum += uint32(binary.BigEndian.Uint16(data[i:]))
	}
	if len(data)%2 == 1 {
		sum += uint32(data[len(data)-1]) << 8
	}
	for sum > 0xffff {
		sum = (sum >> 16) + (sum & 0xffff)
	}
	return ^uint16(sum)
}
