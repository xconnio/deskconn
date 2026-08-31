package deskconn

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"sync"
	"time"

	pty "github.com/aymanbagabas/go-pty"
	log "github.com/sirupsen/logrus"

	"github.com/xconnio/xconn-go"
)

type encryptionKeys struct {
	sendKey    []byte
	receiveKey []byte
}

// migrationTokenTTL bounds how long a migration token stays usable after it's issued,
// so a captured ciphertext blob can't be replayed indefinitely.
const migrationTokenTTL = 30 * time.Second

type migrationToken struct {
	value    string
	issuedAt time.Time
}

// shellTransport is how a running PTY's output gets back to whoever's
// attached to it, and is the one thing that changes on a live migration.
type shellTransport interface {
	// writeOutput encrypts and delivers one chunk of PTY output.
	writeOutput(plaintext []byte) error
	// close signals end-of-output on this transport.
	close() error
}

type ptySession struct {
	mu        sync.Mutex
	transport shellTransport
	closeOnce sync.Once
}

// closePty closes ptmx at most once, however many of startOutputReader's own
// cleanup, cleanupShellID, and closePtyOnProcessExit race to call it --
// double-closing a *nix pty is harmless, but double-closing a Windows ConPTY
// handle is not (see closePtyOnProcessExit's doc comment in shell_windows.go).
func (ps *ptySession) closePty(ptmx pty.Pty) {
	ps.closeOnce.Do(func() {
		if err := ptmx.Close(); err != nil && !errors.Is(err, os.ErrClosed) {
			log.Printf("Error closing PTY: %v", err)
		}
	})
}

type interactiveShellSession struct {
	ptmx            map[string]pty.Pty
	sessions        map[string]*ptySession
	migrationTokens map[string]migrationToken
	pids            map[string]int        // shell ID → PTY child PID, for /proc cwd lookups
	agentForward    *agentForwardSessions // set by NewDeskconn; may be nil, check before use
	sync.Mutex
}

func newInteractiveShellSession() *interactiveShellSession {
	return &interactiveShellSession{
		ptmx:            make(map[string]pty.Pty),
		sessions:        make(map[string]*ptySession),
		migrationTokens: make(map[string]migrationToken),
		pids:            make(map[string]int),
	}
}

// issueMigrationToken generates and stores a fresh migration token for
// shellID, returning it for the caller to deliver however its transport
// does control messages.
func (p *interactiveShellSession) issueMigrationToken(shellID string) string {
	tokenBytes := make([]byte, 32)
	if _, err := rand.Read(tokenBytes); err != nil {
		return ""
	}
	token := hex.EncodeToString(tokenBytes)

	p.Lock()
	p.migrationTokens[shellID] = migrationToken{value: token, issuedAt: time.Now()}
	p.Unlock()

	return token
}

func (p *interactiveShellSession) cleanupShellID(shellID string) {
	p.Lock()
	stored, hasPtmx := p.ptmx[shellID]
	ps, hasSession := p.sessions[shellID]
	if hasPtmx {
		delete(p.ptmx, shellID)
	}
	killShellProcessGroup(p.pids[shellID])
	delete(p.sessions, shellID)
	delete(p.migrationTokens, shellID)
	delete(p.pids, shellID)
	p.Unlock()

	if hasPtmx && hasSession {
		ps.closePty(stored)
	}
}

// cwdForShell reads the live working directory of an existing shell straight from
// the OS, so a new tab can start.
func (p *interactiveShellSession) cwdForShell(shellID string) (string, error) {
	p.Lock()
	pid, ok := p.pids[shellID]
	p.Unlock()
	if !ok {
		return "", fmt.Errorf("no such shell: %s", shellID)
	}
	return os.Readlink(fmt.Sprintf("/proc/%d/cwd", pid))
}

// isBusy reports whether some other process (not the shell itself) currently
// owns the pty's foreground process group — the same check a local terminal
// (e.g. GNOME Terminal) makes via tcgetpgrp before warning on tab close.
func (p *interactiveShellSession) isBusy(shellID string) (bool, error) {
	p.Lock()
	ptmx, ptmxOk := p.ptmx[shellID]
	pid, pidOk := p.pids[shellID]
	p.Unlock()
	if !ptmxOk || !pidOk {
		return false, fmt.Errorf("no such shell: %s", shellID)
	}

	return foregroundPGIDDiffers(ptmx, pid)
}

func (p *interactiveShellSession) handleShellIsBusy() func(_ context.Context,
	inv *xconn.Invocation) *xconn.InvocationResult {
	return func(_ context.Context, inv *xconn.Invocation) *xconn.InvocationResult {
		shellID, err := inv.ArgString(0)
		if err != nil {
			return xconn.NewInvocationError(ErrInvalidArgument, err.Error())
		}

		busy, err := p.isBusy(shellID)
		if err != nil {
			return xconn.NewInvocationResult(false)
		}
		return xconn.NewInvocationResult(busy)
	}
}

