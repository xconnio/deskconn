package terminal

import (
	"bufio"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"runtime/debug"
	"sync"
	"time"

	pty "github.com/aymanbagabas/go-pty"

	"github.com/xconnio/deskconn/internal/common"
)

// Spec is how to start a session, handed to a new worker on its standard input.
type Spec struct {
	Dir     string   `json:"dir"` // the terminals directory
	ID      string   `json:"id"`
	Owner   string   `json:"owner"`
	Secret  string   `json:"secret"`
	Command string   `json:"command"`
	Args    []string `json:"args,omitempty"`
	Env     []string `json:"env,omitempty"` // empty: the worker's own environment
	WorkDir string   `json:"work_dir,omitempty"`
	Cols    uint16   `json:"cols"`
	Rows    uint16   `json:"rows"`
	// HistoryLimit is the bytes of output history kept (see history).
	HistoryLimit int `json:"history_limit"`
}

const (
	// readyLine and errorPrefix are what a worker prints on standard output once it's
	// serving its session, or failed to start it.
	readyLine   = "ready"
	errorPrefix = "error: "

	// handshakeTimeout bounds a connection's first request.
	handshakeTimeout = 10 * time.Second
	// attachWriteTimeout bounds one write to the attachment: an attachment that stops
	// reading is dropped, rather than stopping the session's output.
	attachWriteTimeout = 30 * time.Second
	// attachQueueLimit bounds the output queued for an attachment beyond its snapshot: one
	// that falls further behind is dropped, rather than stopping the session's output or
	// holding more of it. Attaching again restores the screen.
	attachQueueLimit = 8 << 20
	// snapshotChunk is the most of a snapshot sent in one frame.
	snapshotChunk = 64 << 10
	// workerMemoryHeadroom is a worker's memory beyond its history: the screens, the parser,
	// an attachment's queue and a snapshot of the history, besides the runtime's own.
	workerMemoryHeadroom = 64 << 20
	// exitDeliveryTimeout bounds waiting for the attachment to take the exit status.
	exitDeliveryTimeout = 5 * time.Second
	// exitWaitTimeout bounds waiting for the shell's exit status once its output ended.
	exitWaitTimeout = 2 * time.Second
	// terminateGrace is how long the shell has to exit on its hangup before it and its
	// session's processes are killed.
	terminateGrace = 3 * time.Second
)

type worker struct {
	spec     Spec
	created  time.Time
	ptmx     pty.Pty
	shellPID int
	procs    processTracker
	listener net.Listener

	exited   chan struct{} // closed once the shell has exited and exitCode is set
	exitCode int           // -1 if unknown
	closePTY sync.Once

	// outMu orders the session's output: what the screen has taken in, and what's been
	// queued for the attachment after its snapshot.
	outMu  sync.Mutex
	screen *screen
	// ptyCols and ptyRows are the PTY's size, as last set.
	ptyCols, ptyRows int

	mu      sync.Mutex
	current *attachment
	ended   bool
	// terminations counts terminate requests not yet answered, which the worker waits
	// for before it exits.
	terminations sync.WaitGroup
}

// attachment is one connection controlling the session. What's sent to it is queued, in
// order, and written by its own goroutine, so a slow connection holds up nothing else.
type attachment struct {
	conn  net.Conn
	token string
	done  chan struct{} // closed once its input is finished with

	mu      sync.Mutex
	wake    *sync.Cond
	queue   [][]byte
	queued  int
	limit   int  // queued bytes beyond which output isn't queued
	closing bool // write what's queued, then close
	closed  bool
}

func newAttachment(conn net.Conn, token string) *attachment {
	a := &attachment{conn: conn, token: token, done: make(chan struct{}), limit: attachQueueLimit}
	a.wake = sync.NewCond(&a.mu)
	return a
}

