package common

import (
	"os"

	log "github.com/sirupsen/logrus"
)

// VPNOpenFrame is the client's first message on a VPN data channel.
type VPNOpenFrame struct {
	Type string `json:"type"`
}

// VPNReadyFrame is the server's reply once tunnel/NAT setup on its side has
// succeeded and it's safe for the client to start routing traffic into it.
type VPNReadyFrame struct {
	Type       string `json:"type"`
	ServerIP   string `json:"server_ip"`
	ClientCIDR string `json:"client_cidr"`
	MTU        int    `json:"mtu"`
}

// PumpTUNToChannel reads raw IP packets from tun and sends each as one
// binary message, blocking on channel's own backpressure rather than
// buffering unboundedly. Shared by both server and client.
func PumpTUNToChannel(tun *os.File, channel MessageChannel, done <-chan struct{}) {
	sendReady := make(chan struct{}, 1)
	channel.SetBufferedAmountLowThreshold(vpnSendBufferLow)
	channel.OnBufferedAmountLow(func() {
		select {
		case sendReady <- struct{}{}:
		default:
		}
	})

	buf := make([]byte, 65536)
	for {
		n, err := tun.Read(buf)
		if err != nil {
			select {
			case <-done:
			default:
				log.Debugf("iptunnel: tun read failed: %v", err)
			}
			return
		}
		if n == 0 {
			continue
		}

		// n is non-negative by construction (checked above).
		for channel.BufferedAmount()+uint64(n) > vpnSendBufferHigh { //nolint:gosec
			select {
			case <-sendReady:
			case <-done:
				return
			}
		}

		pkt := make([]byte, n)
		copy(pkt, buf[:n])
		if err := channel.Send(pkt); err != nil {
			return
		}
	}
}
