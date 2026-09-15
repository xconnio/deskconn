package deskconn

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"os"
	"os/exec"
	"sync"
	"syscall"
	"time"

	"github.com/creack/pty"
	log "github.com/sirupsen/logrus"
	"golang.org/x/sys/unix"

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
}

type interactiveShellSession struct {
	ptmx            map[string]*os.File
	sessions        map[string]*ptySession
	migrationTokens map[string]migrationToken
	pids            map[string]int        // shell ID → PTY child PID, for /proc cwd lookups
	agentForward    *agentForwardSessions // set by NewDeskconn; may be nil, check before use
	sync.Mutex
}

func newInteractiveShellSession() *interactiveShellSession {
	return &interactiveShellSession{
		ptmx:            make(map[string]*os.File),
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
	if stored, ok := p.ptmx[shellID]; ok {
		_ = stored.Close()
		delete(p.ptmx, shellID)
	}
	delete(p.sessions, shellID)
	delete(p.migrationTokens, shellID)
	delete(p.pids, shellID)
	p.Unlock()
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

	// SyscallConn (not Fd) so the pty stays non-blocking for the output reader
	// goroutine that's continuously reading it — Fd() would flip that
	// permanently to blocking mode for the rest of this file's lifetime.
	rawConn, err := ptmx.SyscallConn()
	if err != nil {
		return false, fmt.Errorf("failed to access pty: %w", err)
	}

	var fgpgid int
	var ioctlErr error
	if err := rawConn.Control(func(fd uintptr) {
		fgpgid, ioctlErr = unix.IoctlGetInt(int(fd), unix.TIOCGPGRP)
	}); err != nil {
		return false, fmt.Errorf("failed to access pty fd: %w", err)
	}
	if ioctlErr != nil {
		return false, fmt.Errorf("failed to get foreground pgid: %w", ioctlErr)
	}

	shellPgid, err := syscall.Getpgid(pid)
	if err != nil {
		return false, fmt.Errorf("failed to get shell pgid: %w", err)
	}

	return fgpgid != shellPgid, nil
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

// agentSockPath, when non-empty, is exported as SSH_AUTH_SOCK in the spawned process's
// environment so tools run in the shell (git, ssh, ...) can use the caller's forwarded
// local SSH agent — see RunAgentForward/handleAgentForward in agentforward.go.
//
// ws sets the PTY's initial size via pty.StartWithSize rather than a separate
// pty.Setsize call after: Setsize racing the output-reader goroutine's first
// Read is a genuine data race (both touch the os.File's internal fd state).
func (p *interactiveShellSession) startPtySession(transport shellTransport, shellID, agentSockPath,
	prevShellID, command string, ws *pty.Winsize, args ...string) (*os.File, error) {
	cmd := exec.Command(command, args...)
	if agentSockPath != "" {
		cmd.Env = append(os.Environ(), "SSH_AUTH_SOCK="+agentSockPath)
	}

	dir, err := p.resolveStartDir(prevShellID)
	if err != nil {
		return nil, err
	}
	cmd.Dir = dir

	ptmx, err := pty.StartWithSize(cmd, ws)
	if err != nil {
		return nil, fmt.Errorf("failed to start PTY: %w", err)
	}

	ps := &ptySession{transport: transport}
	p.Lock()
	p.ptmx[shellID] = ptmx
	p.sessions[shellID] = ps
	p.pids[shellID] = cmd.Process.Pid
	p.Unlock()

	SafeGo(func() { p.startOutputReader(ptmx, ps, shellID) })

	return ptmx, nil
}

func (p *interactiveShellSession) startOutputReader(ptmx *os.File, ps *ptySession, shellID string) {
	defer func() {
		p.Lock()
		shouldClose := p.ptmx[shellID] == ptmx
		delete(p.ptmx, shellID)
		delete(p.sessions, shellID)
		delete(p.pids, shellID)
		p.Unlock()
		if shouldClose {
			if err := ptmx.Close(); err != nil {
				log.Printf("Error closing PTY: %v", err)
			}
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