// send queues frame. It fails, queuing nothing, if that would be over the limit and !force.
// An attachment that's closing takes nothing more.
func (a *attachment) send(frame []byte, force bool) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.closed || a.closing {
		return true
	}
	if !force && a.queued+len(frame) > a.limit {
		return false
	}
	a.queue = append(a.queue, frame)
	a.queued += len(frame)
	a.wake.Signal()
	return true
}

// finish writes frame after what's queued, or instead of it if discard, and then closes.
func (a *attachment) finish(frame []byte, discard bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.closed || a.closing {
		return
	}
	if discard {
		a.queue, a.queued = nil, 0
	}
	if frame != nil {
		a.queue = append(a.queue, frame)
		a.queued += len(frame)
	}
	a.closing = true
	a.wake.Signal()
}

// abort closes the connection, dropping what's queued.
func (a *attachment) abort() {
	a.mu.Lock()
	a.closed, a.queue, a.queued = true, nil, 0
	a.wake.Signal()
	a.mu.Unlock()
	_ = a.conn.Close()
}

// writeLoop writes what's queued until the attachment closes.
func (a *attachment) writeLoop() {
	defer a.conn.Close()
	for {
		a.mu.Lock()
		for len(a.queue) == 0 && !a.closing && !a.closed {
			a.wake.Wait()
		}
		if a.closed || len(a.queue) == 0 {
			a.mu.Unlock()
			return
		}
		frame := a.queue[0]
		a.queue[0] = nil
		a.queue = a.queue[1:]
		a.queued -= len(frame)
		a.mu.Unlock()
		_ = a.conn.SetWriteDeadline(time.Now().Add(attachWriteTimeout))
		if common.WriteFrame(a.conn, frame) != nil {
			a.abort()
			return
		}
	}
}

// RunWorker runs one session: it reads the session's Spec from stdin, starts the shell,
// reports readiness on stdout and serves the session until its shell exits or it's
// terminated.
func RunWorker(stdin io.Reader, stdout io.WriteCloser) error {
	var spec Spec
	err := json.NewDecoder(stdin).Decode(&spec)
	if err == nil {
		err = validateSpec(spec)
	}
	var w *worker
	if err == nil {
		// The garbage collector works harder rather than let the worker grow past what its
		// session needs.
		debug.SetMemoryLimit(int64(2*spec.HistoryLimit) + workerMemoryHeadroom)
		w, err = startWorker(spec)
	}
	if err != nil {
		_, _ = fmt.Fprintf(stdout, "%s%v\n", errorPrefix, err)
		_ = stdout.Close()
		return err
	}
	_, _ = fmt.Fprintln(stdout, readyLine)
	_ = stdout.Close()
	w.serve()
	return nil
}

func validateSpec(spec Spec) error {
	switch {
	case !validID(spec.ID):
		return errors.New("invalid session ID")
	case spec.Dir == "", spec.Command == "", spec.Owner == "", spec.Secret == "":
		return errors.New("incomplete session spec")
	}
	return nil
}

func startWorker(spec Spec) (*worker, error) {
	if err := ensureDir(spec.Dir); err != nil {
		return nil, err
	}
	sock := socketPath(spec.Dir, spec.ID)
	listener, err := net.Listen("unix", sock)
	if err != nil {
		return nil, fmt.Errorf("listen: %w", err)
	}
	_ = os.Chmod(sock, 0o600)

	cols, rows := int(spec.Cols), int(spec.Rows)
	if cols == 0 || rows == 0 {
		cols, rows = 80, 24
	}
	w := &worker{spec: spec, created: time.Now().UTC(), listener: listener, exited: make(chan struct{}),
		screen: newScreen(cols, rows, spec.HistoryLimit)}
	w.ptyCols, w.ptyRows = w.screen.cols, w.screen.rows
	if err := w.startShell(); err != nil {
		_ = listener.Close()
		removeSession(spec.Dir, spec.ID)
		return nil, err
	}
	rec := record{Version: protocolVersion, ID: spec.ID, Owner: spec.Owner, Command: spec.Command,
		Created: w.created, PID: os.Getpid(), Secret: spec.Secret, HistoryLimit: spec.HistoryLimit}
	if err := writeRecord(spec.Dir, rec); err != nil {
		w.procs.kill()
		_ = listener.Close()
		removeSession(spec.Dir, spec.ID)
		return nil, fmt.Errorf("write session record: %w", err)
	}
	return w, nil
}

