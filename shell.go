package deskconn

import (
	"bytes"
	"context"
	"crypto/subtle"
	"fmt"
	"math"
	"os"
	"os/exec"
	"os/signal"
	"sync"
	"syscall"

	"github.com/creack/pty"
	log "github.com/sirupsen/logrus"
	"golang.org/x/term"

	"github.com/xconnio/xconn-go"
)

type encryptionKeys struct {
	sendKey    []byte
	receiveKey []byte
}

type ptySession struct {
	mu      sync.Mutex
	inv     *xconn.Invocation
	sendKey []byte
	authID  string
}

type interactiveShellSession struct {
	ptmx            map[string]*os.File
	sessions        map[string]*ptySession
	migrationTokens map[string]string
	encKeys         map[string]*encryptionKeys
	invShellIDs     map[*xconn.Invocation]string // fast-path: inv pointer → shell ID
	pids            map[string]int               // shell ID → PTY child PID, for /proc cwd lookups
	sync.Mutex
}

func newInteractiveShellSession() *interactiveShellSession {
	return &interactiveShellSession{
		ptmx:            make(map[string]*os.File),
		sessions:        make(map[string]*ptySession),
		migrationTokens: make(map[string]string),
		encKeys:         make(map[string]*encryptionKeys),
		invShellIDs:     make(map[*xconn.Invocation]string),
		pids:            make(map[string]int),
	}
}

// generateShellID must be called with p.Lock() held.
// First shell for a caller gets "<callerID>", subsequent ones get "<callerID>1", "<callerID>2".
func (p *interactiveShellSession) generateShellID(caller uint64) string {
	base := fmt.Sprintf("%d", caller)
	if _, exists := p.ptmx[base]; !exists {
		return base
	}
	for i := 1; ; i++ {
		id := fmt.Sprintf("%d%d", caller, i)
		if _, exists := p.ptmx[id]; !exists {
			return id
		}
	}
}

// shellIDForInv returns the shell ID bound to this invocation via the inv pointer
// map, or falls back to the legacy "<callerID>" key for single-shell clients.
func (p *interactiveShellSession) shellIDForInv(inv *xconn.Invocation) string {
	p.Lock()
	id, ok := p.invShellIDs[inv]
	p.Unlock()
	if ok {
		return id
	}
	return fmt.Sprintf("%d", inv.Caller())
}

// setupEncryption performs the X25519 key exchange and sends the KEY response.
// When embedShellID is true the assigned shell ID is appended after the 32-byte public key.
// embedShellID is false for the first shell of a caller (backwards-compat: old clients read data[4:]
// as the raw key and would break if extra bytes were added).
func (p *interactiveShellSession) setupEncryption(inv *xconn.Invocation, clientPublicKey []byte,
	shellID string, embedShellID bool) (*encryptionKeys, *xconn.InvocationResult) {
	serverPublicKey, sendKey, receiveKey, err := ServerKeyExchange(clientPublicKey)
	if err != nil {
		return nil, xconn.NewInvocationError(ErrOperationFailed, err.Error())
	}
	enc := &encryptionKeys{sendKey: sendKey, receiveKey: receiveKey}
	p.Lock()
	p.encKeys[shellID] = enc
	p.Unlock()

	keyData := append([]byte("KEY:"), serverPublicKey...)
	if embedShellID {
		keyData = append(keyData, []byte(shellID)...)
	}
	_ = inv.SendProgress([]any{keyData}, nil)
	return enc, nil
}

