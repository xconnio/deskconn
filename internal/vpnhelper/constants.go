package vpnhelper

const (
	VPNOpOpenTUN             VPNHelperOp = "open_tun"
	VPNOpConfigureTUN        VPNHelperOp = "configure_tun"
	VPNOpAddHostRoute        VPNHelperOp = "add_host_route"
	VPNOpDelHostRoute        VPNHelperOp = "del_host_route"
	VPNOpAddTunnelRoute      VPNHelperOp = "add_tunnel_route"
	VPNOpDelTunnelRoute      VPNHelperOp = "del_tunnel_route"
	VPNOpReplaceDefaultRoute VPNHelperOp = "replace_default_route"
	VPNOpRestoreDefaultRoute VPNHelperOp = "restore_default_route"
	VPNOpBlockIPv6Default    VPNHelperOp = "block_ipv6_default"
	VPNOpRestoreIPv6Default  VPNHelperOp = "restore_ipv6_default"
	VPNOpSetLinkDNS          VPNHelperOp = "set_link_dns"
	VPNOpRevertLinkDNS       VPNHelperOp = "revert_link_dns"

	// The remaining ops are only used by the exit-node (server) role.
	VPNOpSetSysctl             VPNHelperOp = "set_sysctl"
	VPNOpRestoreSysctl         VPNHelperOp = "restore_sysctl"
	VPNOpAddMasquerade         VPNHelperOp = "add_masquerade"
	VPNOpDelMasquerade         VPNHelperOp = "del_masquerade"
	VPNOpAddForwardAccept      VPNHelperOp = "add_forward_accept"
	VPNOpDelForwardAccept      VPNHelperOp = "del_forward_accept"
	VPNOpAddForwardEstablished VPNHelperOp = "add_forward_established"
	VPNOpDelForwardEstablished VPNHelperOp = "del_forward_established"
)

const VPNHelperBufSize = 64 * 1024
