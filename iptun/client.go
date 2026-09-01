package iptun

import (
	"encoding/json"
	"fmt"
	"net"
	"os"

	"golang.org/x/sys/unix"
)

// Client talks to a deskconn-vpnd process (see LaunchHelper) over a
// unixpacket socket.
type Client struct {
	conn *net.UnixConn
}

// DialClient connects to a helper already listening on socketPath (see
// LaunchHelper, which starts the helper and returns this path once its
// listener is up).
func DialClient(socketPath string) (*Client, error) {
	conn, err := net.DialUnix("unixpacket", nil, &net.UnixAddr{Name: socketPath, Net: "unixpacket"})
	if err != nil {
		return nil, fmt.Errorf("dial vpn helper at %s: %w", socketPath, err)
	}
	return &Client{conn: conn}, nil
}

// Close ends the session with the helper, which causes it to unwind any
// changes still outstanding and exit -- see Server.Serve.
func (c *Client) Close() error {
	return c.conn.Close()
}

func (c *Client) call(op rpcOp, args any) (rpcResponse, int, error) {
	var argData json.RawMessage
	if args != nil {
		data, err := json.Marshal(args)
		if err != nil {
			return rpcResponse{}, -1, err
		}
		argData = data
	}

	req, err := json.Marshal(rpcRequest{Op: op, Args: argData})
	if err != nil {
		return rpcResponse{}, -1, err
	}
	if _, _, err := c.conn.WriteMsgUnix(req, nil, nil); err != nil {
		return rpcResponse{}, -1, fmt.Errorf("send %s request: %w", op, err)
	}

	buf := make([]byte, rpcBufSize)
	oob := make([]byte, unix.CmsgSpace(4))
	n, oobn, _, _, err := c.conn.ReadMsgUnix(buf, oob)
	if err != nil {
		return rpcResponse{}, -1, fmt.Errorf("read %s response: %w", op, err)
	}

	var resp rpcResponse
	if err := json.Unmarshal(buf[:n], &resp); err != nil {
		return rpcResponse{}, -1, fmt.Errorf("malformed %s response: %w", op, err)
	}
	if !resp.OK {
		msg := resp.Error
		if msg == "" {
			msg = "unknown error"
		}
		return rpcResponse{}, -1, fmt.Errorf("%s: %s", op, msg)
	}

	fd := -1
	if oobn > 0 {
		fd, err = parseRightsFD(oob[:oobn])
		if err != nil {
			return rpcResponse{}, -1, fmt.Errorf("%s: %w", op, err)
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
func (c *Client) OpenTUN(name string) (tun *os.File, ifaceName string, err error) {
	resp, fd, err := c.call(opOpenTUN, openTUNArgs{Name: name})
	if err != nil {
		return nil, "", err
	}
	if fd < 0 {
		return nil, "", fmt.Errorf("open_tun: helper did not return a file descriptor")
	}

	var data openTUNData
	if err := json.Unmarshal(resp.Data, &data); err != nil {
		_ = unix.Close(fd)
		return nil, "", fmt.Errorf("open_tun: malformed response: %w", err)
	}

	return os.NewFile(uintptr(fd), "/dev/net/tun"), data.Iface, nil
}

// ConfigureTUNAddress mirrors the package-level ConfigureTUNAddress.
func (c *Client) ConfigureTUNAddress(iface, cidr string, mtu int) error {
	_, _, err := c.call(opConfigureTUN, configureTUNArgs{Iface: iface, CIDR: cidr, MTU: mtu})
	return err
}

// AddHostRoute mirrors the package-level AddHostRoute.
func (c *Client) AddHostRoute(ip, gateway, iface string) error {
	_, _, err := c.call(opAddHostRoute, addHostRouteArgs{IP: ip, Gateway: gateway, Iface: iface})
	return err
}

// DelHostRoute mirrors the package-level DelHostRoute.
func (c *Client) DelHostRoute(ip string) error {
	_, _, err := c.call(opDelHostRoute, delHostRouteArgs{IP: ip})
	return err
}

// ReplaceDefaultRoute mirrors the package-level ReplaceDefaultRoute.
func (c *Client) ReplaceDefaultRoute(ipVersion int, iface string) (*DefaultRoute, error) {
	resp, _, err := c.call(opReplaceDefaultRoute, replaceDefaultRouteArgs{IPVersion: ipVersion, Iface: iface})
	if err != nil {
		return nil, err
	}
	var data replaceDefaultRouteData
	if err := json.Unmarshal(resp.Data, &data); err != nil {
		return nil, fmt.Errorf("replace_default_route: malformed response: %w", err)
	}
	return data.Prev, nil
}

// RestoreDefaultRoute mirrors the package-level RestoreDefaultRoute.
func (c *Client) RestoreDefaultRoute(ipVersion int, prev *DefaultRoute) error {
	_, _, err := c.call(opRestoreDefaultRoute, restoreDefaultRouteArgs{IPVersion: ipVersion, Prev: prev})
	return err
}

// BlockIPv6Default mirrors the package-level BlockIPv6Default.
func (c *Client) BlockIPv6Default() (hadDefault bool, prev *DefaultRoute, err error) {
	resp, _, err := c.call(opBlockIPv6Default, nil)
	if err != nil {
		return false, nil, err
	}
	var data blockIPv6DefaultData
	if err := json.Unmarshal(resp.Data, &data); err != nil {
		return false, nil, fmt.Errorf("block_ipv6_default: malformed response: %w", err)
	}
	return data.HadDefault, data.Prev, nil
}

// RestoreIPv6Default mirrors the package-level RestoreIPv6Default.
func (c *Client) RestoreIPv6Default(hadDefault bool, prev *DefaultRoute) error {
	_, _, err := c.call(opRestoreIPv6Default, restoreIPv6DefaultArgs{HadDefault: hadDefault, Prev: prev})
	return err
}

// SetSysctl mirrors the package-level SetSysctl, setting key to value.
func (c *Client) SetSysctl(key, value string) (previous string, err error) {
	resp, _, err := c.call(opSetSysctl, setSysctlArgs{Key: key, Value: value})
	if err != nil {
		return "", err
	}
	var data setSysctlData
	if err := json.Unmarshal(resp.Data, &data); err != nil {
		return "", fmt.Errorf("set_sysctl: malformed response: %w", err)
	}
	return data.Previous, nil
}

// RestoreSysctl sets key back to value (its value before a prior SetSysctl
// call).
func (c *Client) RestoreSysctl(key, value string) error {
	_, _, err := c.call(opRestoreSysctl, setSysctlArgs{Key: key, Value: value})
	return err
}

// AddMasquerade mirrors the package-level AddMasquerade.
func (c *Client) AddMasquerade(subnet, oif string) error {
	_, _, err := c.call(opAddMasquerade, masqueradeArgs{Subnet: subnet, Oif: oif})
	return err
}

// DelMasquerade mirrors the package-level DelMasquerade.
func (c *Client) DelMasquerade(subnet, oif string) error {
	_, _, err := c.call(opDelMasquerade, masqueradeArgs{Subnet: subnet, Oif: oif})
	return err
}

// AddForwardAccept mirrors the package-level AddForwardAccept.
func (c *Client) AddForwardAccept(inIface, outIface string) error {
	_, _, err := c.call(opAddForwardAccept, forwardArgs{InIface: inIface, OutIface: outIface})
	return err
}

// DelForwardAccept mirrors the package-level DelForwardAccept.
func (c *Client) DelForwardAccept(inIface, outIface string) error {
	_, _, err := c.call(opDelForwardAccept, forwardArgs{InIface: inIface, OutIface: outIface})
	return err
}

// AddForwardEstablished mirrors the package-level AddForwardEstablished.
func (c *Client) AddForwardEstablished(inIface, outIface string) error {
	_, _, err := c.call(opAddForwardEstablished, forwardArgs{InIface: inIface, OutIface: outIface})
	return err
}

// DelForwardEstablished mirrors the package-level DelForwardEstablished.
func (c *Client) DelForwardEstablished(inIface, outIface string) error {
	_, _, err := c.call(opDelForwardEstablished, forwardArgs{InIface: inIface, OutIface: outIface})
	return err
}
