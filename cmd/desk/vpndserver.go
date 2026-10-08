package main

import (
	"encoding/json"
	"fmt"
	"net"

	log "github.com/sirupsen/logrus"
	"golang.org/x/sys/unix"

	"github.com/xconnio/deskconn/common"
)

// Server implements the privileged side of the vpnd protocol: it
// runs the actual iptun calls (expects to be root) on behalf of one
// connected client, and undoes anything left outstanding if that client
// disconnects without cleanly undoing it first (crash, kill -9, ...).
type Server struct {
	undo undoStack
}

// Serve handles requests on conn until it's closed or a read fails, then
// unwinds anything still outstanding. It only ever serves one connection;
// callers wanting a fresh session should construct a new Server.
func (s *Server) Serve(conn *net.UnixConn) {
	defer s.undo.unwindAll()

	buf := make([]byte, common.VPNHelperBufSize)
	for {
		n, _, _, _, err := conn.ReadMsgUnix(buf, nil)
		if err != nil {
			return
		}
		if n == 0 {
			continue
		}

		var req common.VPNHelperRequest
		if err := json.Unmarshal(buf[:n], &req); err != nil {
			writeResponse(conn, common.VPNHelperResponse{Error: fmt.Sprintf("malformed request: %v", err)}, -1)
			continue
		}

		s.handle(conn, req)
	}
}

func (s *Server) handle(conn *net.UnixConn, req common.VPNHelperRequest) {
	switch req.Op {
	case common.VPNOpOpenTUN:
		s.handleOpenTUN(conn, req)
	case common.VPNOpConfigureTUN:
		s.handleConfigureTUN(conn, req)
	case common.VPNOpAddHostRoute:
		s.handleAddHostRoute(conn, req)
	case common.VPNOpDelHostRoute:
		s.handleDelHostRoute(conn, req)
	case common.VPNOpReplaceDefaultRoute:
		s.handleReplaceDefaultRoute(conn, req)
	case common.VPNOpRestoreDefaultRoute:
		s.handleRestoreDefaultRoute(conn, req)
	case common.VPNOpBlockIPv6Default:
		s.handleBlockIPv6Default(conn, req)
	case common.VPNOpRestoreIPv6Default:
		s.handleRestoreIPv6Default(conn, req)
	case common.VPNOpSetLinkDNS:
		s.handleSetLinkDNS(conn, req)
	case common.VPNOpRevertLinkDNS:
		s.handleRevertLinkDNS(conn, req)
	case common.VPNOpSetSysctl:
		s.handleSetSysctl(conn, req)
	case common.VPNOpRestoreSysctl:
		s.handleRestoreSysctl(conn, req)
	case common.VPNOpAddMasquerade:
		s.handleAddMasquerade(conn, req)
	case common.VPNOpDelMasquerade:
		s.handleDelMasquerade(conn, req)
	case common.VPNOpAddForwardAccept:
		s.handleAddForwardAccept(conn, req)
	case common.VPNOpDelForwardAccept:
		s.handleDelForwardAccept(conn, req)
	case common.VPNOpAddForwardEstablished:
		s.handleAddForwardEstablished(conn, req)
	case common.VPNOpDelForwardEstablished:
		s.handleDelForwardEstablished(conn, req)
	default:
		writeResponse(conn, common.VPNHelperResponse{Error: fmt.Sprintf("unknown op %q", req.Op)}, -1)
	}
}

func (s *Server) handleOpenTUN(conn *net.UnixConn, req common.VPNHelperRequest) {
	var args common.VPNOpenTUNArgs
	if err := json.Unmarshal(req.Args, &args); err != nil {
		writeResponse(conn, errResponse(err), -1)
		return
	}

	tun, ifaceName, err := OpenTUN(args.Name)
	if err != nil {
		writeResponse(conn, errResponse(err), -1)
		return
	}
	defer func() { _ = tun.Close() }() // this process's copy; the fd we send below is a dup

	// SyscallConn, not tun.Fd(): the latter forces the fd into blocking mode, which -- since a
	// SCM_RIGHTS-duplicated fd shares status flags with the original -- would silently undo
	// the client's O_NONBLOCK too.
	rawConn, err := tun.SyscallConn()
	if err != nil {
		writeResponse(conn, errResponse(err), -1)
		return
	}
	ctrlErr := rawConn.Control(func(fd uintptr) {
		writeResponse(conn, dataResponse(common.VPNOpenTUNData{Iface: ifaceName}), int(fd))
	})
	if ctrlErr != nil {
		log.Debugf("vpnd: open_tun: failed to access raw fd: %v", ctrlErr)
	}
}

