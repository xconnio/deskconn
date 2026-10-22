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

// streamMode is the --mode a shell, logs or port forward to target uses: the daemon's
// connection when it has one to the machine, otherwise a connection of its own through the
// router (which, unlike trying a direct one first, doesn't print on the way).
func (m *Model) streamMode(target topTarget) string {
	if target.profile == "" {
		if _, ok := m.connected[target.realm]; ok {
			return ""
		}
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

// topSessions are the real Remote: the connections desk top calls machines' procedures on
// (resources, processes, ping), one per machine, kept for as long as they work.
type topSessions struct {
	cfgDirectory string

	mu      sync.Mutex
	byRealm map[string]*topSession
}

type topSession struct {
	conn *common.MachineConn
	keys *common.SessionKeys
}

func newTopSessions(cfgDirectory string) *topSessions {
	return &topSessions{cfgDirectory: cfgDirectory, byRealm: map[string]*topSession{}}
}

// topConnectTimeout bounds connecting to a machine.
const topConnectTimeout = 15 * time.Second

func (s *topSessions) get(realm string) (*topSession, error) {
	s.mu.Lock()
	session := s.byRealm[realm]
	s.mu.Unlock()
	if session != nil {
		return session, nil
	}

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
	session = &topSession{conn: conn, keys: keys}

	s.mu.Lock()
	defer s.mu.Unlock()
	if existing := s.byRealm[realm]; existing != nil {
		closeMachineConn(conn)
		return existing, nil
	}
	s.byRealm[realm] = session
	return session, nil
}

// call runs fn on realm's session, connecting first if needed. A failed call drops the
// session, so the next one starts on a fresh connection.
func (s *topSessions) call(realm string, fn func(*topSession) error) error {
	session, err := s.get(realm)
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
	var payload []byte
	if args != nil {
		var err error
		if payload, err = json.Marshal(args); err != nil {
			return err
		}
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
	sessions := s.byRealm
	s.byRealm = map[string]*topSession{}
	s.mu.Unlock()
	for _, session := range sessions {
		closeMachineConn(session.conn)
	}
}

// closeMachineConn leaves conn's session and closes the connection under it.
func closeMachineConn(conn *common.MachineConn) {
	_ = conn.Close()
	_ = conn.Connection().Close()
}
