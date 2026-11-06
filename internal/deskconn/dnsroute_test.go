//go:build linux

package deskconn_test

import (
	"encoding/binary"
	"testing"

	"github.com/stretchr/testify/require"
	"golang.org/x/net/dns/dnsmessage"

	"github.com/xconnio/deskconn/internal/deskconn"
)

const resolverIP = "\x01\x01\x01\x01"

// dnsAnswerPacket builds an IPv4/UDP packet from 1.1.1.1:53 to 10.66.0.2 answering name with
// one A and one AAAA record.
func dnsAnswerPacket(t *testing.T, name string, srcPort uint16) []byte {
	t.Helper()
	msg := dnsmessage.Message{
		Header: dnsmessage.Header{ID: 7, Response: true, RecursionDesired: true, RecursionAvailable: true},
		Questions: []dnsmessage.Question{
			{Name: dnsmessage.MustNewName(name), Type: dnsmessage.TypeA, Class: dnsmessage.ClassINET},
		},
		Answers: []dnsmessage.Resource{
			{
				Header: dnsmessage.ResourceHeader{Name: dnsmessage.MustNewName(name), Class: dnsmessage.ClassINET, TTL: 60},
				Body:   &dnsmessage.AResource{A: [4]byte{142, 250, 200, 174}},
			},
			{
				Header: dnsmessage.ResourceHeader{Name: dnsmessage.MustNewName(name), Class: dnsmessage.ClassINET, TTL: 60},
				Body:   &dnsmessage.AAAAResource{AAAA: [16]byte{0x2a, 0x00, 15: 1}},
			},
		},
	}
	payload, err := msg.Pack()
	require.NoError(t, err)

	pkt := make([]byte, 28)
	pkt[0], pkt[8], pkt[9] = 0x45, 64, 17
	copy(pkt[12:16], resolverIP)
	copy(pkt[16:20], []byte{10, 66, 0, 2})
	binary.BigEndian.PutUint16(pkt[20:22], srcPort)
	binary.BigEndian.PutUint16(pkt[22:24], 40000)
	return deskconn.WithUDPPayload(pkt, 20, payload)
}

// requireValidChecksums checks pkt's IPv4 header and UDP checksums.
func requireValidChecksums(t *testing.T, pkt []byte) {
	t.Helper()
	require.Equal(t, uint16(0), deskconn.Checksum(0, pkt[:20]), "ip header checksum")
	pseudo := make([]byte, 12)
	copy(pseudo, pkt[12:20])
	pseudo[9] = 17
	binary.BigEndian.PutUint16(pseudo[10:], uint16(len(pkt)-20))
	require.Equal(t, uint16(0), deskconn.Checksum(deskconn.Checksum(0, pseudo)^0xffff, pkt[20:]), "udp checksum")
}

func TestDNSRouterRoutesMatchingAnswers(t *testing.T) {
	var added []string
	router := deskconn.NewDNSRouter([]string{"google.com"}, func(cidr string) error {
		added = append(added, cidr)
		return nil
	})

	pkt := dnsAnswerPacket(t, "www.google.com.", 53)
	requireValidChecksums(t, pkt)
	out := router.Filter(pkt)

	require.Equal(t, []string{"142.250.200.174/32"}, added)
	requireValidChecksums(t, out)
	require.Equal(t, uint16(len(out)), binary.BigEndian.Uint16(out[2:4]))

	var msg dnsmessage.Message
	require.NoError(t, msg.Unpack(out[28:]))
	require.Equal(t, uint16(7), msg.ID)
	require.Len(t, msg.Answers, 1, "the AAAA answer is left out")
	require.Equal(t, dnsmessage.TypeA, msg.Answers[0].Header.Type)

	// The same address again isn't routed twice; after stop nothing new is.
	router.Filter(dnsAnswerPacket(t, "google.com.", 53))
	require.Len(t, added, 1)
	require.Equal(t, []string{"142.250.200.174/32"}, router.Stop())
}

func TestDNSRouterIgnoresOtherPackets(t *testing.T) {
	router := deskconn.NewDNSRouter([]string{"google.com"}, func(string) error {
		t.Fatal("nothing should be routed")
		return nil
	})

	for _, pkt := range [][]byte{
		// another domain
		dnsAnswerPacket(t, "example.com.", 53),
		// only a suffix match
		dnsAnswerPacket(t, "notgoogle.com.", 53),
		// not from port 53
		dnsAnswerPacket(t, "www.google.com.", 443),
		// truncated
		{0x45, 0, 0, 20},
		nil,
	} {
		require.Equal(t, pkt, router.Filter(pkt))
	}

	// Without domains even matching answers pass straight through.
	pkt := dnsAnswerPacket(t, "www.google.com.", 53)
	require.Equal(t, pkt, deskconn.NewDNSRouter(nil, nil).Filter(pkt))
}
