package deskconn

import (
	"bytes"
	"context"
	"fmt"
	"log"
	"math"
	"os"
	"os/exec"
	"os/signal"
	"sync"
	"syscall"

	"github.com/creack/pty"
	"golang.org/x/term"

	"github.com/xconnio/xconn-go"
)

type shellEncryption struct {
	sendKey    []byte
	receiveKey []byte
}

type interactiveShellSession struct {
	ptmx map[uint64]*os.File
	enc  map[uint64]*shellEncryption
	sync.Mutex
}

func newInteractiveShellSession() *interactiveShellSession {
	return &interactiveShellSession{
		ptmx: make(map[uint64]*os.File),
		enc:  make(map[uint64]*shellEncryption),
	}
}

func (p *interactiveShellSession) performKeyExchange(inv *xconn.Invocation, payload []byte) (*shellEncryption,
	*xconn.InvocationResult) {
	if !bytes.HasPrefix(payload, []byte("KEY:")) {
		return nil, xconn.NewInvocationError("wamp.error.invalid_argument", "expected key exchange")
	}
	clientPublicKey := payload[4:]
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
	p.Lock()
	p.enc[inv.Caller()] = enc
	p.Unlock()

	_ = inv.SendProgress([]any{append([]byte("KEY:"), serverPublicKey...)}, nil)
	return enc, nil
}

func (p *interactiveShellSession) cleanupCaller(caller uint64) {
	p.Lock()
	if stored, ok := p.ptmx[caller]; ok {
		_ = stored.Close()
		delete(p.ptmx, caller)
	}
	delete(p.enc, caller)
	p.Unlock()
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
	p.Lock()
	p.ptmx[inv.Caller()] = ptmx
	p.Unlock()

	go p.startOutputReader(inv, ptmx, sendKey)

	return ptmx, nil
}

func (p *interactiveShellSession) startOutputReader(inv *xconn.Invocation, ptmx *os.File, sendKey []byte) {
	caller := inv.Caller()
	defer func() {
		p.Lock()
		delete(p.ptmx, caller)
		p.Unlock()
		if err := ptmx.Close(); err != nil {
			log.Printf("Error closing PTY for caller %d: %v", caller, err)
		}
	}()
	buf := make([]byte, 4096)
	for {
		n, err := ptmx.Read(buf)
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
		enc, encExists := p.enc[caller]
		p.Unlock()

		if inv.Progress() {
			payload, err := inv.ArgBytes(0)
			if err != nil {
				return xconn.NewInvocationError("wamp.error.invalid_argument", err.Error())
			}

			if !encExists {
				var invErr *xconn.InvocationResult
				enc, invErr = p.performKeyExchange(inv, payload)
				if invErr != nil {
					return invErr
				}
				return xconn.NewInvocationError(xconn.ErrNoResult)
			}

			payload, err = DecryptPayload(payload, enc.receiveKey)
			if err != nil {
				return xconn.NewInvocationError("io.xconn.error", err.Error())
			}

			if bytes.HasPrefix(payload, []byte("SIZE:")) {
				var cols, rows int
				n, _ := fmt.Sscanf(string(payload), "SIZE:%d:%d", &cols, &rows)
				if n == 2 {
					if cols < 0 || cols > math.MaxUint16 || rows < 0 || rows > math.MaxUint16 {
						return xconn.NewInvocationError("wamp.error.invalid_argument", "invalid size")
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
		enc, encExists := p.enc[caller]
		p.Unlock()

		if inv.Progress() {
			payload, err := inv.ArgBytes(0)
			if err != nil {
				return xconn.NewInvocationError("wamp.error.invalid_argument", err.Error())
			}

			if !encExists {
				var invErr *xconn.InvocationResult
				enc, invErr = p.performKeyExchange(inv, payload)
				if invErr != nil {
					return invErr
				}
				return xconn.NewInvocationError(xconn.ErrNoResult)
			}

			payload, err = DecryptPayload(payload, enc.receiveKey)
			if err != nil {
				return xconn.NewInvocationError("io.xconn.error", err.Error())
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

	getSizeBytes := func() []byte {
		width, height, err := term.GetSize(fd)
		if err != nil {
			return nil
		}
		return []byte(fmt.Sprintf("SIZE:%d:%d", width, height))
	}

	// Feed SIZE and stdin into progressChan after key exchange completes.
	go func() {
		<-keyExchangeReady

		if sizeBytes := getSizeBytes(); sizeBytes != nil {
			if encrypted, err := EncryptPayload(sizeBytes, sendKey); err == nil {
				progressChan <- xconn.NewProgress(encrypted, args)
			}
		}

		go func() {
			sigChan := make(chan os.Signal, 1)
			signal.Notify(sigChan, syscall.SIGWINCH)
			for range sigChan {
				if sizeBytes := getSizeBytes(); sizeBytes != nil {
					if encrypted, err := EncryptPayload(sizeBytes, sendKey); err == nil {
						progressChan <- xconn.NewProgress(encrypted, args)
					}
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

	keySent := false
	callResp := session.Call(procedureName).
		ProgressSender(func(ctx context.Context) *xconn.Progress {
			if !keySent {
				keySent = true
				return xconn.NewProgress(append([]byte("KEY:"), publicKey...))
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
			if bytes.HasPrefix(data, []byte("KEY:")) {
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
			plaintext, err := DecryptPayload(data, receiveKey)
			if err == nil {
				_, _ = os.Stdout.Write(plaintext)
			}
		}).Do()

	return callResp.Err
}
