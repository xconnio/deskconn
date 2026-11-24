package terminal

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"sort"
	"sync"
	"syscall"
	"time"

	log "github.com/sirupsen/logrus"
)

var (
	// ErrNotFound is a session that doesn't exist, has ended, or isn't the caller's: knowing a
	// session's ID tells nothing about someone else's.
	ErrNotFound = errors.New("no such terminal session; it may have ended")
	// ErrAttached is a session attached elsewhere, without a takeover asked for.
	ErrAttached = errors.New("the terminal session is attached elsewhere; take it over to attach here")
	// ErrUnavailable is a session whose worker can't be reached.
	ErrUnavailable = errors.New("the terminal session is unavailable")
	// ErrLimit is a new session refused, as the sessions running have the machine's history
	// budget.
	ErrLimit = errors.New("too many terminal sessions: the machine's limit on their output history is reached; " +
		"end one to start another")
)

const (
	// DefaultHistoryLimit is the output history a session keeps by default.
	DefaultHistoryLimit = 16 << 20
	// DefaultHistoryTotal is the history all of a daemon's sessions may keep by default.
	DefaultHistoryTotal = 256 << 20
	// minHistoryLimit is the least history a session starts with once the total runs low.
	minHistoryLimit = 1 << 20
)

const (
	// spawnTimeout bounds a new worker's start.
	spawnTimeout = 15 * time.Second
	// dialTimeout bounds connecting to a worker and its reply.
	dialTimeout = 10 * time.Second
	// goneCheckDelay is how long to give a worker that dropped a request to finish exiting.
	goneCheckDelay = 200 * time.Millisecond
)

// Manager is the daemon's side of the sessions in its terminals directory. It keeps no state
// of its own: the workers do, so a new Manager in a restarted daemon finds every session the
// old one had.
type Manager struct {
	dir string
	// workerCommand is how to run a worker: the executable and its arguments.
	workerCommand func() (string, []string, error)
	// launch starts a worker, for it to outlive this process (see launch_*.go).
	launch   func(exe string, args []string, id string) (*startedWorker, error)
	launcher launcher

	// createMu serializes creating sessions, for each to take its share of historyTotal.
	createMu     sync.Mutex
	historyLimit int
	historyTotal int
}

// NewManager manages the sessions in dir, starting each worker as this executable's
// workerArgs command.
func NewManager(dir string, workerArgs ...string) *Manager {
	m := &Manager{
		dir: dir,
		workerCommand: func() (string, []string, error) {
			exe, err := os.Executable()
			return exe, workerArgs, err
		},
		historyLimit: DefaultHistoryLimit,
		historyTotal: DefaultHistoryTotal,
	}
	m.launch = m.launcher.launch
	return m
}

// SetHistoryLimits sets the output history each new session keeps, and that all sessions
// may keep: a new session gets less once the total runs low, and none starts once it's
// spent. A limit of 0 keeps no history.
func (m *Manager) SetHistoryLimits(perSession, total int) {
	m.createMu.Lock()
	defer m.createMu.Unlock()
	m.historyLimit, m.historyTotal = max(perSession, 0), max(total, 0)
}

// CreateOptions is a new session's shell.
type CreateOptions struct {
	Owner   string
	Command string
	Args    []string
	Env     []string
	WorkDir string
	Cols    uint16
	Rows    uint16
}

// Create starts a session in a worker of its own and returns its ID. The session's first
// output waits a while for its first attachment.
func (m *Manager) Create(ctx context.Context, opts CreateOptions) (string, error) {
	if opts.Owner == "" {
		return "", errors.New("a terminal session needs an owner")
	}
	if err := ensureDir(m.dir); err != nil {
		return "", fmt.Errorf("terminals directory: %w", err)
	}
	id, err := newSessionID()
	if err != nil {
		return "", err
	}
	secret, err := randomHex(32)
	if err != nil {
		return "", err
	}
	m.createMu.Lock()
	defer m.createMu.Unlock()
	historyLimit, err := m.newHistoryLimit(ctx)
	if err != nil {
		return "", err
	}
	spec := Spec{Dir: m.dir, ID: id, Owner: opts.Owner, Secret: secret, Command: opts.Command, Args: opts.Args,
		Env: opts.Env, WorkDir: opts.WorkDir, Cols: opts.Cols, Rows: opts.Rows, HistoryLimit: historyLimit}
	if err := m.spawn(ctx, spec); err != nil {
		return "", err
	}
	return id, nil
}