// resolveStartDir picks the start dir: prevShellID's live cwd if given and
// still running, or home.
func (p *interactiveShellSession) resolveStartDir(prevShellID string) (string, error) {
	if prevShellID != "" {
		if dir, err := p.cwdForShell(prevShellID); err == nil {
			return dir, nil
		}
	}

	homeDir, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("failed to get home dir: %w", err)
	}
	return homeDir, nil
}

// agentSockForAuthID returns the forwarded SSH agent socket path for
// authID (self-reported by the client in shellControlMsg), or "" if agent
// forwarding isn't active for it.
func (p *interactiveShellSession) agentSockForAuthID(authID string) string {
	if p.agentForward == nil || authID == "" {
		return ""
	}
	path, ok := p.agentForward.socketPathByAuthID(authID)
	if !ok {
		return ""
	}
	return path
}

// startPtySession starts command in a new PTY but does not yet start
// reading its output -- the caller must invoke the returned startReader
// once it's safe for output to start flowing (see beginShellSession's doc
// comment for why this is deferred).
//
// agentSockPath, when non-empty, is exported as SSH_AUTH_SOCK in the spawned process's
// environment so tools run in the shell (git, ssh, ...) can use the caller's forwarded
// local SSH agent — see RunAgentForward/handleAgentForward in agentforward.go.
//
// cols/rows set the PTY's initial size right after it's created, before anything can read
// from it, rather than via a separate resize call once the reader is already running --
// racing a resize against startOutputReader's first Read would touch the pty's internal
// state concurrently from two goroutines.
//
// echo is left enabled (the PTY's default) for an interactive shell, where
// the user needs to see what they type. For a one-shot exec, it's disabled:
// exec's client forwards stdin unconditionally even though most exec'd
// commands never read it, and the PTY driver's default echoing (in
// particular ECHOCTL rendering a stray control byte as its two-character
// caret form, e.g. an EOT arriving as the client's stdin closes) would
// otherwise leak extra bytes into the exec'd command's own output, racing
// with and sometimes landing right before its real first output.
func (p *interactiveShellSession) startPtySession(transport shellTransport, shellID, agentSockPath,
	prevShellID, command string, cols, rows uint16, echo bool, args ...string) (
	ptmx pty.Pty, startReader func(), err error) {
	dir, err := p.resolveStartDir(prevShellID)
	if err != nil {
		return nil, nil, err
	}

	ptmx, err = pty.New()
	if err != nil {
		return nil, nil, fmt.Errorf("failed to start PTY: %w", err)
	}
	_ = ptmx.Resize(int(cols), int(rows))
	if !echo {
		disablePTYEcho(ptmx)
	}

	if resolved, lookErr := exec.LookPath(command); lookErr == nil {
		command = resolved
	}

	cmd := ptmx.Command(command, args...)
	cmd.Dir = dir
	if agentSockPath != "" {
		cmd.Env = append(os.Environ(), "SSH_AUTH_SOCK="+agentSockPath)
	}

	if err := cmd.Start(); err != nil {
		_ = ptmx.Close()
		return nil, nil, fmt.Errorf("failed to start PTY: %w", err)
	}
	ps := &ptySession{transport: transport}
	// Nothing else calls Wait, so without this the process stays a zombie
	// (invisible to killShellProcessGroup, but still a process-table entry)
	// forever after it exits, however that happens. closePtyOnProcessExit
	// additionally unblocks startOutputReader's Read on platforms where the
	// pty doesn't signal that on its own -- see its doc comment.
	SafeGo(func() {
		_ = cmd.Wait()
		closePtyOnProcessExit(ps, ptmx)
	})
	// go-pty keeps its own slave fd open for the pty's lifetime; without closing
	// it here, the master's Read never sees EOF/EIO after the child exits, since
	// the kernel still sees an open slave reference in this process.
	if unixPtmx, ok := ptmx.(pty.UnixPty); ok {
		_ = unixPtmx.Slave().Close()
	}

	p.Lock()
	p.ptmx[shellID] = ptmx
	p.sessions[shellID] = ps
	p.pids[shellID] = cmd.Process.Pid
	p.Unlock()

	return ptmx, func() { SafeGo(func() { p.startOutputReader(ptmx, ps, shellID) }) }, nil
}

func (p *interactiveShellSession) startOutputReader(ptmx pty.Pty, ps *ptySession, shellID string) {
	defer func() {
		p.Lock()
		shouldClose := p.ptmx[shellID] == ptmx
		pid := p.pids[shellID]
		delete(p.ptmx, shellID)
		delete(p.sessions, shellID)
		delete(p.pids, shellID)
		p.Unlock()
		if shouldClose {
			ps.closePty(ptmx)
			// Usually a no-op (the read loop below ending almost always means
			// the process already exited on its own); matters when it ended
			// because writeOutput failed instead, with the process possibly
			// still running -- see killShellProcessGroup's doc comment.
			killShellProcessGroup(pid)
		}
	}()
	buf := make([]byte, 4096)
	for {
		n, err := ptmx.Read(buf)
		ps.mu.Lock()
		transport := ps.transport
		ps.mu.Unlock()

		if n > 0 {
			if werr := transport.writeOutput(buf[:n]); werr != nil {
				_ = transport.close()
				return
			}
		}
		if err != nil {
			_ = transport.close()
			return
		}
	}
}