func (s *Server) handleConfigureTUN(conn *net.UnixConn, req common.VPNHelperRequest) {
	var args common.VPNConfigureTUNArgs
	if err := json.Unmarshal(req.Args, &args); err != nil {
		writeResponse(conn, errResponse(err), -1)
		return
	}

	if err := ConfigureTUNAddress(args.Iface, args.CIDR, args.MTU); err != nil {
		writeResponse(conn, errResponse(err), -1)
		return
	}
	writeResponse(conn, common.VPNHelperResponse{OK: true}, -1)
}

func (s *Server) handleAddHostRoute(conn *net.UnixConn, req common.VPNHelperRequest) {
	var args common.VPNAddHostRouteArgs
	if err := json.Unmarshal(req.Args, &args); err != nil {
		writeResponse(conn, errResponse(err), -1)
		return
	}

	if err := AddHostRoute(args.IP, args.Gateway, args.Iface); err != nil {
		writeResponse(conn, errResponse(err), -1)
		return
	}
	ip := args.IP
	s.undo.push("host_route:"+ip, func() {
		if err := DelHostRoute(ip); err != nil {
			log.Debugf("vpnd: failed to remove leftover host route to %s: %v", ip, err)
		}
	})
	writeResponse(conn, common.VPNHelperResponse{OK: true}, -1)
}

func (s *Server) handleDelHostRoute(conn *net.UnixConn, req common.VPNHelperRequest) {
	var args common.VPNDelHostRouteArgs
	if err := json.Unmarshal(req.Args, &args); err != nil {
		writeResponse(conn, errResponse(err), -1)
		return
	}

	if err := DelHostRoute(args.IP); err != nil {
		writeResponse(conn, errResponse(err), -1)
		return
	}
	s.undo.pop("host_route:" + args.IP)
	writeResponse(conn, common.VPNHelperResponse{OK: true}, -1)
}

func (s *Server) handleReplaceDefaultRoute(conn *net.UnixConn, req common.VPNHelperRequest) {
	var args common.VPNReplaceDefaultRouteArgs
	if err := json.Unmarshal(req.Args, &args); err != nil {
		writeResponse(conn, errResponse(err), -1)
		return
	}

	prev, err := ReplaceDefaultRoute(args.IPVersion, args.Iface)
	if err != nil {
		writeResponse(conn, errResponse(err), -1)
		return
	}
	ipVersion := args.IPVersion
	s.undo.push("default_route", func() {
		if err := RestoreDefaultRoute(ipVersion, prev); err != nil {
			log.Debugf("vpnd: failed to restore leftover default route: %v", err)
		}
	})
	writeResponse(conn, dataResponse(common.VPNReplaceDefaultRouteData{Prev: prev}), -1)
}

func (s *Server) handleRestoreDefaultRoute(conn *net.UnixConn, req common.VPNHelperRequest) {
	var args common.VPNRestoreDefaultRouteArgs
	if err := json.Unmarshal(req.Args, &args); err != nil {
		writeResponse(conn, errResponse(err), -1)
		return
	}

	if err := RestoreDefaultRoute(args.IPVersion, args.Prev); err != nil {
		writeResponse(conn, errResponse(err), -1)
		return
	}
	s.undo.pop("default_route")
	writeResponse(conn, common.VPNHelperResponse{OK: true}, -1)
}

func (s *Server) handleBlockIPv6Default(conn *net.UnixConn, _ common.VPNHelperRequest) {
	hadDefault, prev, err := BlockIPv6Default()
	if err != nil {
		writeResponse(conn, errResponse(err), -1)
		return
	}
	s.undo.push("ipv6_block", func() {
		if err := RestoreIPv6Default(hadDefault, prev); err != nil {
			log.Debugf("vpnd: failed to restore leftover ipv6 default route: %v", err)
		}
	})
	writeResponse(conn, dataResponse(common.VPNBlockIPv6DefaultData{HadDefault: hadDefault, Prev: prev}), -1)
}

func (s *Server) handleRestoreIPv6Default(conn *net.UnixConn, req common.VPNHelperRequest) {
	var args common.VPNRestoreIPv6DefaultArgs
	if err := json.Unmarshal(req.Args, &args); err != nil {
		writeResponse(conn, errResponse(err), -1)
		return
	}

	if err := RestoreIPv6Default(args.HadDefault, args.Prev); err != nil {
		writeResponse(conn, errResponse(err), -1)
		return
	}
	s.undo.pop("ipv6_block")
	writeResponse(conn, common.VPNHelperResponse{OK: true}, -1)
}