func (w *worker) startShell() error {
	ptmx, err := pty.New()
	if err != nil {
		return fmt.Errorf("start PTY: %w", err)
	}
	_ = ptmx.Resize(w.ptyCols, w.ptyRows)
	cmd := ptmx.Command(w.spec.Command, w.spec.Args...)
	cmd.Dir = w.spec.WorkDir
	if len(w.spec.Env) > 0 {
		cmd.Env = w.spec.Env
	}
	if err := cmd.Start(); err != nil {
		_ = ptmx.Close()
		return fmt.Errorf("start %s: %w", w.spec.Command, err)
	}
	w.ptmx = ptmx
	w.shellPID = cmd.Process.Pid
	w.procs = trackProcesses(cmd.Process.Pid)
	go func() {
		_ = cmd.Wait()
		w.exitCode = processExitCode(cmd.ProcessState)
		close(w.exited)
		if ptyNeedsCloseOnExit {
			// ConPTY outlives its process: close it once the last output had time to arrive,
			// so reading it ends.
			time.Sleep(ptyDrainDelay)
			w.closePty()
		}
	}()
	// go-pty keeps the slave open for the PTY's lifetime; reading the master only ends,
	// once the shell exits, without it.
	if unixPtmx, ok := ptmx.(pty.UnixPty); ok {
		_ = unixPtmx.Slave().Close()
	}
	return nil
}

func (w *worker) closePty() {
	w.closePTY.Do(func() { _ = w.ptmx.Close() })
}

// serve serves the session until it ends.
func (w *worker) serve() {
	stopSignals := handleSignals(w.terminate)
	defer stopSignals()
	go w.acceptLoop()
	w.readOutput()
	w.finish()
}

func (w *worker) acceptLoop() {
	for {
		conn, err := w.listener.Accept()
		if err != nil {
			return
		}
		go w.handleConn(conn)
	}
}

// readOutput takes in the session's output until it ends: the screen keeps it, and the
// attachment, if any, is sent it. It's read whether or not anyone's attached, so a command
// writing while nobody watches never blocks on a full terminal.
func (w *worker) readOutput() {
	buf := make([]byte, 32*1024)
	for {
		n, err := w.ptmx.Read(buf)
		if n > 0 {
			w.output(buf[:n])
		}
		if err != nil {
			return
		}
	}
}

func (w *worker) output(data []byte) {
	w.outMu.Lock()
	defer w.outMu.Unlock()
	w.feed(data)
	w.mu.Lock()
	a := w.current
	w.mu.Unlock()
	if a != nil && !a.send(dataFrame(data), false) {
		// Too far behind: it's dropped, and attaching again restores the screen.
		w.drop(a)
	}
}

// feed runs data through the screen. Should the screen fail on it, it starts afresh, keeping
// the history: the session goes on, and its screen is right again once redrawn.
func (w *worker) feed(data []byte) {
	start := w.screen.offset
	defer func() {
		if recover() != nil {
			fresh := newScreen(w.screen.cols, w.screen.rows, 0)
			fresh.history, fresh.offset = w.screen.history, start+uint64(len(data))
			w.screen = fresh
		}
	}()
	w.screen.write(data)
}

