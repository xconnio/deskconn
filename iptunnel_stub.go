//go:build !linux

package deskconn

// vpnServer is a stub on non-Linux platforms: the VPN/iptunnel feature depends on TUN devices
// and iptables via the iptun package, which is Linux-only (see iptunnel.go, iptun/). Only the
// type needs to exist here, for the Deskconn struct's "vpn" field to compile - nothing outside
// iptunnel.go ever reads it on this platform.
type vpnServer struct{}

func newVPNServer() *vpnServer { return &vpnServer{} }