func (s *Server) handleSetLinkDNS(conn *net.UnixConn, req common.VPNHelperRequest) {
	var args common.VPNSetLinkDNSArgs
	if err := json.Unmarshal(req.Args, &args); err != nil {
		writeResponse(conn, errResponse(err), -1)
		return
	}

	if err := SetLinkDNS(args.Iface, args.Servers); err != nil {
		writeResponse(conn, errResponse(err), -1)
		return
	}
	iface := args.Iface
	s.undo.push("link_dns:"+iface, func() {
		if err := RevertLinkDNS(iface); err != nil {
			log.Debugf("vpnd: failed to revert leftover dns settings on %s: %v", iface, err)
		}
	})
	writeResponse(conn, common.VPNHelperResponse{OK: true}, -1)
}

func (s *Server) handleRevertLinkDNS(conn *net.UnixConn, req common.VPNHelperRequest) {
	var args common.VPNRevertLinkDNSArgs
	if err := json.Unmarshal(req.Args, &args); err != nil {
		writeResponse(conn, errResponse(err), -1)
		return
	}

	if err := RevertLinkDNS(args.Iface); err != nil {
		writeResponse(conn, errResponse(err), -1)
		return
	}
	s.undo.pop("link_dns:" + args.Iface)
	writeResponse(conn, common.VPNHelperResponse{OK: true}, -1)
}

func (s *Server) handleSetSysctl(conn *net.UnixConn, req common.VPNHelperRequest) {
	var args common.VPNSetSysctlArgs
	if err := json.Unmarshal(req.Args, &args); err != nil {
		writeResponse(conn, errResponse(err), -1)
		return
	}

	previous, err := SetSysctl(args.Key, args.Value)
	if err != nil {
		writeResponse(conn, errResponse(err), -1)
		return
	}
	key, prev := args.Key, previous
	s.undo.push("sysctl:"+key, func() {
		if _, err := SetSysctl(key, prev); err != nil {
			log.Debugf("vpnd: failed to restore leftover sysctl %s: %v", key, err)
		}
	})
	writeResponse(conn, dataResponse(common.VPNSetSysctlData{Previous: previous}), -1)
}

func (s *Server) handleRestoreSysctl(conn *net.UnixConn, req common.VPNHelperRequest) {
	var args common.VPNSetSysctlArgs
	if err := json.Unmarshal(req.Args, &args); err != nil {
		writeResponse(conn, errResponse(err), -1)
		return
	}

	if _, err := SetSysctl(args.Key, args.Value); err != nil {
		writeResponse(conn, errResponse(err), -1)
		return
	}
	s.undo.pop("sysctl:" + args.Key)
	writeResponse(conn, common.VPNHelperResponse{OK: true}, -1)
}

func (s *Server) handleAddMasquerade(conn *net.UnixConn, req common.VPNHelperRequest) {
	var args common.VPNMasqueradeArgs
	if err := json.Unmarshal(req.Args, &args); err != nil {
		writeResponse(conn, errResponse(err), -1)
		return
	}

	if err := AddMasquerade(args.Subnet, args.Oif); err != nil {
		writeResponse(conn, errResponse(err), -1)
		return
	}
	subnet, oif := args.Subnet, args.Oif
	s.undo.push("masquerade:"+subnet+"|"+oif, func() {
		if err := DelMasquerade(subnet, oif); err != nil {
			log.Debugf("vpnd: failed to remove leftover masquerade rule: %v", err)
		}
	})
	writeResponse(conn, common.VPNHelperResponse{OK: true}, -1)
}

func (s *Server) handleDelMasquerade(conn *net.UnixConn, req common.VPNHelperRequest) {
	var args common.VPNMasqueradeArgs
	if err := json.Unmarshal(req.Args, &args); err != nil {
		writeResponse(conn, errResponse(err), -1)
		return
	}

	if err := DelMasquerade(args.Subnet, args.Oif); err != nil {
		writeResponse(conn, errResponse(err), -1)
		return
	}
	s.undo.pop("masquerade:" + args.Subnet + "|" + args.Oif)
	writeResponse(conn, common.VPNHelperResponse{OK: true}, -1)
}