// newHistoryLimit is the history a new session may keep: the per-session limit, or what's
// left of the total, which the running sessions' limits count against. createMu is held.
func (m *Manager) newHistoryLimit(ctx context.Context) (int, error) {
	if m.historyLimit == 0 {
		return 0, nil
	}
	ids, err := recordIDs(m.dir)
	if err != nil {
		return 0, err
	}
	left := m.historyTotal
	for _, id := range ids {
		rec, err := readRecord(m.dir, id)
		if err != nil {
			continue
		}
		if _, err := m.info(ctx, rec); err == nil || !errors.Is(err, ErrNotFound) {
			left -= rec.HistoryLimit
		}
	}
	limit := min(m.historyLimit, left)
	if limit < min(m.historyLimit, minHistoryLimit) {
		return 0, ErrLimit
	}
	return limit, nil
}

func (m *Manager) spawn(ctx context.Context, spec Spec) error {
	exe, args, err := m.workerCommand()
	if err != nil {
		return fmt.Errorf("terminal worker: %w", err)
	}
	started, err := m.launch(exe, args, spec.ID)
	if err != nil {
		return fmt.Errorf("start terminal worker: %w", err)
	}
	cmd, stdin, stdout := started.cmd, started.stdin, started.stdout
	// Reap it while this process lives; after, whatever inherits it does.
	go func() { _ = cmd.Wait() }()

	ready := make(chan error, 1)
	go func() {
		err := writeSpec(stdin, spec)
		if err == nil {
			err = readWorkerReady(stdout)
		}
		ready <- err
	}()
	ctx, cancel := context.WithTimeout(ctx, spawnTimeout)
	defer cancel()
	select {
	case err = <-ready:
	case <-ctx.Done():
		err = fmt.Errorf("terminal worker didn't start: %w", ctx.Err())
	}
	if err != nil {
		_ = cmd.Process.Kill()
		removeSession(spec.Dir, spec.ID)
		var startErr *workerStartError
		if errors.As(err, &startErr) {
			return startErr
		}
		return fmt.Errorf("terminal worker: %w", err)
	}
	return nil
}

// startedWorker is a worker process that has been started, with its standard input and
// output.
type startedWorker struct {
	cmd    *exec.Cmd
	stdin  io.WriteCloser
	stdout io.ReadCloser
}

// startWorkerCommand starts cmd with pipes to its standard input and output. Its standard
// error is discarded: once its starter is gone, nothing would read it.
func startWorkerCommand(cmd *exec.Cmd) (*startedWorker, error) {
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	return &startedWorker{cmd: cmd, stdin: stdin, stdout: stdout}, nil
}

func writeSpec(stdin io.WriteCloser, spec Spec) error {
	defer stdin.Close()
	return jsonEncode(stdin, spec)
}

// List returns owner's running sessions, oldest first. Records of workers that are gone are
// removed on the way.
func (m *Manager) List(ctx context.Context, owner string) ([]SessionInfo, error) {
	ids, err := recordIDs(m.dir)
	if err != nil {
		return nil, err
	}
	sessions := make([]SessionInfo, 0, len(ids))
	for _, id := range ids {
		rec, err := m.ownedRecord(id, owner)
		if err != nil {
			continue
		}
		info, err := m.info(ctx, rec)
		if err != nil {
			continue
		}
		sessions = append(sessions, *info)
	}
	sort.Slice(sessions, func(i, j int) bool { return sessions[i].Created.Before(sessions[j].Created) })
	return sessions, nil
}

