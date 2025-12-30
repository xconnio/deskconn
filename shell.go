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

type interactiveShellSession struct {
	ptmx map[uint64]*os.File
	sync.Mutex
}

func newInteractiveShellSession() *interactiveShellSession {
	return &interactiveShellSession{
		ptmx: make(map[uint64]*os.File),
	}
}

func (p *interactiveShellSession) startPtySession(inv *xconn.Invocation) (*os.File, error) {
	cmd := exec.Command("bash")
	ptmx, err := pty.Start(cmd)
	if err != nil {
		return nil, fmt.Errorf("failed to start PTY: %w", err)
	}
	p.Lock()
	p.ptmx[inv.Caller()] = ptmx
	p.Unlock()

	go p.startOutputReader(inv, ptmx)

	return ptmx, nil
}

func (p *interactiveShellSession) startOutputReader(inv *xconn.Invocation, ptmx *os.File) {
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
			_ = inv.SendProgress([]any{buf[:n]}, nil)
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

		if inv.Progress() {
			payload, err := inv.ArgBytes(0)
			if err != nil {
				return xconn.NewInvocationError("wamp.error.invalid_argument", err.Error())
			}

			if bytes.HasPrefix(payload, []byte("SIZE:")) {
				var cols, rows int
				n, _ := fmt.Sscanf(string(payload), "SIZE:%d:%d", &cols, &rows)
				if n == 2 {
					if cols < 0 || cols > math.MaxUint16 || rows < 0 || rows > math.MaxUint16 {
						return xconn.NewInvocationError("wamp.error.invalid_argument", "invalid size")
					}
					if !exists {
						newPt, err := p.startPtySession(inv)
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
				newPt, err := p.startPtySession(inv)
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

		p.Lock()
		if stored, ok := p.ptmx[caller]; ok {
			_ = stored.Close()
			delete(p.ptmx, caller)
		}
		p.Unlock()

		return xconn.NewInvocationResult()
	}
}

func StartInteractiveShell(session *xconn.Session, procedure string) error {
	fd := int(os.Stdin.Fd())
	oldState, err := term.MakeRaw(fd)
	if err != nil {
		return fmt.Errorf("failed to set raw mode: %w", err)
	}
	defer func() { _ = term.Restore(fd, oldState) }()

	progressChan := make(chan *xconn.Progress, 32)

	sendSize := func() *xconn.Progress {
		width, height, err := term.GetSize(fd)
		if err != nil {
			return nil
		}
		msg := fmt.Sprintf("SIZE:%d:%d", width, height)
		return xconn.NewProgress(msg)
	}

	if p := sendSize(); p != nil {
		progressChan <- p
	}

	go func() {
		sigChan := make(chan os.Signal, 1)
		signal.Notify(sigChan, syscall.SIGWINCH)
		for range sigChan {
			if p := sendSize(); p != nil {
				progressChan <- p
			}
		}
	}()

	go func() {
		buf := make([]byte, 1024)
		for {
			n, err := os.Stdin.Read(buf)
			if err != nil {
				close(progressChan)
				return
			}
			progressChan <- xconn.NewProgress(buf[:n])
		}
	}()

	call := session.Call(procedure).
		ProgressSender(func(ctx context.Context) *xconn.Progress {
			p, ok := <-progressChan
			if !ok {
				return xconn.NewFinalProgress()
			}
			return p
		}).
		ProgressReceiver(func(result *xconn.ProgressResult) {
			if len(result.Args()) > 0 {
				_, err = os.Stdout.Write(result.Args()[0].([]byte))
			} else {
				_ = term.Restore(fd, oldState)
				os.Exit(0)
			}
		}).Do()

	if call.Err != nil {
		return fmt.Errorf("shell error: %w", call.Err)
	}
	return nil
}
