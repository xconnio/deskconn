package top

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"sync"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/xconnio/deskconn/internal/common"
	"github.com/xconnio/deskconn/internal/deskconn"
	"github.com/xconnio/xconn-go"
)

// topTarget is a machine desk top acts on: one of the account's, or a profile's.
type topTarget struct {
	name    string
	realm   string
	profile string // the profile's name, or "" for a machine of the account
}

func machineTarget(machine common.Machine) topTarget {
	return topTarget{name: machine.Name, realm: machine.Realm}
}

func profileTarget(profile common.Profile) topTarget {
	return topTarget{name: profile.Name, realm: common.ProfileRealm(profile.Name), profile: profile.Name}
}

// streamMode is the --mode a shell, logs or port forward to target uses: for a machine of
// the account, the daemon's connection to it when the daemon is running (P2P once it has
// upgraded it, which it starts on connecting), otherwise a connection of its own through the
// router (which, unlike trying a direct one first, doesn't print on the way).
func (m *Model) streamMode(target topTarget) string {
	if target.profile == "" && m.daemonUp {
		return ""
	}
	return deskconn.ModeRouted
}

// withTarget runs action on target, asking for the profile's password first if it's needed
// and not known yet.
func (m *Model) withTarget(target topTarget, action func() tea.Cmd) tea.Cmd {
	standalone := m.targets[target.profile]
	if standalone == nil || !standalone.NeedsPassword() {
		return action()
	}
	m.dialog = &topPasswordDialog{
		authID: standalone.AuthID,
		name:   target.name,
		entered: func(password string) tea.Cmd {
			standalone.SetPassword(password)
			return action()
		},
	}
	return nil
}

// failed reports err from doing what to target, forgetting the profile's password if it may
// have been the wrong one, so it's asked for again next time.
func (m *Model) failed(target topTarget, what string, err error) tea.Cmd {
	if standalone := m.targets[target.profile]; standalone != nil && standalone.PrivateKey == "" &&
		strings.Contains(strings.ToLower(err.Error()), "auth") {
		standalone.SetPassword("")
	}
	return m.flashErrorf("%s: %v", what, err)
}

// fetchConnected asks the local daemon which machines it holds connections to. Without a
// daemon, the list just shows no connections.
func (m *Model) fetchConnected() tea.Cmd {
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		session, err := xconn.ConnectAnonymous(ctx, common.DaemonURI(m.cfgDirectory), common.LocalRealm)
		if err != nil {
			return topConnectedMsg{}
		}
		defer func() { _ = session.Leave() }()
		resp := session.Call(common.ProcedureConnectedMachines).DoContext(ctx)
		if resp.Err != nil || len(resp.Args()) == 0 {
			return topConnectedMsg{daemonUp: true}
		}
		connected, _ := resp.Args()[0].(map[string]any)
		return topConnectedMsg{connected: connected, daemonUp: true}
	}
}

// ping times a call to target's machine.
func (m *Model) ping(target topTarget) tea.Cmd {
	return m.withTarget(target, func() tea.Cmd {
		return tea.Batch(m.setFlash("Pinging "+target.name+"…", false), func() tea.Msg {
			start := time.Now()
			err := m.remote.Ping(target.realm)
			return topPingMsg{target: target, rtt: time.Since(start), err: err}
		})
	})
}

// shell opens a shell on target in the terminal, which desk top hands over until it ends.
func (m *Model) shell(target topTarget) tea.Cmd {
	return m.withTarget(target, func() tea.Cmd {
		shell := &topShell{target: target, mode: m.streamMode(target), cfgDirectory: m.cfgDirectory}
		return tea.Exec(shell, func(err error) tea.Msg { return topShellDoneMsg{target: target, err: err} })
	})
}

// topShell runs a shell on a machine, as desk shell does, in the terminal tea.Exec hands it.
type topShell struct {
	target       topTarget
	mode         string
	cfgDirectory string
	stdout       io.Writer
}

func (s *topShell) SetStdin(io.Reader)    {}
func (s *topShell) SetStdout(w io.Writer) { s.stdout = w }
func (s *topShell) SetStderr(io.Writer)   {}

