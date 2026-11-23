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
	"sync"
	"time"

	pty "github.com/aymanbagabas/go-pty"
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
	// initialOutputHold is how long a new session's output waits for its first attachment,
	// so the creator sees the shell's first prompt. Output is read regardless after that.
	initialOutputHold = 10 * time.Second
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

	startReader sync.Once
	readerGo    chan struct{} // closed to start reading output
	closePTY    sync.Once

	mu       sync.Mutex
	current  *attachment
	attached bool // ever attached
	ended    bool
	// terminations counts terminate requests not yet answered, which the worker waits
	// for before it exits.
	terminations sync.WaitGroup
	cols         uint16
	rows         uint16
}

// attachment is one connection controlling the session.
type attachment struct {
	conn    net.Conn
	token   string
	writeMu sync.Mutex
	done    chan struct{} // closed once its connection is finished with
}

func (a *attachment) write(fn func(io.Writer) error) error {
	a.writeMu.Lock()
	defer a.writeMu.Unlock()
	_ = a.conn.SetWriteDeadline(time.Now().Add(attachWriteTimeout))
	return fn(a.conn)
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

	w := &worker{spec: spec, created: time.Now().UTC(), listener: listener, exited: make(chan struct{}),
		readerGo: make(chan struct{}), cols: spec.Cols, rows: spec.Rows}
	if err := w.startShell(); err != nil {
		_ = listener.Close()
		removeSession(spec.Dir, spec.ID)
		return nil, err
	}
	rec := record{Version: protocolVersion, ID: spec.ID, Owner: spec.Owner, Command: spec.Command,
		Created: w.created, PID: os.Getpid(), Secret: spec.Secret}
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
	if w.cols > 0 && w.rows > 0 {
		_ = ptmx.Resize(int(w.cols), int(w.rows))
	}
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
	time.AfterFunc(initialOutputHold, w.beginOutput)
	<-w.readerGo
	w.readOutput()
	w.finish()
}

func (w *worker) beginOutput() { w.startReader.Do(func() { close(w.readerGo) }) }

func (w *worker) acceptLoop() {
	for {
		conn, err := w.listener.Accept()
		if err != nil {
			return
		}
		go w.handleConn(conn)
	}
}

// readOutput delivers the session's output to its attachment until the output ends.
// Without an attachment the output is read and dropped, so a command writing while nobody
// watches never blocks on a full terminal.
func (w *worker) readOutput() {
	buf := make([]byte, 32*1024)
	for {
		n, err := w.ptmx.Read(buf)
		if n > 0 {
			w.mu.Lock()
			a := w.current
			w.mu.Unlock()
			if a != nil {
				if werr := a.write(func(out io.Writer) error { return writeData(out, buf[:n]) }); werr != nil {
					w.drop(a)
				}
			}
		}
		if err != nil {
			return
		}
	}
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
		if a.write(func(out io.Writer) error { return writeControl(out, message{Op: opExit, ExitCode: &exitCode}) }) == nil {
			select {
			case <-a.done:
			case <-time.After(exitDeliveryTimeout):
			}
		}
	}
	if a != nil {
		_ = a.conn.Close()
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
	// The output may still be waiting for a first attachment.
	w.beginOutput()
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
// replaced, or its connection ends. Only the current attachment's input and resizes reach
// the terminal.
func (w *worker) attach(conn net.Conn, req message) {
	token, err := randomHex(16)
	if err != nil {
		_ = conn.Close()
		return
	}
	a := &attachment{conn: conn, token: token, done: make(chan struct{})}
	defer func() {
		w.release(a)
		close(a.done)
		_ = conn.Close()
	}()

	// Hold the new attachment's writes until it has been told it's attached, so output
	// can't overtake the reply.
	a.writeMu.Lock()
	w.mu.Lock()
	if w.ended {
		w.mu.Unlock()
		a.writeMu.Unlock()
		_ = writeControl(conn, message{Op: opError, Code: codeEnded, Error: "the session has ended"})
		return
	}
	prev := w.current
	sameClient := prev != nil && req.Token != "" &&
		subtle.ConstantTimeCompare([]byte(req.Token), []byte(prev.token)) == 1
	if prev != nil && !req.Takeover && !sameClient {
		w.mu.Unlock()
		a.writeMu.Unlock()
		_ = writeControl(conn, message{Op: opError, Code: codeAttached,
			Error: "the session is attached elsewhere"})
		return
	}
	reattach := w.attached
	w.current, w.attached = a, true
	w.mu.Unlock()
	if prev != nil {
		go w.displace(prev)
	}

	_ = conn.SetDeadline(time.Time{})
	err = writeControl(conn, message{Op: opOK, Token: token})
	a.writeMu.Unlock()
	if err != nil {
		return
	}
	w.resize(req.Cols, req.Rows, reattach)
	w.beginOutput()

	for {
		kind, msg, data, err := readFrame(conn)
		if err != nil || !w.isCurrent(a) {
			return
		}
		switch {
		case kind == frameData:
			_, _ = w.ptmx.Write(data)
		case msg.Op == opSize:
			w.resize(msg.Cols, msg.Rows, false)
		case msg.Op == opDetach:
			return
		}
	}
}

// resize sets the terminal's size. redraw also has the programs in it redraw at an unchanged
// size, as a client attaching again has no screen to show until they do.
func (w *worker) resize(cols, rows uint16, redraw bool) {
	if cols == 0 || rows == 0 {
		return
	}
	w.mu.Lock()
	same := cols == w.cols && rows == w.rows
	w.cols, w.rows = cols, rows
	w.mu.Unlock()
	if same && redraw && rows > 1 {
		_ = w.ptmx.Resize(int(cols), int(rows-1))
	}
	if !same || redraw {
		_ = w.ptmx.Resize(int(cols), int(rows))
	}
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

// drop detaches a, whose connection failed, and closes it.
func (w *worker) drop(a *attachment) {
	w.release(a)
	_ = a.conn.Close()
}

// displace tells prev it was taken over and closes it.
func (w *worker) displace(prev *attachment) {
	_ = prev.write(func(out io.Writer) error { return writeControl(out, message{Op: opDetached}) })
	_ = prev.conn.Close()
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