// finish ends the session once its output ended: it reports the shell's exit status to the
// attachment, if any, kills what's left of the shell's process group, and removes the
// session.
func (w *worker) finish() {
	code, ok := w.waitExit(exitWaitTimeout)
	if !ok {
		// The terminal closed while the shell runs on.
		w.procs.kill()
		code, ok = w.waitExit(exitWaitTimeout)
	}

	w.mu.Lock()
	w.ended = true
	a := w.current
	w.mu.Unlock()
	// Answer whoever asked for the termination before going.
	answered := make(chan struct{})
	go func() { w.terminations.Wait(); close(answered) }()
	select {
	case <-answered:
	case <-time.After(exitDeliveryTimeout):
	}
	if a != nil && ok {
		exitCode := code
		// After the output queued before it, then the attachment closes.
		a.finish(controlFrame(message{Op: opExit, ExitCode: &exitCode}), false)
		select {
		case <-a.done:
		case <-time.After(exitDeliveryTimeout):
		}
	}
	if a != nil {
		a.abort()
	}
	_ = w.listener.Close()
	removeSession(w.spec.Dir, w.spec.ID)
	w.procs.killGroup()
	w.procs.release()
	w.closePty()
}

func (w *worker) waitExit(timeout time.Duration) (int, bool) {
	select {
	case <-w.exited:
		return w.exitCode, w.exitCode >= 0
	case <-time.After(timeout):
		return 0, false
	}
}

// terminate ends the shell and every process of its session: a hangup first, as closing a
// terminal would, then a kill once terminateGrace has passed.
func (w *worker) terminate() {
	w.procs.hangup()
	select {
	case <-w.exited:
	case <-time.After(terminateGrace):
	}
	w.procs.kill()
}

func (w *worker) info() *SessionInfo {
	w.mu.Lock()
	defer w.mu.Unlock()
	return &SessionInfo{ID: w.spec.ID, Command: w.spec.Command, Created: w.created, Attached: w.current != nil}
}

func (w *worker) handleConn(conn net.Conn) {
	_ = conn.SetDeadline(time.Now().Add(handshakeTimeout))
	req, err := readControl(conn)
	if err != nil {
		_ = conn.Close()
		return
	}
	if req.Version < 1 || subtle.ConstantTimeCompare([]byte(req.Secret), []byte(w.spec.Secret)) != 1 {
		_ = writeControl(conn, message{Op: opError, Code: codeUnauthorized, Error: "unauthorized"})
		_ = conn.Close()
		return
	}
	switch req.Op {
	case opInfo:
		_ = writeControl(conn, message{Op: opOK, Info: w.info()})
		_ = conn.Close()
	case opTerminate:
		w.mu.Lock()
		if w.ended {
			w.mu.Unlock()
			_ = writeControl(conn, message{Op: opError, Code: codeEnded, Error: "the session has ended"})
			_ = conn.Close()
			return
		}
		w.terminations.Add(1)
		w.mu.Unlock()
		defer w.terminations.Done()
		w.terminate()
		_ = conn.SetDeadline(time.Now().Add(handshakeTimeout))
		_ = writeControl(conn, message{Op: opOK})
		_ = conn.Close()
	case opAttach:
		w.attach(conn, req)
	default:
		_ = writeControl(conn, message{Op: opError, Code: codeInvalid, Error: "unknown request " + req.Op})
		_ = conn.Close()
	}
}

