package common

import (
	"encoding/json"
)

// This file defines the wire protocol between deskconn-rootd (root,
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

type VPNTunnelRouteArgs struct {
	CIDR  string `json:"cidr"`
	Iface string `json:"iface"`
}

type VPNReplaceDefaultRouteArgs struct {
	IPVersion int    `json:"ip_version"`
	Iface     string `json:"iface"`
}

type VPNRestoreDefaultRouteArgs struct {
	IPVersion int `json:"ip_version"`
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
	// Domains, if any, limits the lookups sent to Servers to these domains and their
	// subdomains, instead of every lookup.
	Domains []string `json:"domains,omitempty"`
}

type VPNSetLinkDNSData struct {
	// AllLookups is set when Domains were given but every lookup was sent to Servers anyway,
	// since per-domain DNS needs systemd-resolved to be answering this machine's lookups.
	AllLookups bool `json:"all_lookups,omitempty"`
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