// Reconcile removes the records of workers that are gone, such as after a reboot. It's run
// as the daemon starts.
func (m *Manager) Reconcile(ctx context.Context) {
	ids, err := recordIDs(m.dir)
	if err != nil {
		return
	}
	for _, id := range ids {
		rec, err := readRecord(m.dir, id)
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				continue
			}
			log.Warnf("terminal: removing unreadable session record %s: %v", id, err)
			removeSession(m.dir, id)
			continue
		}
		if _, err := m.info(ctx, rec); err == nil {
			log.Infof("terminal: found running session %s", id)
		}
	}
}

// ownedRecord is session id's record, if it's owner's.
func (m *Manager) ownedRecord(id, owner string) (record, error) {
	if !validID(id) {
		return record{}, ErrNotFound
	}
	rec, err := readRecord(m.dir, id)
	if errors.Is(err, os.ErrNotExist) {
		return record{}, ErrNotFound
	}
	if err != nil {
		return record{}, fmt.Errorf("%w: %w", ErrUnavailable, err)
	}
	if rec.Owner != owner {
		return record{}, ErrNotFound
	}
	return rec, nil
}

// info asks rec's worker about its session.
func (m *Manager) info(ctx context.Context, rec record) (*SessionInfo, error) {
	conn, reply, err := m.request(ctx, rec, message{Op: opInfo})
	if err != nil {
		return nil, err
	}
	_ = conn.Close()
	if reply.Info == nil {
		return nil, ErrUnavailable
	}
	return reply.Info, nil
}

// Terminate ends owner's session id and its processes.
func (m *Manager) Terminate(ctx context.Context, owner, id string) error {
	rec, err := m.ownedRecord(id, owner)
	if err != nil {
		return err
	}
	conn, _, err := m.request(ctx, rec, message{Op: opTerminate})
	if err != nil {
		return err
	}
	_ = conn.Close()
	return nil
}

// AttachOptions is how to attach to a session.
type AttachOptions struct {
	Cols, Rows uint16
	// Takeover replaces an existing attachment.
	Takeover bool
	// Token is an earlier attachment's, whose client this is reconnecting: it replaces that
	// attachment without a takeover.
	Token string
	// Since is the output offset the client has seen up to, for the history restored to
	// leave out what it has; nil restores all of it.
	Since *uint64
}

// Attach makes the caller owner's session id's attachment.
func (m *Manager) Attach(ctx context.Context, owner, id string, opts AttachOptions) (*Attachment, error) {
	rec, err := m.ownedRecord(id, owner)
	if err != nil {
		return nil, err
	}
	conn, reply, err := m.request(ctx, rec, message{Op: opAttach, Cols: opts.Cols, Rows: opts.Rows,
		Takeover: opts.Takeover, Token: opts.Token, Since: opts.Since})
	if err != nil {
		return nil, err
	}
	return &Attachment{conn: conn, token: reply.Token, Offset: reply.Offset, Snapshot: reply.Snapshot,
		HistoryGap: reply.HistoryGap}, nil
}

