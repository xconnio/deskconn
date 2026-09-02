//go:build linux

package common

import (
	"encoding/json"
	"fmt"
	"net"
	"os"

	"golang.org/x/sys/unix"
)

// VPNHelperClient talks to a vpnd process (see deskconn.LaunchVPNHelper) over a
// unixpacket socket.
type VPNHelperClient struct {
	conn *net.UnixConn
}

// DialVPNHelper connects to a helper already listening on socketPath (see
// LaunchHelper, which starts the helper and returns this path once its
// listener is up).
func DialVPNHelper(socketPath string) (*VPNHelperClient, error) {
	conn, err := net.DialUnix("unixpacket", nil, &net.UnixAddr{Name: socketPath, Net: "unixpacket"})
	if err != nil {
		return nil, fmt.Errorf("dial vpn helper at %s: %w", socketPath, err)
	}
	return &VPNHelperClient{conn: conn}, nil
}

// Close ends the session with the helper, which causes it to unwind any
// changes still outstanding and exit -- see Server.Serve.
func (c *VPNHelperClient) Close() error {
	return c.conn.Close()
}

// Alive reports whether the helper is still on the other end of the
// connection, by peeking at it without blocking: a helper that exited (or
// was stopped) shows up as end-of-file. Only meaningful while no call is
// in flight, since it would otherwise race that call's response.
func (c *VPNHelperClient) Alive() bool {
	raw, err := c.conn.SyscallConn()
	if err != nil {
		return false
	}

	alive := false
	ctrlErr := raw.Control(func(fd uintptr) {
		buf := make([]byte, 1)
		n, _, err := unix.Recvfrom(int(fd), buf, unix.MSG_PEEK|unix.MSG_DONTWAIT)
		alive = (err == unix.EAGAIN || err == unix.EWOULDBLOCK) || (err == nil && n > 0) //nolint:errorlint
	})
	return ctrlErr == nil && alive
}

func (c *VPNHelperClient) call(op VPNHelperOp, args any) (VPNHelperResponse, int, error) {
	var argData json.RawMessage
	if args != nil {
		data, err := json.Marshal(args)
		if err != nil {
			return VPNHelperResponse{}, -1, err
		}
		argData = data
	}

	req, err := json.Marshal(VPNHelperRequest{Op: op, Args: argData})
	if err != nil {
		return VPNHelperResponse{}, -1, err
	}
	if _, _, err := c.conn.WriteMsgUnix(req, nil, nil); err != nil {
		return VPNHelperResponse{}, -1, fmt.Errorf("send %s request: %w", op, err)
	}

	buf := make([]byte, VPNHelperBufSize)
	oob := make([]byte, unix.CmsgSpace(4))
	n, oobn, _, _, err := c.conn.ReadMsgUnix(buf, oob)
	if err != nil {
		return VPNHelperResponse{}, -1, fmt.Errorf("read %s response: %w", op, err)
	}

	var resp VPNHelperResponse
	if err := json.Unmarshal(buf[:n], &resp); err != nil {
		return VPNHelperResponse{}, -1, fmt.Errorf("malformed %s response: %w", op, err)
	}
	if !resp.OK {
		msg := resp.Error
		if msg == "" {
			msg = "unknown error"
		}
		return VPNHelperResponse{}, -1, fmt.Errorf("%s: %s", op, msg)
	}

	fd := -1
	if oobn > 0 {
		fd, err = parseRightsFD(oob[:oobn])
		if err != nil {
			return VPNHelperResponse{}, -1, fmt.Errorf("%s: %w", op, err)
		}
	}

	return resp, fd, nil
}

func parseRightsFD(oob []byte) (int, error) {
	msgs, err := unix.ParseSocketControlMessage(oob)
	if err != nil {
		return -1, fmt.Errorf("parse control message: %w", err)
	}
	for _, msg := range msgs {
		fds, err := unix.ParseUnixRights(&msg)
		if err != nil {
			continue
		}
		if len(fds) > 0 {
			return fds[0], nil
		}
	}
	return -1, fmt.Errorf("no file descriptor in response")
}

// OpenTUN mirrors the package-level OpenTUN, but has the helper create the
// device and hands back the resulting file descriptor.
func (c *VPNHelperClient) OpenTUN(name string) (tun *os.File, ifaceName string, err error) {
	resp, fd, err := c.call(VPNOpOpenTUN, VPNOpenTUNArgs{Name: name})
	if err != nil {
		return nil, "", err
	}
	if fd < 0 {
		return nil, "", fmt.Errorf("open_tun: helper did not return a file descriptor")
	}

	var data VPNOpenTUNData
	if err := json.Unmarshal(resp.Data, &data); err != nil {
		_ = unix.Close(fd)
		return nil, "", fmt.Errorf("open_tun: malformed response: %w", err)
	}

	return os.NewFile(uintptr(fd), "/dev/net/tun"), data.Iface, nil
}

// ConfigureTUNAddress mirrors the package-level ConfigureTUNAddress.
func (c *VPNHelperClient) ConfigureTUNAddress(iface, cidr string, mtu int) error {
	_, _, err := c.call(VPNOpConfigureTUN, VPNConfigureTUNArgs{Iface: iface, CIDR: cidr, MTU: mtu})
	return err
}

// AddHostRoute mirrors the package-level AddHostRoute.
func (c *VPNHelperClient) AddHostRoute(ip, gateway, iface string) error {
	_, _, err := c.call(VPNOpAddHostRoute, VPNAddHostRouteArgs{IP: ip, Gateway: gateway, Iface: iface})
	return err
}

