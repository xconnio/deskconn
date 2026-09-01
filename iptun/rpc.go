package iptun

import "encoding/json"

// This file defines the wire protocol between deskconn-vpnd (root,
// launched on demand via sudo -- see LaunchHelper) and whichever
// unprivileged process needs privileged networking done on its behalf.
//
// Messages go over a SOCK_SEQPACKET ("unixpacket") socket, one JSON
// request/response per datagram -- the socket type preserves record
// boundaries, so no length-prefixing is needed. open_tun's response also
// carries the TUN fd as SCM_RIGHTS ancillary data alongside its JSON.

type rpcOp string

const (
	opOpenTUN             rpcOp = "open_tun"
	opConfigureTUN        rpcOp = "configure_tun"
	opAddHostRoute        rpcOp = "add_host_route"
	opDelHostRoute        rpcOp = "del_host_route"
	opReplaceDefaultRoute rpcOp = "replace_default_route"
	opRestoreDefaultRoute rpcOp = "restore_default_route"
	opBlockIPv6Default    rpcOp = "block_ipv6_default"
	opRestoreIPv6Default  rpcOp = "restore_ipv6_default"

	// The remaining ops are only used by the exit-node (server) role.
	opSetSysctl             rpcOp = "set_sysctl"
	opRestoreSysctl         rpcOp = "restore_sysctl"
	opAddMasquerade         rpcOp = "add_masquerade"
	opDelMasquerade         rpcOp = "del_masquerade"
	opAddForwardAccept      rpcOp = "add_forward_accept"
	opDelForwardAccept      rpcOp = "del_forward_accept"
	opAddForwardEstablished rpcOp = "add_forward_established"
	opDelForwardEstablished rpcOp = "del_forward_established"
)

type rpcRequest struct {
	Op   rpcOp           `json:"op"`
	Args json.RawMessage `json:"args,omitempty"`
}

type rpcResponse struct {
	OK    bool            `json:"ok"`
	Error string          `json:"error,omitempty"`
	Data  json.RawMessage `json:"data,omitempty"`
}

type openTUNArgs struct {
	Name string `json:"name"`
}

type openTUNData struct {
	Iface string `json:"iface"`
}

type configureTUNArgs struct {
	Iface string `json:"iface"`
	CIDR  string `json:"cidr"`
	MTU   int    `json:"mtu"`
}

type addHostRouteArgs struct {
	IP      string `json:"ip"`
	Gateway string `json:"gateway"`
	Iface   string `json:"iface"`
}

type delHostRouteArgs struct {
	IP string `json:"ip"`
}

type replaceDefaultRouteArgs struct {
	IPVersion int    `json:"ip_version"`
	Iface     string `json:"iface"`
}

type replaceDefaultRouteData struct {
	Prev *DefaultRoute `json:"prev,omitempty"`
}

type restoreDefaultRouteArgs struct {
	IPVersion int           `json:"ip_version"`
	Prev      *DefaultRoute `json:"prev,omitempty"`
}

type blockIPv6DefaultData struct {
	HadDefault bool          `json:"had_default"`
	Prev       *DefaultRoute `json:"prev,omitempty"`
}

type restoreIPv6DefaultArgs struct {
	HadDefault bool          `json:"had_default"`
	Prev       *DefaultRoute `json:"prev,omitempty"`
}

type setSysctlArgs struct {
	Key   string `json:"key"`
	Value string `json:"value"`
}

type setSysctlData struct {
	Previous string `json:"previous"`
}

type masqueradeArgs struct {
	Subnet string `json:"subnet"`
	Oif    string `json:"oif"`
}

type forwardArgs struct {
	InIface  string `json:"in_iface"`
	OutIface string `json:"out_iface"`
}