func (p *interactiveShellSession) cleanupShell(shellID string, inv *xconn.Invocation) {
	p.Lock()
	if stored, ok := p.ptmx[shellID]; ok {
		_ = stored.Close()
		delete(p.ptmx, shellID)
	}
	delete(p.sessions, shellID)
	delete(p.migrationTokens, shellID)
	delete(p.encKeys, shellID)
	delete(p.invShellIDs, inv)
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

// resolveStartDir picks the start dir: "prev-shell" kwarg's live cwd, or home.
func (p *interactiveShellSession) resolveStartDir(inv *xconn.Invocation) (string, error) {
	if prevShellID := inv.KwargStringOr("prev-shell", ""); prevShellID != "" {
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

func (p *interactiveShellSession) startPtySession(inv *xconn.Invocation, sendKey []byte,
	shellID string, command string, args ...string) (*os.File, error) {
	cmd := exec.Command(command, args...)

	dir, err := p.resolveStartDir(inv)
	if err != nil {
		return nil, err
	}
	cmd.Dir = dir

	ptmx, err := pty.Start(cmd)
	if err != nil {
		return nil, fmt.Errorf("failed to start PTY: %w", err)
	}

	ps := &ptySession{inv: inv, sendKey: sendKey, authID: inv.CallerAuthID()}
	p.Lock()
	p.ptmx[shellID] = ptmx
	p.sessions[shellID] = ps
	p.invShellIDs[inv] = shellID
	p.pids[shellID] = cmd.Process.Pid
	p.Unlock()

	go p.startOutputReader(ptmx, ps, shellID)

	return ptmx, nil
}

func (p *interactiveShellSession) startOutputReader(ptmx *os.File, ps *ptySession, shellID string) {
	defer func() {
		p.Lock()
		shouldClose := p.ptmx[shellID] == ptmx
		delete(p.ptmx, shellID)
		delete(p.sessions, shellID)
		delete(p.encKeys, shellID)
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
		inv := ps.inv
		sendKey := ps.sendKey
		ps.mu.Unlock()

		if n > 0 {
			encrypted, encErr := EncryptPayload(buf[:n], sendKey)
			if encErr != nil {
				_ = inv.SendProgress(nil, nil)
				return
			}
			_ = inv.SendProgress([]any{encrypted}, nil)
		}
		if err != nil {
			_ = inv.SendProgress(nil, nil)
			return
		}
	}
}

func (p *interactiveShellSession) decryptProgress(payload []byte, shellID string, enc *encryptionKeys) (
	plaintext []byte, resolvedShellID string, resolvedEnc *encryptionKeys, invErr *xconn.InvocationResult) {

	// Old clients: decrypt directly with the enc already resolved for this inv.
	if enc != nil {
		if pt, err := DecryptPayload(payload, enc.receiveKey); err == nil {
			return pt, shellID, enc, nil
		}
	}

	// New clients (non-first shells): strip the "<shellID>:" prefix and look up the enc.
	if colonIdx := bytes.IndexByte(payload, ':'); colonIdx > 0 {
		prefixID := string(payload[:colonIdx])
		p.Lock()
		prefixEnc := p.encKeys[prefixID]
		p.Unlock()
		if prefixEnc != nil {
			stripped := payload[colonIdx+1:]
			if pt, err := DecryptPayload(stripped, prefixEnc.receiveKey); err == nil {
				return pt, prefixID, prefixEnc, nil
			}
		}
	}

	// No enc at all means no key exchange was ever done.
	if enc == nil {
		return nil, "", nil, xconn.NewInvocationError(ErrInvalidArgument, "missing encryption key")
	}
	return nil, "", nil, xconn.NewInvocationError(ErrOperationFailed, "failed to decrypt")
}

func (p *interactiveShellSession) handleShell() func(_ context.Context,
	inv *xconn.Invocation) *xconn.InvocationResult {
	return func(_ context.Context, inv *xconn.Invocation) *xconn.InvocationResult {
		shellID := p.shellIDForInv(inv)

		p.Lock()
		enc := p.encKeys[shellID]
		p.Unlock()

		var exists bool
		var ptmx *os.File
		if inv.Progress() {
			payload, err := inv.ArgBytes(0)
			if err != nil {
				return xconn.NewInvocationError(ErrInvalidArgument, err.Error())
			}

			keyMarker := []byte(":KEY:")
			keyIdx := bytes.Index(payload, keyMarker)
			if keyIdx >= 0 {
				caller := inv.Caller()
				// First message for a new shell: generate a unique ID.
				p.Lock()
				shellID = p.generateShellID(caller)
				p.invShellIDs[inv] = shellID
				p.Unlock()

				// Embed the shell ID in the KEY response only for non-first shells so
				// the client can use it as a prefix. The first shell uses the old KEY format
				// for backwards compatibility with old clients.
				callerStr := fmt.Sprintf("%d", caller)
				var invErr *xconn.InvocationResult
				enc, invErr = p.setupEncryption(inv, payload[keyIdx+len(keyMarker):], shellID, shellID != callerStr)
				if invErr != nil {
					return invErr
				}
				payload = payload[:keyIdx]
				exists = false
			} else {
				var invErr *xconn.InvocationResult
				payload, shellID, enc, invErr = p.decryptProgress(payload, shellID, enc)
				if invErr != nil {
					return invErr
				}
				p.Lock()
				ptmx, exists = p.ptmx[shellID]
				p.Unlock()
			}

			if bytes.HasPrefix(payload, []byte("SIZE:")) {
				var cols, rows int
				n, _ := fmt.Sscanf(string(payload), "SIZE:%d:%d", &cols, &rows)
				if n == 2 {
					if cols < 0 || cols > math.MaxUint16 || rows < 0 || rows > math.MaxUint16 {
						return xconn.NewInvocationError(ErrInvalidArgument, "invalid size")
					}
					if !exists {
						if oldSessionID, err := inv.KwargUInt64("session-id"); err == nil {
							migrateToken, tokenErr := inv.KwargString("migrate-token")
							oldShellID := fmt.Sprintf("%d", oldSessionID)
							p.Lock()
							oldPtmx, ptmxOk := p.ptmx[oldShellID]
							oldPS, psOk := p.sessions[oldShellID]
							expectedToken, tokenOk := p.migrationTokens[oldShellID]
							if ptmxOk && psOk {
								validToken := tokenOk && subtle.ConstantTimeCompare(
									[]byte(migrateToken), []byte(expectedToken)) == 1
								sameCaller := oldPS.authID != "" && oldPS.authID == inv.CallerAuthID()
								if tokenErr != nil || !validToken || !sameCaller {
									p.Unlock()
									return xconn.NewInvocationError(ErrNotAuthorized, "invalid migration token")
								}
								delete(p.migrationTokens, oldShellID)
								oldPS.mu.Lock()
								oldInv := oldPS.inv
								oldPS.inv = inv
								oldPS.sendKey = enc.sendKey
								oldPS.mu.Unlock()
								delete(p.ptmx, oldShellID)
								delete(p.sessions, oldShellID)
								delete(p.encKeys, oldShellID)
								for oldCallInv, id := range p.invShellIDs {
									if id == oldShellID {
										delete(p.invShellIDs, oldCallInv)
										break
									}
								}
								p.ptmx[shellID] = oldPtmx
								p.sessions[shellID] = oldPS
								p.invShellIDs[inv] = shellID
								ptmx = oldPtmx
								exists = true
								_ = oldInv.SendProgress(nil, nil)
							}
							p.Unlock()
						}
					}
					if !exists {
						if token, err := inv.KwargString("migrate-token"); err == nil && token != "" {
							p.Lock()
							p.migrationTokens[shellID] = token
							p.Unlock()
						}
						newPt, err := p.startPtySession(inv, enc.sendKey, shellID, "bash")
						if err != nil {
							return xconn.NewInvocationError(ErrOperationFailed, err.Error())
						}
						ptmx = newPt
					}
					winsize := &pty.Winsize{
						Cols: uint16(cols), // #nosec G115
						Rows: uint16(rows), // #nosec G115
					}
					_ = pty.Setsize(ptmx, winsize)
				}
				return xconn.NewInvocationError(xconn.ErrNoResult)
			}

			if !exists {
				newPt, err := p.startPtySession(inv, enc.sendKey, shellID, "bash")
				if err != nil {
					return xconn.NewInvocationError(ErrOperationFailed, err.Error())
				}
				ptmx = newPt
			}

			_, err = ptmx.Write(payload)
			if err != nil {
				return xconn.NewInvocationError(ErrOperationFailed, err.Error())
			}
			return xconn.NewInvocationError(xconn.ErrNoResult)
		}

		if id, err := inv.ArgString(0); err == nil && id != "" {
			shellID = id
		}
		p.cleanupShell(shellID, inv)
		return xconn.NewInvocationResult()
	}
}

func (p *interactiveShellSession) handleExec() func(_ context.Context,
	inv *xconn.Invocation) *xconn.InvocationResult {
	return func(_ context.Context, inv *xconn.Invocation) *xconn.InvocationResult {
		caller := inv.Caller()
		shellID := p.shellIDForInv(inv)

		p.Lock()
		enc := p.encKeys[shellID]
		p.Unlock()

		var ptmx *os.File
		var exists bool
		if inv.Progress() {
			payload, err := inv.ArgBytes(0)
			if err != nil {
				return xconn.NewInvocationError(ErrInvalidArgument, err.Error())
			}

			keyMarker := []byte(":KEY:")
			keyIdx := bytes.Index(payload, keyMarker)
			if keyIdx >= 0 {
				p.Lock()
				shellID = p.generateShellID(caller)
				p.invShellIDs[inv] = shellID
				p.Unlock()
				callerStr := fmt.Sprintf("%d", caller)
				var invErr *xconn.InvocationResult
				enc, invErr = p.setupEncryption(inv, payload[keyIdx+len(keyMarker):], shellID, shellID != callerStr)
				if invErr != nil {
					return invErr
				}
				payload = payload[:keyIdx]
				exists = false
			} else {
				var invErr *xconn.InvocationResult
				payload, shellID, enc, invErr = p.decryptProgress(payload, shellID, enc)
				if invErr != nil {
					return invErr
				}
				p.Lock()
				ptmx, exists = p.ptmx[shellID]
				p.Unlock()
			}

			if bytes.HasPrefix(payload, []byte("SIZE:")) {
				commandWithArgs, err := inv.ArgList(1)
				if err != nil {
					return xconn.NewInvocationError(ErrInvalidArgument, err.Error())
				}
				command, _ := commandWithArgs.String(0)
				var args []string
				for _, arg := range commandWithArgs[1:] {
					args = append(args, arg.StringOr(""))
				}

				var cols, rows int
				n, _ := fmt.Sscanf(string(payload), "SIZE:%d:%d", &cols, &rows)
				if n == 2 {
					if cols < 0 || cols > math.MaxUint16 || rows < 0 || rows > math.MaxUint16 {
						return xconn.NewInvocationError(ErrInvalidArgument, "invalid size")
					}
					if !exists {
						newPt, err := p.startPtySession(inv, enc.sendKey, shellID, command, args...)
						if err != nil {
							return xconn.NewInvocationError(ErrOperationFailed, err.Error())
						}
						ptmx = newPt
					}
					winsize := &pty.Winsize{
						Cols: uint16(cols), // #nosec G115
						Rows: uint16(rows), // #nosec G115
					}
					_ = pty.Setsize(ptmx, winsize)
				}
				return xconn.NewInvocationError(xconn.ErrNoResult)
			}

			_, err = ptmx.Write(payload)
			if err != nil {
				return xconn.NewInvocationError(ErrOperationFailed, err.Error())
			}
			return xconn.NewInvocationError(xconn.ErrNoResult)
		}

		if id, err := inv.ArgString(0); err == nil && id != "" {
			shellID = id
		}
		p.cleanupShell(shellID, inv)
		return xconn.NewInvocationResult()
	}
}

func StartInteractiveCommand(session *xconn.Session, realm, procedureName string, args ...string) error {
	fd := int(os.Stdin.Fd()) // #nosec
	oldState, err := term.MakeRaw(fd)
	if err != nil {
		return fmt.Errorf("failed to set raw mode: %w", err)
	}
	defer func() { _ = term.Restore(fd, oldState) }()

	publicKey, privateKey, err := CreateX25519KeyPair()
	if err != nil {
		return fmt.Errorf("failed to generate keypair: %w", err)
	}

	var sendKey, receiveKey []byte
	// shellID is set when the server embeds it in the KEY response (non-first shells).
	// Once set, every outgoing message is prefixed with "<shellID>:" so the server can route it.
	var shellID string
	keyExchangeReady := make(chan struct{})
	var keyExchangeOnce sync.Once
	progressChan := make(chan *xconn.Progress, 32)

	// withShellPrefix prepends "<shellID>:" to payload when the server has assigned
	// a shell ID for this call (i.e. this is a non-first shell on the same session).
	withShellPrefix := func(payload []byte) []byte {
		if shellID == "" {
			return payload
		}
		return append([]byte(shellID+":"), payload...)
	}

	buildSizeProgress := func(firstMsg bool) *xconn.Progress {
		width, height, err := term.GetSize(fd)
		if err != nil {
			return nil
		}
		sizeStr := fmt.Sprintf("SIZE:%d:%d", width, height)
		if firstMsg {
			if realm == "" {
				return xconn.NewProgress(append([]byte(sizeStr+":KEY:"), publicKey...), args)
			}
			return xconn.NewProgress(realm, append([]byte(sizeStr+":KEY:"), publicKey...), args)
		}
		encrypted, err := EncryptPayload([]byte(sizeStr), sendKey)
		if err != nil {
			return nil
		}
		payload := withShellPrefix(encrypted)
		if realm == "" {
			return xconn.NewProgress(payload, args)
		}
		return xconn.NewProgress(realm, payload, args)
	}

	go func() {
		<-keyExchangeReady

		go func() {
			sigChan := make(chan os.Signal, 1)
			signal.Notify(sigChan, syscall.SIGWINCH)
			for range sigChan {
				if p := buildSizeProgress(false); p != nil {
					progressChan <- p
				}
			}
		}()

		buf := make([]byte, 1024)
		for {
			n, err := os.Stdin.Read(buf)
			if err != nil {
				close(progressChan)
				return
			}
			if encrypted, err := EncryptPayload(buf[:n], sendKey); err == nil {
				payload := withShellPrefix(encrypted)
				if realm == "" {
					progressChan <- xconn.NewProgress(payload)
					continue
				}
				progressChan <- xconn.NewProgress(realm, payload)
			}
		}
	}()

	firstSent := false
	callResp := session.Call(procedureName).
		ProgressSender(func(ctx context.Context) *xconn.Progress {
			if !firstSent {
				firstSent = true
				// First message: SIZE with embedded public key for key negotiation.
				return buildSizeProgress(true)
			}
			p, ok := <-progressChan
			if !ok {
				if shellID != "" {
					return xconn.NewFinalProgress(shellID)
				}
				return xconn.NewFinalProgress()
			}
			return p
		}).
		ProgressReceiver(func(result *xconn.ProgressResult) {
			if len(result.Args()) == 0 {
				if shellID != "" {
					progressChan <- xconn.NewFinalProgress(shellID)
				} else {
					progressChan <- xconn.NewFinalProgress()
				}
				return
			}
			data, err := result.ArgBytes(0)
			if err != nil {
				return
			}

			if bytes.HasPrefix(data, []byte("KEY:")) {
				// Public key is always 32 bytes. Anything beyond that is the shell ID
				// the server assigned for this call (only present for non-first shells).
				rest := data[4:]
				var serverPublicKey []byte
				if len(rest) > 32 {
					serverPublicKey = rest[:32]
					shellID = string(rest[32:])
				} else {
					serverPublicKey = rest
				}
				var err error
				sendKey, receiveKey, err = ClientKeyExchangeKeys(privateKey, serverPublicKey)
				if err != nil {
					close(progressChan)
					return
				}
				keyExchangeOnce.Do(func() { close(keyExchangeReady) })
				return
			}

			if plaintext, err := DecryptPayload(data, receiveKey); err == nil {
				_, _ = os.Stdout.Write(plaintext)
			}
		}).Do()

	return callResp.Err
}