func (s *topShell) Run() error {
	// A clear screen, the shell's own output starting at the top.
	_, _ = fmt.Fprintf(s.stdout, "\x1b[2J\x1b[HConnecting to %s…\r\n", s.target.name)
	return deskconn.RunShell(context.Background(), s.mode, s.target.realm, s.cfgDirectory)
}

// Remote is what desk top does on machines, each named by its realm.
type Remote interface {
	// Call calls one of the machine's encrypted procedures with args, as JSON, decoding what
	// it returns into result.
	Call(realm, procedure string, args, result any) error
	Ping(realm string) error
	// Logs streams the machine's logs of source (a service or file, or "" for the system
	// journal), following them, to out until ctx is done.
	Logs(ctx context.Context, mode, realm, source string, out io.Writer) error
	// Forward forwards connections to port listen to port to until ctx is done: from this
	// machine to the remote one, or, if reverse, from the remote one to this one.
	Forward(ctx context.Context, mode, realm string, reverse bool, listen, to string) error
	// Close closes the connections that Call and Ping keep.
	Close()
}

// topSessions are the real Remote. Calls to a machine of the account go through the local
// daemon when it's running, on its connection to the machine: P2P once it has upgraded it,
// and with the keys already exchanged. Otherwise, and for profiles, they go on connections
// of desk top's own, one per machine, kept for as long as they work.
type topSessions struct {
	cfgDirectory string

	mu     sync.Mutex
	daemon *xconn.Session  // to the local daemon, or nil if not connected yet
	ready  map[string]bool // the realms whose machines the daemon has been asked to connect to
	// proxying lets one call at a time through the daemon to each realm's machine: on a
	// connection it hasn't used yet, calls at once each exchange keys, and all but the
	// last are refused for theirs.
	proxying map[string]*sync.Mutex
	byRealm  map[string]*topSession
	dialing  map[string]chan struct{} // by realm, closed once the connection being made is made or fails
}

type topSession struct {
	conn *common.MachineConn
	keys *common.SessionKeys
}

func newTopSessions(cfgDirectory string) *topSessions {
	return &topSessions{cfgDirectory: cfgDirectory, ready: map[string]bool{}, proxying: map[string]*sync.Mutex{},
		byRealm: map[string]*topSession{}, dialing: map[string]chan struct{}{}}
}

const (
	// topConnectTimeout bounds connecting to a machine, and a call through the daemon,
	// which may have to connect to the machine first.
	topConnectTimeout = 15 * time.Second
	// topDaemonTimeout bounds connecting to the local daemon.
	topDaemonTimeout = 2 * time.Second
)

// viaDaemon returns the local daemon's session if calls to realm's machine go through it:
// for a machine of the account, while the daemon is running.
func (s *topSessions) viaDaemon(realm string) *xconn.Session {
	if _, standalone := common.StandaloneTargetFor(realm); standalone {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.daemon != nil && s.daemon.Connected() {
		return s.daemon
	}
	ctx, cancel := context.WithTimeout(context.Background(), topDaemonTimeout)
	defer cancel()
	daemon, err := xconn.ConnectAnonymous(ctx, common.DaemonURI(s.cfgDirectory), common.LocalRealm)
	if err != nil {
		s.daemon = nil
		return nil
	}
	s.daemon = daemon
	return daemon
}

// proxy calls procedure on realm's machine through the daemon, having it connect to the
// machine first if it hasn't yet: calls at once would each have it connect, the last
// connection replacing the others under the calls on them.
func (s *topSessions) proxy(daemon *xconn.Session, realm, procedure string, args ...any) (xconn.CallResponse, error) {
	s.mu.Lock()
	proxying := s.proxying[realm]
	if proxying == nil {
		proxying = &sync.Mutex{}
		s.proxying[realm] = proxying
	}
	ready := s.ready[realm]
	s.mu.Unlock()

	proxying.Lock()
	defer proxying.Unlock()
	ctx, cancel := context.WithTimeout(context.Background(), topConnectTimeout)
	defer cancel()
	if !ready {
		if err := daemon.Call(common.ProcedureConnect).Args(realm).DoContext(ctx).Err; err != nil {
			return xconn.CallResponse{}, err
		}
	}
	resp := daemon.Call(procedure).Args(args...).DoContext(ctx)

	s.mu.Lock()
	defer s.mu.Unlock()
	// After a failure, have it connect again first, in case it lost the connection.
	s.ready[realm] = resp.Err == nil
	return resp, resp.Err
}

// sessionFor returns realm's session, connecting first if there isn't one. Calls at once
// share one connection.
func (s *topSessions) sessionFor(realm string) (*topSession, error) {
	s.mu.Lock()
	for {
		if session := s.byRealm[realm]; session != nil {
			s.mu.Unlock()
			return session, nil
		}
		dialing, ok := s.dialing[realm]
		if !ok {
			break
		}
		s.mu.Unlock()
		<-dialing
		s.mu.Lock()
	}
	dialing := make(chan struct{})
	s.dialing[realm] = dialing
	s.mu.Unlock()

	session, err := s.dial(realm)

	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.dialing, realm)
	close(dialing)
	if err != nil {
		return nil, err
	}
	s.byRealm[realm] = session
	return session, nil
}