// request connects to rec's worker and makes req, returning the connection and the reply.
// A worker that's gone has its record removed, and is ErrNotFound: its session has ended.
func (m *Manager) request(ctx context.Context, rec record, req message) (net.Conn, message, error) {
	ctx, cancel := context.WithTimeout(ctx, dialTimeout)
	defer cancel()
	var dialer net.Dialer
	conn, err := dialer.DialContext(ctx, "unix", socketPath(m.dir, rec.ID))
	if err != nil {
		if workerGone(err) {
			removeSession(m.dir, rec.ID)
			return nil, message{}, ErrNotFound
		}
		return nil, message{}, fmt.Errorf("%w: %w", ErrUnavailable, err)
	}
	deadline, _ := ctx.Deadline()
	_ = conn.SetDeadline(deadline)
	req.Version, req.Secret = protocolVersion, rec.Secret
	if err := writeControl(conn, req); err != nil {
		_ = conn.Close()
		return nil, message{}, fmt.Errorf("%w: %w", ErrUnavailable, err)
	}
	reply, err := readControl(conn)
	if err != nil {
		_ = conn.Close()
		// The worker may be exiting, as its session ends or it dies: once it's gone,
		// connecting again tells.
		time.Sleep(goneCheckDelay)
		if again, dialErr := dialer.DialContext(ctx, "unix", socketPath(m.dir, rec.ID)); dialErr == nil {
			_ = again.Close()
		} else if workerGone(dialErr) {
			removeSession(m.dir, rec.ID)
			return nil, message{}, ErrNotFound
		}
		return nil, message{}, fmt.Errorf("%w: %w", ErrUnavailable, err)
	}
	_ = conn.SetDeadline(time.Time{})
	if reply.Op == opError {
		_ = conn.Close()
		switch reply.Code {
		case codeAttached:
			return nil, message{}, ErrAttached
		case codeEnded:
			return nil, message{}, ErrNotFound
		default:
			return nil, message{}, fmt.Errorf("%w: %s", ErrUnavailable, reply.Error)
		}
	}
	return conn, reply, nil
}

// workerGone reports whether connecting to a worker failed because nothing listens on its
// socket anymore.
func workerGone(err error) bool {
	return errors.Is(err, syscall.ECONNREFUSED) || errors.Is(err, os.ErrNotExist) ||
		errors.Is(err, syscall.ENOENT) || isWindowsConnRefused(err)
}

// Attachment is a connection controlling a session: its output and end come from Next,
// its input goes to Write.
//
// Its output starts with Snapshot bytes restoring the session's screen as it was at output
// offset Offset; the session's output from there follows. A client that counts the output
// after the snapshot knows the offset it's seen up to, to attach again with as Since.
type Attachment struct {
	conn    net.Conn
	token   string
	writeMu sync.Mutex

	Offset   uint64
	Snapshot int
	// HistoryGap tells that history the snapshot would have restored is gone.
	HistoryGap bool
}

// Token identifies this attachment, for its client to attach again without a takeover.
func (a *Attachment) Token() string { return a.token }

// Event is what happened in an attached session: output, or its end for this attachment.
type Event struct {
	Output []byte
	// Exit is set once the shell exited, with its status. The attachment ends with it.
	Exit *int
	// Displaced is set when another attachment took the session over. The attachment
	// ends with it.
	Displaced bool
}

// Next returns the session's next event. It fails once the attachment is over without an
// exit or takeover, such as the worker dying.
func (a *Attachment) Next() (Event, error) {
	for {
		kind, msg, data, err := readFrame(a.conn)
		if err != nil {
			return Event{}, err
		}
		if kind == frameData {
			return Event{Output: data}, nil
		}
		switch msg.Op {
		case opExit:
			code := -1
			if msg.ExitCode != nil {
				code = *msg.ExitCode
			}
			return Event{Exit: &code}, nil
		case opDetached:
			return Event{Displaced: true}, nil
		}
	}
}

// Write sends input to the session.
func (a *Attachment) Write(input []byte) error {
	return a.send(func(w io.Writer) error { return writeData(w, input) })
}

// Resize sets the session's terminal size.
func (a *Attachment) Resize(cols, rows uint16) error {
	return a.send(func(w io.Writer) error { return writeControl(w, message{Op: opSize, Cols: cols, Rows: rows}) })
}

// Detach leaves the session running without this attachment. Next ends once the session
// has let go of it: attaching again after that finds it detached. Close it then.
func (a *Attachment) Detach() error {
	return a.send(func(w io.Writer) error { return writeControl(w, message{Op: opDetach}) })
}

// Close ends the attachment, which detaches it.
func (a *Attachment) Close() error { return a.conn.Close() }

func (a *Attachment) send(fn func(io.Writer) error) error {
	a.writeMu.Lock()
	defer a.writeMu.Unlock()
	_ = a.conn.SetWriteDeadline(time.Now().Add(attachWriteTimeout))
	return fn(a.conn)
}