func (s *Server) handleAddForwardAccept(conn *net.UnixConn, req common.VPNHelperRequest) {
	var args common.VPNForwardArgs
	if err := json.Unmarshal(req.Args, &args); err != nil {
		writeResponse(conn, errResponse(err), -1)
		return
	}

	if err := AddForwardAccept(args.InIface, args.OutIface); err != nil {
		writeResponse(conn, errResponse(err), -1)
		return
	}
	in, out := args.InIface, args.OutIface
	s.undo.push("forward_accept:"+in+"|"+out, func() {
		if err := DelForwardAccept(in, out); err != nil {
			log.Debugf("vpnd: failed to remove leftover forward-accept rule: %v", err)
		}
	})
	writeResponse(conn, common.VPNHelperResponse{OK: true}, -1)
}

func (s *Server) handleDelForwardAccept(conn *net.UnixConn, req common.VPNHelperRequest) {
	var args common.VPNForwardArgs
	if err := json.Unmarshal(req.Args, &args); err != nil {
		writeResponse(conn, errResponse(err), -1)
		return
	}

	if err := DelForwardAccept(args.InIface, args.OutIface); err != nil {
		writeResponse(conn, errResponse(err), -1)
		return
	}
	s.undo.pop("forward_accept:" + args.InIface + "|" + args.OutIface)
	writeResponse(conn, common.VPNHelperResponse{OK: true}, -1)
}

func (s *Server) handleAddForwardEstablished(conn *net.UnixConn, req common.VPNHelperRequest) {
	var args common.VPNForwardArgs
	if err := json.Unmarshal(req.Args, &args); err != nil {
		writeResponse(conn, errResponse(err), -1)
		return
	}

	if err := AddForwardEstablished(args.InIface, args.OutIface); err != nil {
		writeResponse(conn, errResponse(err), -1)
		return
	}
	in, out := args.InIface, args.OutIface
	s.undo.push("forward_established:"+in+"|"+out, func() {
		if err := DelForwardEstablished(in, out); err != nil {
			log.Debugf("vpnd: failed to remove leftover forward-established rule: %v", err)
		}
	})
	writeResponse(conn, common.VPNHelperResponse{OK: true}, -1)
}

func (s *Server) handleDelForwardEstablished(conn *net.UnixConn, req common.VPNHelperRequest) {
	var args common.VPNForwardArgs
	if err := json.Unmarshal(req.Args, &args); err != nil {
		writeResponse(conn, errResponse(err), -1)
		return
	}

	if err := DelForwardEstablished(args.InIface, args.OutIface); err != nil {
		writeResponse(conn, errResponse(err), -1)
		return
	}
	s.undo.pop("forward_established:" + args.InIface + "|" + args.OutIface)
	writeResponse(conn, common.VPNHelperResponse{OK: true}, -1)
}

func errResponse(err error) common.VPNHelperResponse {
	return common.VPNHelperResponse{Error: err.Error()}
}

func dataResponse(v any) common.VPNHelperResponse {
	data, err := json.Marshal(v)
	if err != nil {
		return common.VPNHelperResponse{Error: err.Error()}
	}
	return common.VPNHelperResponse{OK: true, Data: data}
}

// writeResponse sends resp as one datagram, attaching fd as SCM_RIGHTS
// ancillary data when fd >= 0. Send errors are logged, not returned: if the
// client already went away there's nothing the caller can do about it here,
// and Serve's own next ReadMsgUnix will notice and return.
func writeResponse(conn *net.UnixConn, resp common.VPNHelperResponse, fd int) {
	if resp.Error != "" {
		resp.OK = false
	}
	data, err := json.Marshal(resp)
	if err != nil {
		log.Debugf("vpnd: failed to marshal response: %v", err)
		return
	}

	var oob []byte
	if fd >= 0 {
		oob = unix.UnixRights(fd)
	}
	if _, _, err := conn.WriteMsgUnix(data, oob, nil); err != nil {
		log.Debugf("vpnd: failed to write response: %v", err)
	}
}

// undoStack tracks outstanding privileged changes as (kind, undo) pairs so
// Serve can unwind whatever a client leaves behind on disconnect. pop
// removes the matching push so an explicit undo doesn't run twice.
type undoStack struct {
	entries []undoEntry
}

type undoEntry struct {
	kind string
	fn   func()
}

func (u *undoStack) push(kind string, fn func()) {
	u.entries = append(u.entries, undoEntry{kind: kind, fn: fn})
}

func (u *undoStack) pop(kind string) {
	for i := len(u.entries) - 1; i >= 0; i-- {
		if u.entries[i].kind == kind {
			u.entries = append(u.entries[:i], u.entries[i+1:]...)
			return
		}
	}
}

func (u *undoStack) unwindAll() {
	entries := u.entries
	u.entries = nil
	for i := len(entries) - 1; i >= 0; i-- {
		entries[i].fn()
	}
}