func (s *topSessions) dial(realm string) (*topSession, error) {
	ctx, cancel := context.WithTimeout(context.Background(), topConnectTimeout)
	defer cancel()
	conn, err := common.ConnectMachineRealmQUIC(ctx, realm, s.cfgDirectory)
	if err != nil {
		return nil, err
	}
	keys, err := common.ClientKeyExchange(conn.Session)
	if err != nil {
		closeMachineConn(conn)
		return nil, err
	}
	return &topSession{conn: conn, keys: keys}, nil
}

// call runs fn on realm's session, connecting first if needed. A failed call drops the
// session, so the next one starts on a fresh connection.
func (s *topSessions) call(realm string, fn func(*topSession) error) error {
	session, err := s.sessionFor(realm)
	if err != nil {
		return err
	}
	if err := fn(session); err != nil {
		s.drop(realm)
		return err
	}
	return nil
}

func (s *topSessions) Call(realm, procedure string, args, result any) error {
	payload := []byte{}
	if args != nil {
		var err error
		if payload, err = json.Marshal(args); err != nil {
			return err
		}
	}
	if daemon := s.viaDaemon(realm); daemon != nil {
		resp, err := s.proxy(daemon, realm, common.ProcedureProxyFileOp, realm, procedure, payload)
		if err != nil {
			return err
		}
		data, err := resp.ArgBytes(0)
		if err != nil {
			return err
		}
		return json.Unmarshal(data, result)
	}
	return s.call(realm, func(session *topSession) error {
		data, err := common.EncryptedCall(session.conn.Session, procedure, payload, session.keys)
		if err != nil {
			return err
		}
		return json.Unmarshal(data, result)
	})
}

func (s *topSessions) drop(realm string) {
	s.mu.Lock()
	session := s.byRealm[realm]
	delete(s.byRealm, realm)
	s.mu.Unlock()
	if session != nil {
		closeMachineConn(session.conn)
	}
}

func (s *topSessions) Ping(realm string) error {
	if daemon := s.viaDaemon(realm); daemon != nil {
		_, err := s.proxy(daemon, realm, common.ProcedureProxyPing, realm)
		return err
	}
	return s.call(realm, func(session *topSession) error {
		ctx, cancel := context.WithTimeout(context.Background(), topConnectTimeout)
		defer cancel()
		return session.conn.Call(common.ProcedurePing).DoContext(ctx).Err
	})
}

func (s *topSessions) Logs(ctx context.Context, mode, realm, source string, out io.Writer) error {
	return deskconn.RunLogs(ctx, mode, realm, s.cfgDirectory, source, true, topLogTail, "", out)
}

func (s *topSessions) Forward(ctx context.Context, mode, realm string, reverse bool, listen, to string) error {
	if reverse {
		return deskconn.RunPortReverse(ctx, mode, realm, s.cfgDirectory, listen, to)
	}
	return deskconn.RunPortForward(ctx, mode, realm, s.cfgDirectory, to, listen)
}

func (s *topSessions) Close() {
	s.mu.Lock()
	daemon, sessions := s.daemon, s.byRealm
	s.daemon, s.byRealm = nil, map[string]*topSession{}
	s.mu.Unlock()
	if daemon != nil {
		_ = daemon.Leave()
	}
	for _, session := range sessions {
		closeMachineConn(session.conn)
	}
}

// closeMachineConn leaves conn's session and closes the connection under it.
func closeMachineConn(conn *common.MachineConn) {
	_ = conn.Close()
	_ = conn.Connection().Close()
}