// attach makes conn the session's attachment and serves its input until it detaches, is
// replaced, or its connection ends. It's first sent the reply, then a snapshot restoring the
// screen, then the output that follows: the snapshot is taken, and the attachment made
// current, at one point of the output, so nothing's missed or sent twice. Only the current
// attachment's input and resizes reach the terminal.
func (w *worker) attach(conn net.Conn, req message) {
	token, err := randomHex(16)
	if err != nil {
		_ = conn.Close()
		return
	}
	a := newAttachment(conn, token)

	w.outMu.Lock()
	w.mu.Lock()
	var refusal *message
	prev := w.current
	switch {
	case w.ended:
		refusal = &message{Op: opError, Code: codeEnded, Error: "the session has ended"}
	case prev != nil && !req.Takeover &&
		(req.Token == "" || subtle.ConstantTimeCompare([]byte(req.Token), []byte(prev.token)) != 1):
		refusal = &message{Op: opError, Code: codeAttached, Error: "the session is attached elsewhere"}
	default:
		w.current = a
	}
	w.mu.Unlock()
	if refusal != nil {
		w.outMu.Unlock()
		_ = writeControl(conn, *refusal)
		_ = conn.Close()
		return
	}
	resized := w.resizeScreen(req.Cols, req.Rows)
	snapshot, gap := w.screen.snapshot(req.Since)
	a.send(controlFrame(message{Op: opOK, Token: token, Offset: w.screen.offset, Snapshot: len(snapshot),
		HistoryGap: gap}), true)
	for len(snapshot) > 0 {
		n := min(len(snapshot), snapshotChunk)
		a.send(dataFrame(snapshot[:n]), true)
		snapshot = snapshot[n:]
	}
	a.mu.Lock()
	a.limit = a.queued + attachQueueLimit
	a.mu.Unlock()
	w.outMu.Unlock()

	go a.writeLoop()
	defer func() {
		w.release(a)
		close(a.done)
		a.finish(nil, false)
	}()
	if prev != nil {
		// Told it's been taken over instead of what it has yet to be sent.
		prev.finish(controlFrame(message{Op: opDetached}), true)
	}
	if resized {
		w.resizePTY()
	}

	_ = conn.SetReadDeadline(time.Time{})
	for {
		kind, msg, data, err := readFrame(conn)
		if err != nil || !w.isCurrent(a) {
			return
		}
		switch {
		case kind == frameData:
			_, _ = w.ptmx.Write(data)
		case msg.Op == opSize:
			w.outMu.Lock()
			resized := w.resizeScreen(msg.Cols, msg.Rows)
			w.outMu.Unlock()
			if resized {
				w.resizePTY()
			}
		case msg.Op == opDetach:
			return
		}
	}
}

// resizeScreen sets the screen's size, before the PTY's: the programs' output after they
// see the new size is for it. It reports whether the PTY's size is then to change. outMu is
// held.
func (w *worker) resizeScreen(cols, rows uint16) bool {
	if cols == 0 || rows == 0 {
		return false
	}
	w.screen.resize(int(cols), int(rows))
	return w.screen.cols != w.ptyCols || w.screen.rows != w.ptyRows
}

// resizePTY sets the PTY's size to the screen's.
func (w *worker) resizePTY() {
	w.outMu.Lock()
	cols, rows := w.screen.cols, w.screen.rows
	w.ptyCols, w.ptyRows = cols, rows
	w.outMu.Unlock()
	_ = w.ptmx.Resize(cols, rows)
}

func (w *worker) isCurrent(a *attachment) bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.current == a
}

// release detaches a, if it's still the attachment.
func (w *worker) release(a *attachment) {
	w.mu.Lock()
	if w.current == a {
		w.current = nil
	}
	w.mu.Unlock()
}

// drop detaches a, whose connection failed or fell behind, and closes it.
func (w *worker) drop(a *attachment) {
	w.release(a)
	a.abort()
}

// readWorkerReady reads a starting worker's report from its standard output.
func readWorkerReady(r io.Reader) error {
	line, err := bufio.NewReader(r).ReadString('\n')
	switch {
	case line == readyLine+"\n":
		return nil
	case len(line) > len(errorPrefix) && line[:len(errorPrefix)] == errorPrefix:
		return &workerStartError{msg: line[len(errorPrefix) : len(line)-1]}
	case err != nil:
		return fmt.Errorf("terminal worker exited before it was ready: %w", err)
	default:
		return fmt.Errorf("unexpected terminal worker output %q", line)
	}
}

// workerStartError is the worker reporting that it couldn't start the session, as opposed
// to the worker itself failing to run.
type workerStartError struct{ msg string }

func (e *workerStartError) Error() string { return e.msg }

// processExitCode is state's exit status, shell style: 128+N for a process killed by signal
// N, -1 if there's no telling.
func processExitCode(state *os.ProcessState) int {
	if state == nil {
		return -1
	}
	if code := state.ExitCode(); code >= 0 {
		return code
	}
	return signaledExitCode(state)
}
