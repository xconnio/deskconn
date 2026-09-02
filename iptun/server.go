package iptun

import (
	"encoding/json"
	"fmt"
	"net"

	log "github.com/sirupsen/logrus"
	"golang.org/x/sys/unix"
)

const rpcBufSize = 64 * 1024

// Server implements the privileged side of the deskconn-vpnd protocol: it
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

	buf := make([]byte, rpcBufSize)
	for {
		n, _, _, _, err := conn.ReadMsgUnix(buf, nil)
		if err != nil {
			return
		}
		if n == 0 {
			continue
		}

		var req rpcRequest
		if err := json.Unmarshal(buf[:n], &req); err != nil {
			writeResponse(conn, rpcResponse{Error: fmt.Sprintf("malformed request: %v", err)}, -1)
			continue
		}

		s.handle(conn, req)
	}
}

func (s *Server) handle(conn *net.UnixConn, req rpcRequest) {
	switch req.Op {
	case opOpenTUN:
		s.handleOpenTUN(conn, req)
	case opConfigureTUN:
		s.handleConfigureTUN(conn, req)
	case opAddHostRoute:
		s.handleAddHostRoute(conn, req)
	case opDelHostRoute:
		s.handleDelHostRoute(conn, req)
	case opReplaceDefaultRoute:
		s.handleReplaceDefaultRoute(conn, req)
	case opRestoreDefaultRoute:
		s.handleRestoreDefaultRoute(conn, req)
	case opBlockIPv6Default:
		s.handleBlockIPv6Default(conn, req)
	case opRestoreIPv6Default:
		s.handleRestoreIPv6Default(conn, req)
	case opSetSysctl:
		s.handleSetSysctl(conn, req)
	case opRestoreSysctl:
		s.handleRestoreSysctl(conn, req)
	case opAddMasquerade:
		s.handleAddMasquerade(conn, req)
	case opDelMasquerade:
		s.handleDelMasquerade(conn, req)
	case opAddForwardAccept:
		s.handleAddForwardAccept(conn, req)
	case opDelForwardAccept:
		s.handleDelForwardAccept(conn, req)
	case opAddForwardEstablished:
		s.handleAddForwardEstablished(conn, req)
	case opDelForwardEstablished:
		s.handleDelForwardEstablished(conn, req)
	default:
		writeResponse(conn, rpcResponse{Error: fmt.Sprintf("unknown op %q", req.Op)}, -1)
	}
}

func (s *Server) handleOpenTUN(conn *net.UnixConn, req rpcRequest) {
	var args openTUNArgs
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
		writeResponse(conn, dataResponse(openTUNData{Iface: ifaceName}), int(fd))
	})
	if ctrlErr != nil {
		log.Debugf("deskconn-vpnd: open_tun: failed to access raw fd: %v", ctrlErr)
	}
}

func (s *Server) handleConfigureTUN(conn *net.UnixConn, req rpcRequest) {
	var args configureTUNArgs
	if err := json.Unmarshal(req.Args, &args); err != nil {
		writeResponse(conn, errResponse(err), -1)
		return
	}

	if err := ConfigureTUNAddress(args.Iface, args.CIDR, args.MTU); err != nil {
		writeResponse(conn, errResponse(err), -1)
		return
	}
	writeResponse(conn, rpcResponse{OK: true}, -1)
}

func (s *Server) handleAddHostRoute(conn *net.UnixConn, req rpcRequest) {
	var args addHostRouteArgs
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
			log.Debugf("deskconn-vpnd: failed to remove leftover host route to %s: %v", ip, err)
		}
	})
	writeResponse(conn, rpcResponse{OK: true}, -1)
}

func (s *Server) handleDelHostRoute(conn *net.UnixConn, req rpcRequest) {
	var args delHostRouteArgs
	if err := json.Unmarshal(req.Args, &args); err != nil {
		writeResponse(conn, errResponse(err), -1)
		return
	}

	if err := DelHostRoute(args.IP); err != nil {
		writeResponse(conn, errResponse(err), -1)
		return
	}
	s.undo.pop("host_route:" + args.IP)
	writeResponse(conn, rpcResponse{OK: true}, -1)
}

func (s *Server) handleReplaceDefaultRoute(conn *net.UnixConn, req rpcRequest) {
	var args replaceDefaultRouteArgs
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
			log.Debugf("deskconn-vpnd: failed to restore leftover default route: %v", err)
		}
	})
	writeResponse(conn, dataResponse(replaceDefaultRouteData{Prev: prev}), -1)
}

func (s *Server) handleRestoreDefaultRoute(conn *net.UnixConn, req rpcRequest) {
	var args restoreDefaultRouteArgs
	if err := json.Unmarshal(req.Args, &args); err != nil {
		writeResponse(conn, errResponse(err), -1)
		return
	}

	if err := RestoreDefaultRoute(args.IPVersion, args.Prev); err != nil {
		writeResponse(conn, errResponse(err), -1)
		return
	}
	s.undo.pop("default_route")
	writeResponse(conn, rpcResponse{OK: true}, -1)
}

func (s *Server) handleBlockIPv6Default(conn *net.UnixConn, _ rpcRequest) {
	hadDefault, prev, err := BlockIPv6Default()
	if err != nil {
		writeResponse(conn, errResponse(err), -1)
		return
	}
	s.undo.push("ipv6_block", func() {
		if err := RestoreIPv6Default(hadDefault, prev); err != nil {
			log.Debugf("deskconn-vpnd: failed to restore leftover ipv6 default route: %v", err)
		}
	})
	writeResponse(conn, dataResponse(blockIPv6DefaultData{HadDefault: hadDefault, Prev: prev}), -1)
}

