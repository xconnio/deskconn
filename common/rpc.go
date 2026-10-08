package common

import (
	"encoding/json"
)

// This file defines the wire protocol between vpnd (root,
// launched on demand via sudo -- see LaunchHelper) and whichever
// unprivileged process needs privileged networking done on its behalf.
//
// Messages go over a SOCK_SEQPACKET ("unixpacket") socket, one JSON
// request/response per datagram -- the socket type preserves record
// boundaries, so no length-prefixing is needed. open_tun's response also
// carries the TUN fd as SCM_RIGHTS ancillary data alongside its JSON.

type VPNHelperOp string

type VPNHelperRequest struct {
	Op   VPNHelperOp     `json:"op"`
	Args json.RawMessage `json:"args,omitempty"`
}

type VPNHelperResponse struct {
	OK    bool            `json:"ok"`
	Error string          `json:"error,omitempty"`
	Data  json.RawMessage `json:"data,omitempty"`
}

type VPNOpenTUNArgs struct {
	Name string `json:"name"`
}

type VPNOpenTUNData struct {
	Iface string `json:"iface"`
}

type VPNConfigureTUNArgs struct {
	Iface string `json:"iface"`
	CIDR  string `json:"cidr"`
	MTU   int    `json:"mtu"`
}

type VPNAddHostRouteArgs struct {
	IP      string `json:"ip"`
	Gateway string `json:"gateway"`
	Iface   string `json:"iface"`
}

type VPNDelHostRouteArgs struct {
	IP string `json:"ip"`
}

type VPNReplaceDefaultRouteArgs struct {
	IPVersion int    `json:"ip_version"`
	Iface     string `json:"iface"`
}

type VPNReplaceDefaultRouteData struct {
	Prev *DefaultRoute `json:"prev,omitempty"`
}

type VPNRestoreDefaultRouteArgs struct {
	IPVersion int           `json:"ip_version"`
	Prev      *DefaultRoute `json:"prev,omitempty"`
}

type VPNBlockIPv6DefaultData struct {
	HadDefault bool          `json:"had_default"`
	Prev       *DefaultRoute `json:"prev,omitempty"`
}

type VPNRestoreIPv6DefaultArgs struct {
	HadDefault bool          `json:"had_default"`
	Prev       *DefaultRoute `json:"prev,omitempty"`
}

type VPNSetSysctlArgs struct {
	Key   string `json:"key"`
	Value string `json:"value"`
}

type VPNSetSysctlData struct {
	Previous string `json:"previous"`
}

type VPNSetLinkDNSArgs struct {
	Iface   string   `json:"iface"`
	Servers []string `json:"servers"`
}

type VPNRevertLinkDNSArgs struct {
	Iface string `json:"iface"`
}

type VPNMasqueradeArgs struct {
	Subnet string `json:"subnet"`
	Oif    string `json:"oif"`
}

type VPNForwardArgs struct {
	InIface  string `json:"in_iface"`
	OutIface string `json:"out_iface"`
}
