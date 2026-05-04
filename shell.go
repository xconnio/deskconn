package deskconn

import (
	"bytes"
	"context"
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

type shellEncryption struct {
	sendKey    []byte
	receiveKey []byte
}

type ptySession struct {
	mu      sync.Mutex
	inv     *xconn.Invocation
	sendKey []byte
}

type interactiveShellSession struct {
	ptmx     map[uint64]*os.File
	sessions map[uint64]*ptySession
	keys     *keyManager
	sync.Mutex
}

func newInteractiveShellSession() *interactiveShellSession {
	return &interactiveShellSession{
		ptmx:     make(map[uint64]*os.File),
		sessions: make(map[uint64]*ptySession),
		keys:     newKeyManager(),
	}
}

func (p *interactiveShellSession) setupEncryption(inv *xconn.Invocation, clientPublicKey []byte) (*shellEncryption,
	*xconn.InvocationResult) {
	serverPublicKey, serverPrivateKey, err := CreateX25519KeyPair()
	if err != nil {
		return nil, xconn.NewInvocationError("io.xconn.error", err.Error())
	}
	sharedSecret, err := PerformKeyExchange(serverPrivateKey, clientPublicKey)
	if err != nil {
		return nil, xconn.NewInvocationError("io.xconn.error", err.Error())
	}
	sendKey, err := DeriveKeyHKDF(sharedSecret, []byte("backendToFrontend"))
	if err != nil {
		return nil, xconn.NewInvocationError("io.xconn.error", err.Error())
	}
	receiveKey, err := DeriveKeyHKDF(sharedSecret, []byte("frontendToBackend"))
	if err != nil {
		return nil, xconn.NewInvocationError("io.xconn.error", err.Error())
	}
	enc := &shellEncryption{sendKey: sendKey, receiveKey: receiveKey}
	p.keys.store(inv.Caller(), enc)

	_ = inv.SendProgress([]any{append([]byte("KEY:"), serverPublicKey...)}, nil)
	return enc, nil
}

func (p *interactiveShellSession) cleanupCaller(caller uint64) {
	p.Lock()
	if stored, ok := p.ptmx[caller]; ok {
		_ = stored.Close()
		delete(p.ptmx, caller)
	}
	delete(p.sessions, caller)
	p.Unlock()
	p.keys.delete(caller)
}

func (p *interactiveShellSession) startPtySession(inv *xconn.Invocation, sendKey []byte,
	command string, args ...string) (*os.File, error) {
	cmd := exec.Command(command, args...)
	homeDir, err := os.UserHomeDir()
	if err != nil {
		return nil, fmt.Errorf("failed to get home dir: %w", err)
	}
	cmd.Dir = homeDir

	ptmx, err := pty.Start(cmd)
	if err != nil {
		return nil, fmt.Errorf("failed to start PTY: %w", err)
	}

	ps := &ptySession{inv: inv, sendKey: sendKey}
	p.Lock()
	p.ptmx[inv.Caller()] = ptmx
	p.sessions[inv.Caller()] = ps
	p.Unlock()

	go p.startOutputReader(ptmx, ps)

	return ptmx, nil
}

func (p *interactiveShellSession) startOutputReader(ptmx *os.File, ps *ptySession) {
	defer func() {
		p.Lock()
		for id, stored := range p.ptmx {
			if stored == ptmx {
				delete(p.ptmx, id)
				delete(p.sessions, id)
				break
			}
		}
		p.Unlock()
		if err := ptmx.Close(); err != nil {
			log.Printf("Error closing PTY: %v", err)
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

func (p *interactiveShellSession) handleShell() func(_ context.Context,
	inv *xconn.Invocation) *xconn.InvocationResult {
	return func(_ context.Context, inv *xconn.Invocation) *xconn.InvocationResult {
		caller := inv.Caller()

		p.Lock()
		ptmx, exists := p.ptmx[caller]
		p.Unlock()
		enc, encExists := p.keys.fetch(caller)

		if inv.Progress() {
			payload, err := inv.ArgBytes(0)
			if err != nil {
				return xconn.NewInvocationError("wamp.error.invalid_argument", err.Error())
			}

			if !encExists {
				keyMarker := []byte(":KEY:")
				keyIdx := bytes.Index(payload, keyMarker)
				if keyIdx < 0 {
					return xconn.NewInvocationError("wamp.error.invalid_argument", "missing encryption key")
				}
				var invErr *xconn.InvocationResult
				enc, invErr = p.setupEncryption(inv, payload[keyIdx+len(keyMarker):])
				if invErr != nil {
					return invErr
				}
				payload = payload[:keyIdx]
				// Fall through to process the SIZE payload.
			} else {
				var decErr error
				payload, decErr = DecryptPayload(payload, enc.receiveKey)
				if decErr != nil {
					return xconn.NewInvocationError("io.xconn.error", decErr.Error())
				}
			}

			if bytes.HasPrefix(payload, []byte("SIZE:")) {
				var cols, rows int
				n, _ := fmt.Sscanf(string(payload), "SIZE:%d:%d", &cols, &rows)
				if n == 2 {
					if cols < 0 || cols > math.MaxUint16 || rows < 0 || rows > math.MaxUint16 {
						return xconn.NewInvocationError("wamp.error.invalid_argument", "invalid size")
					}
					if !exists {
						if oldSessionID, err := inv.KwargUInt64("session-id"); err == nil {
							p.Lock()
							oldPtmx, ptmxOk := p.ptmx[oldSessionID]
							oldPS, psOk := p.sessions[oldSessionID]
							if ptmxOk && psOk {
								oldPS.mu.Lock()
								oldInv := oldPS.inv
								oldPS.inv = inv
								oldPS.sendKey = enc.sendKey
								oldPS.mu.Unlock()
								delete(p.ptmx, oldSessionID)
								delete(p.sessions, oldSessionID)
								p.ptmx[caller] = oldPtmx
								p.sessions[caller] = oldPS
								ptmx = oldPtmx
								exists = true
								_ = oldInv.SendProgress(nil, nil)
							}
							p.Unlock()
							p.keys.delete(oldSessionID)
						}
					}
					if !exists {
						newPt, err := p.startPtySession(inv, enc.sendKey, "bash")
						if err != nil {
							return xconn.NewInvocationError("io.xconn.error", err.Error())
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
				newPt, err := p.startPtySession(inv, enc.sendKey, "bash")
				if err != nil {
					return xconn.NewInvocationError("io.xconn.error", err.Error())
				}
				ptmx = newPt
			}

			_, err = ptmx.Write(payload)
			if err != nil {
				return xconn.NewInvocationError("io.xconn.error", err.Error())
			}
			return xconn.NewInvocationError(xconn.ErrNoResult)
		}

		p.cleanupCaller(caller)
		return xconn.NewInvocationResult()
	}
}

func (p *interactiveShellSession) handleExec() func(_ context.Context,
	inv *xconn.Invocation) *xconn.InvocationResult {
	return func(_ context.Context, inv *xconn.Invocation) *xconn.InvocationResult {
		caller := inv.Caller()

		p.Lock()
		ptmx, exists := p.ptmx[caller]
		p.Unlock()
		enc, encExists := p.keys.fetch(caller)

		if inv.Progress() {
			payload, err := inv.ArgBytes(0)
			if err != nil {
				return xconn.NewInvocationError("wamp.error.invalid_argument", err.Error())
			}

			if !encExists {
				keyMarker := []byte(":KEY:")
				keyIdx := bytes.Index(payload, keyMarker)
				if keyIdx < 0 {
					return xconn.NewInvocationError("wamp.error.invalid_argument", "missing encryption key")
				}
				var invErr *xconn.InvocationResult
				enc, invErr = p.setupEncryption(inv, payload[keyIdx+len(keyMarker):])
				if invErr != nil {
					return invErr
				}
				payload = payload[:keyIdx]
				// Fall through to process the SIZE payload.
			} else {
				var decErr error
				payload, decErr = DecryptPayload(payload, enc.receiveKey)
				if decErr != nil {
					return xconn.NewInvocationError("io.xconn.error", decErr.Error())
				}
			}

			if bytes.HasPrefix(payload, []byte("SIZE:")) {
				commandWithArgs, err := inv.ArgList(1)
				if err != nil {
					return xconn.NewInvocationError("wamp.error.invalid_argument", err.Error())
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
						return xconn.NewInvocationError("wamp.error.invalid_argument", "invalid size")
					}
					if !exists {
						newPt, err := p.startPtySession(inv, enc.sendKey, command, args...)
						if err != nil {
							return xconn.NewInvocationError("io.xconn.error", err.Error())
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
				return xconn.NewInvocationError("io.xconn.error", err.Error())
			}
			return xconn.NewInvocationError(xconn.ErrNoResult)
		}

		p.cleanupCaller(caller)
		return xconn.NewInvocationResult()
	}
}

func StartInteractiveCommand(session *xconn.Session, procedureName string, args ...string) error {
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
	keyExchangeReady := make(chan struct{})
	progressChan := make(chan *xconn.Progress, 32)

	buildSizeProgress := func(firstMsg bool) *xconn.Progress {
		width, height, err := term.GetSize(fd)
		if err != nil {
			return nil
		}
		sizeStr := fmt.Sprintf("SIZE:%d:%d", width, height)
		if firstMsg {
			return xconn.NewProgress(append([]byte(sizeStr+":KEY:"), publicKey...), args)
		}
		encrypted, err := EncryptPayload([]byte(sizeStr), sendKey)
		if err != nil {
			return nil
		}
		return xconn.NewProgress(encrypted, args)
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
				progressChan <- xconn.NewProgress(encrypted)
			}
		}
	}()

	firstSent := false
	firstServerMsg := true
	callResp := session.Call(procedureName).
		ProgressSender(func(ctx context.Context) *xconn.Progress {
			if !firstSent {
				firstSent = true
				// First message: SIZE with embedded public key for key negotiation.
				return buildSizeProgress(true)
			}
			p, ok := <-progressChan
			if !ok {
				return xconn.NewFinalProgress()
			}
			return p
		}).
		ProgressReceiver(func(result *xconn.ProgressResult) {
			if len(result.Args()) == 0 {
				progressChan <- xconn.NewFinalProgress()
				return
			}
			data := result.Args()[0].([]byte)

			if firstServerMsg {
				firstServerMsg = false
				if !bytes.HasPrefix(data, []byte("KEY:")) {
					progressChan <- xconn.NewFinalProgress()
					return
				}
				serverPublicKey := data[4:]
				sharedSecret, err := PerformKeyExchange(privateKey, serverPublicKey)
				if err != nil {
					close(progressChan)
					return
				}
				sendKey, err = DeriveKeyHKDF(sharedSecret, []byte("frontendToBackend"))
				if err != nil {
					close(progressChan)
					return
				}
				receiveKey, err = DeriveKeyHKDF(sharedSecret, []byte("backendToFrontend"))
				if err != nil {
					close(progressChan)
					return
				}
				close(keyExchangeReady)
				return
			}

			if plaintext, err := DecryptPayload(data, receiveKey); err == nil {
				_, _ = os.Stdout.Write(plaintext)
			}
		}).Do()

	return callResp.Err
}

// StartInteractiveCommandWithMigration starts a shell on normalSession, then transparently
// upgrades to a WebRTC session when one becomes available, migrating the PTY to the new session.
func StartInteractiveCommandWithMigration(parentCtx context.Context, normalSession *xconn.Session,
	realm, cfgDirectory, procedureName string, args ...string) error {

	fd := int(os.Stdin.Fd()) // #nosec
	oldState, err := term.MakeRaw(fd)
	if err != nil {
		return fmt.Errorf("failed to set raw mode: %w", err)
	}
	defer func() { _ = term.Restore(fd, oldState) }()

	ctx, cancel := context.WithCancel(parentCtx)
	defer cancel()

	type routerState struct {
		mu      sync.Mutex
		sendKey []byte
		progCh  chan *xconn.Progress
	}
	router := &routerState{progCh: make(chan *xconn.Progress, 32)}

	getSendKey := func() []byte {
		router.mu.Lock()
		defer router.mu.Unlock()
		return router.sendKey
	}
	setSendKey := func(key []byte) {
		router.mu.Lock()
		router.sendKey = key
		router.mu.Unlock()
	}
	sendToActive := func(p *xconn.Progress) {
		router.mu.Lock()
		ch := router.progCh
		router.mu.Unlock()
		select {
		case ch <- p:
		case <-ctx.Done():
		}
	}
	switchChannel := func(newCh chan *xconn.Progress) {
		router.mu.Lock()
		router.progCh = newCh
		router.mu.Unlock()
	}

	keyExchangeReady := make(chan struct{})
	var keyExchangeOnce sync.Once

	go func() {
		select {
		case <-keyExchangeReady:
		case <-ctx.Done():
			return
		}

		go func() {
			sigChan := make(chan os.Signal, 1)
			signal.Notify(sigChan, syscall.SIGWINCH)
			for range sigChan {
				key := getSendKey()
				if key == nil {
					continue
				}
				width, height, sErr := term.GetSize(fd)
				if sErr != nil {
					continue
				}
				sizeStr := fmt.Sprintf("SIZE:%d:%d", width, height)
				if enc, eErr := EncryptPayload([]byte(sizeStr), key); eErr == nil {
					sendToActive(xconn.NewProgress(enc, args))
				}
			}
		}()

		buf := make([]byte, 1024)
		for {
			n, readErr := os.Stdin.Read(buf)
			if readErr != nil {
				sendToActive(xconn.NewFinalProgress())
				return
			}
			key := getSendKey()
			if key == nil {
				continue
			}
			if enc, eErr := EncryptPayload(buf[:n], key); eErr == nil {
				sendToActive(xconn.NewProgress(enc))
			}
		}
	}()

	migrated := make(chan struct{})
	var migrateOnce sync.Once
	signalMigrated := func() { migrateOnce.Do(func() { close(migrated) }) }
	isMigrated := func() bool {
		select {
		case <-migrated:
			return true
		default:
			return false
		}
	}

	resultCh := make(chan error, 2)

	doCall := func(session *xconn.Session, progCh chan *xconn.Progress, oldSessionID uint64) error {
		pubKey, privKey, kErr := CreateX25519KeyPair()
		if kErr != nil {
			return fmt.Errorf("failed to generate keypair: %w", kErr)
		}

		var recvKey []byte
		firstSent := false
		firstServerMsg := true

		sendFinal := func() {
			select {
			case progCh <- xconn.NewFinalProgress():
			default:
			}
		}

		resp := session.Call(procedureName).
			ProgressSender(func(_ context.Context) *xconn.Progress {
				if !firstSent {
					firstSent = true
					width, height, _ := term.GetSize(fd)
					sizeStr := fmt.Sprintf("SIZE:%d:%d", width, height)
					p := xconn.NewProgress(append([]byte(sizeStr+":KEY:"), pubKey...), args)
					if oldSessionID != 0 {
						p.Kwargs = map[string]any{"session-id": oldSessionID}
					}
					return p
				}
				return <-progCh
			}).
			ProgressReceiver(func(result *xconn.ProgressResult) {
				if len(result.Args()) == 0 {
					sendFinal()
					return
				}
				data, err := result.ArgBytes(0)
				if err != nil {
					sendFinal()
					return
				}

				if firstServerMsg {
					firstServerMsg = false
					if !bytes.HasPrefix(data, []byte("KEY:")) {
						sendFinal()
						return
					}
					serverPub := data[4:]
					sharedSecret, sErr := PerformKeyExchange(privKey, serverPub)
					if sErr != nil {
						sendFinal()
						return
					}
					sndKey, sErr := DeriveKeyHKDF(sharedSecret, []byte("frontendToBackend"))
					if sErr != nil {
						sendFinal()
						return
					}
					recvKey, sErr = DeriveKeyHKDF(sharedSecret, []byte("backendToFrontend"))
					if sErr != nil {
						sendFinal()
						return
					}
					setSendKey(sndKey)
					keyExchangeOnce.Do(func() { close(keyExchangeReady) })
					return
				}

				if plaintext, dErr := DecryptPayload(data, recvKey); dErr == nil {
					_, _ = os.Stdout.Write(plaintext)
				}
			}).Do()

		return resp.Err
	}

	go func() {
		err := doCall(normalSession, router.progCh, 0)
		if isMigrated() {
			_ = normalSession.Leave()
			return
		}
		cancel()
		resultCh <- err
	}()

	go func() {
		select {
		case <-keyExchangeReady:
		case <-ctx.Done():
			return
		}
		webrtcSession, connErr := ConnectDeviceRealm(ctx, realm, cfgDirectory, true)
		if connErr != nil {
			log.Debugf("webrtc migration: %v", connErr)
			return
		}
		newCh := make(chan *xconn.Progress, 32)
		switchChannel(newCh)
		signalMigrated()

		err := doCall(webrtcSession, newCh, normalSession.ID())
		resultCh <- err
	}()

	return <-resultCh
}