// DelHostRoute mirrors the package-level DelHostRoute.
func (c *VPNHelperClient) DelHostRoute(ip string) error {
	_, _, err := c.call(VPNOpDelHostRoute, VPNDelHostRouteArgs{IP: ip})
	return err
}

// ReplaceDefaultRoute mirrors the package-level ReplaceDefaultRoute.
func (c *VPNHelperClient) ReplaceDefaultRoute(ipVersion int, iface string) (*DefaultRoute, error) {
	resp, _, err := c.call(VPNOpReplaceDefaultRoute, VPNReplaceDefaultRouteArgs{IPVersion: ipVersion, Iface: iface})
	if err != nil {
		return nil, err
	}
	var data VPNReplaceDefaultRouteData
	if err := json.Unmarshal(resp.Data, &data); err != nil {
		return nil, fmt.Errorf("replace_default_route: malformed response: %w", err)
	}
	return data.Prev, nil
}

// RestoreDefaultRoute mirrors the package-level RestoreDefaultRoute.
func (c *VPNHelperClient) RestoreDefaultRoute(ipVersion int, prev *DefaultRoute) error {
	_, _, err := c.call(VPNOpRestoreDefaultRoute, VPNRestoreDefaultRouteArgs{IPVersion: ipVersion, Prev: prev})
	return err
}

// BlockIPv6Default mirrors the package-level BlockIPv6Default.
func (c *VPNHelperClient) BlockIPv6Default() (hadDefault bool, prev *DefaultRoute, err error) {
	resp, _, err := c.call(VPNOpBlockIPv6Default, nil)
	if err != nil {
		return false, nil, err
	}
	var data VPNBlockIPv6DefaultData
	if err := json.Unmarshal(resp.Data, &data); err != nil {
		return false, nil, fmt.Errorf("block_ipv6_default: malformed response: %w", err)
	}
	return data.HadDefault, data.Prev, nil
}

// RestoreIPv6Default mirrors the package-level RestoreIPv6Default.
func (c *VPNHelperClient) RestoreIPv6Default(hadDefault bool, prev *DefaultRoute) error {
	_, _, err := c.call(VPNOpRestoreIPv6Default, VPNRestoreIPv6DefaultArgs{HadDefault: hadDefault, Prev: prev})
	return err
}

// SetLinkDNS mirrors the package-level SetLinkDNS.
func (c *VPNHelperClient) SetLinkDNS(iface string, servers []string) error {
	_, _, err := c.call(VPNOpSetLinkDNS, VPNSetLinkDNSArgs{Iface: iface, Servers: servers})
	return err
}

// RevertLinkDNS mirrors the package-level RevertLinkDNS.
func (c *VPNHelperClient) RevertLinkDNS(iface string) error {
	_, _, err := c.call(VPNOpRevertLinkDNS, VPNRevertLinkDNSArgs{Iface: iface})
	return err
}

// SetSysctl mirrors the package-level SetSysctl, setting key to value.
func (c *VPNHelperClient) SetSysctl(key, value string) (previous string, err error) {
	resp, _, err := c.call(VPNOpSetSysctl, VPNSetSysctlArgs{Key: key, Value: value})
	if err != nil {
		return "", err
	}
	var data VPNSetSysctlData
	if err := json.Unmarshal(resp.Data, &data); err != nil {
		return "", fmt.Errorf("set_sysctl: malformed response: %w", err)
	}
	return data.Previous, nil
}

// RestoreSysctl sets key back to value (its value before a prior SetSysctl
// call).
func (c *VPNHelperClient) RestoreSysctl(key, value string) error {
	_, _, err := c.call(VPNOpRestoreSysctl, VPNSetSysctlArgs{Key: key, Value: value})
	return err
}

// AddMasquerade mirrors the package-level AddMasquerade.
func (c *VPNHelperClient) AddMasquerade(subnet, oif string) error {
	_, _, err := c.call(VPNOpAddMasquerade, VPNMasqueradeArgs{Subnet: subnet, Oif: oif})
	return err
}

// DelMasquerade mirrors the package-level DelMasquerade.
func (c *VPNHelperClient) DelMasquerade(subnet, oif string) error {
	_, _, err := c.call(VPNOpDelMasquerade, VPNMasqueradeArgs{Subnet: subnet, Oif: oif})
	return err
}

// AddForwardAccept mirrors the package-level AddForwardAccept.
func (c *VPNHelperClient) AddForwardAccept(inIface, outIface string) error {
	_, _, err := c.call(VPNOpAddForwardAccept, VPNForwardArgs{InIface: inIface, OutIface: outIface})
	return err
}

// DelForwardAccept mirrors the package-level DelForwardAccept.
func (c *VPNHelperClient) DelForwardAccept(inIface, outIface string) error {
	_, _, err := c.call(VPNOpDelForwardAccept, VPNForwardArgs{InIface: inIface, OutIface: outIface})
	return err
}

// AddForwardEstablished mirrors the package-level AddForwardEstablished.
func (c *VPNHelperClient) AddForwardEstablished(inIface, outIface string) error {
	_, _, err := c.call(VPNOpAddForwardEstablished, VPNForwardArgs{InIface: inIface, OutIface: outIface})
	return err
}

// DelForwardEstablished mirrors the package-level DelForwardEstablished.
func (c *VPNHelperClient) DelForwardEstablished(inIface, outIface string) error {
	_, _, err := c.call(VPNOpDelForwardEstablished, VPNForwardArgs{InIface: inIface, OutIface: outIface})
	return err
}