func (s *Server) handleRestoreIPv6Default(conn *net.UnixConn, req rpcRequest) {
	var args restoreIPv6DefaultArgs
	if err := json.Unmarshal(req.Args, &args); err != nil {
		writeResponse(conn, errResponse(err), -1)
		return
	}

	if err := RestoreIPv6Default(args.HadDefault, args.Prev); err != nil {
		writeResponse(conn, errResponse(err), -1)
		return
	}
	s.undo.pop("ipv6_block")
	writeResponse(conn, rpcResponse{OK: true}, -1)
}

func (s *Server) handleSetSysctl(conn *net.UnixConn, req rpcRequest) {
	var args setSysctlArgs
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
			log.Debugf("deskconn-vpnd: failed to restore leftover sysctl %s: %v", key, err)
		}
	})
	writeResponse(conn, dataResponse(setSysctlData{Previous: previous}), -1)
}

func (s *Server) handleRestoreSysctl(conn *net.UnixConn, req rpcRequest) {
	var args setSysctlArgs
	if err := json.Unmarshal(req.Args, &args); err != nil {
		writeResponse(conn, errResponse(err), -1)
		return
	}

	if _, err := SetSysctl(args.Key, args.Value); err != nil {
		writeResponse(conn, errResponse(err), -1)
		return
	}
	s.undo.pop("sysctl:" + args.Key)
	writeResponse(conn, rpcResponse{OK: true}, -1)
}

func (s *Server) handleAddMasquerade(conn *net.UnixConn, req rpcRequest) {
	var args masqueradeArgs
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
			log.Debugf("deskconn-vpnd: failed to remove leftover masquerade rule: %v", err)
		}
	})
	writeResponse(conn, rpcResponse{OK: true}, -1)
}

func (s *Server) handleDelMasquerade(conn *net.UnixConn, req rpcRequest) {
	var args masqueradeArgs
	if err := json.Unmarshal(req.Args, &args); err != nil {
		writeResponse(conn, errResponse(err), -1)
		return
	}

	if err := DelMasquerade(args.Subnet, args.Oif); err != nil {
		writeResponse(conn, errResponse(err), -1)
		return
	}
	s.undo.pop("masquerade:" + args.Subnet + "|" + args.Oif)
	writeResponse(conn, rpcResponse{OK: true}, -1)
}

func (s *Server) handleAddForwardAccept(conn *net.UnixConn, req rpcRequest) {
	var args forwardArgs
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
			log.Debugf("deskconn-vpnd: failed to remove leftover forward-accept rule: %v", err)
		}
	})
	writeResponse(conn, rpcResponse{OK: true}, -1)
}

func (s *Server) handleDelForwardAccept(conn *net.UnixConn, req rpcRequest) {
	var args forwardArgs
	if err := json.Unmarshal(req.Args, &args); err != nil {
		writeResponse(conn, errResponse(err), -1)
		return
	}

	if err := DelForwardAccept(args.InIface, args.OutIface); err != nil {
		writeResponse(conn, errResponse(err), -1)
		return
	}
	s.undo.pop("forward_accept:" + args.InIface + "|" + args.OutIface)
	writeResponse(conn, rpcResponse{OK: true}, -1)
}

func (s *Server) handleAddForwardEstablished(conn *net.UnixConn, req rpcRequest) {
	var args forwardArgs
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
			log.Debugf("deskconn-vpnd: failed to remove leftover forward-established rule: %v", err)
		}
	})
	writeResponse(conn, rpcResponse{OK: true}, -1)
}

func (s *Server) handleDelForwardEstablished(conn *net.UnixConn, req rpcRequest) {
	var args forwardArgs
	if err := json.Unmarshal(req.Args, &args); err != nil {
		writeResponse(conn, errResponse(err), -1)
		return
	}

	if err := DelForwardEstablished(args.InIface, args.OutIface); err != nil {
		writeResponse(conn, errResponse(err), -1)
		return
	}
	s.undo.pop("forward_established:" + args.InIface + "|" + args.OutIface)
	writeResponse(conn, rpcResponse{OK: true}, -1)
}

func errResponse(err error) rpcResponse {
	return rpcResponse{Error: err.Error()}
}

func dataResponse(v any) rpcResponse {
	data, err := json.Marshal(v)
	if err != nil {
		return rpcResponse{Error: err.Error()}
	}
	return rpcResponse{OK: true, Data: data}
}

// writeResponse sends resp as one datagram, attaching fd as SCM_RIGHTS
// ancillary data when fd >= 0. Send errors are logged, not returned: if the
// client already went away there's nothing the caller can do about it here,
// and Serve's own next ReadMsgUnix will notice and return.
func writeResponse(conn *net.UnixConn, resp rpcResponse, fd int) {
	if resp.Error != "" {
		resp.OK = false
	}
	data, err := json.Marshal(resp)
	if err != nil {
		log.Debugf("deskconn-vpnd: failed to marshal response: %v", err)
		return
	}

	var oob []byte
	if fd >= 0 {
		oob = unix.UnixRights(fd)
	}
	if _, _, err := conn.WriteMsgUnix(data, oob, nil); err != nil {
		log.Debugf("deskconn-vpnd: failed to write response: %v", err)
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
