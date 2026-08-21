package deskconn

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"os/signal"
	"syscall"

	"github.com/xconnio/xconn-go"
)

const ProcedureFileCat = "io.xconn.deskconn.deskconnd.file.cat"

func (d *Deskconn) handleFileCat(ctx context.Context, inv *xconn.Invocation) *xconn.InvocationResult {
	remotePath, err := inv.ArgString(0)
	if err != nil {
		return xconn.NewInvocationError(ErrInvalidArgument, err.Error())
	}

	enc, invErr := serverStreamKeyExchange(inv, 1)
	if invErr != nil {
		return invErr
	}
	sendKey := enc.sendKey

	resolvedPath, info, pathErr := resolveAndStatPath(remotePath)
	if pathErr != nil {
		return pathErr
	}

	if info.IsDir() {
		return xconn.NewInvocationError(ErrInvalidArgument,
			fmt.Sprintf("%s: is a directory", remotePath))
	}

	f, err := os.Open(resolvedPath)
	if err != nil {
		return xconn.NewInvocationError(ErrOperationFailed, err.Error())
	}
	defer f.Close()

	buf := make([]byte, fileChunkSize)
	for {
		if err := ctx.Err(); err != nil {
			return xconn.NewInvocationError(ErrOperationFailed, err.Error())
		}
		n, readErr := f.Read(buf)
		if n > 0 {
			chunk := make([]byte, n)
			copy(chunk, buf[:n])
			encrypted, err := EncryptPayload(chunk, sendKey)
			if err != nil {
				return xconn.NewInvocationError(ErrOperationFailed, err.Error())
			}
			if err := inv.SendProgress([]any{encrypted}, nil); err != nil {
				return xconn.NewInvocationError(ErrOperationFailed, err.Error())
			}
		}
		if readErr == io.EOF {
			break
		}
		if readErr != nil {
			return xconn.NewInvocationError(ErrOperationFailed, readErr.Error())
		}
	}

	return xconn.NewInvocationResult()
}

func CatFile(session *xconn.Session, remotePath string) error {
	return streamCat(session, ProcedureFileCat, os.Stdout, remotePath)
}

func CatFileViaProxy(localSession *xconn.Session, realm, remotePath string) error {
	return streamCat(localSession, ProcedureProxyCat, os.Stdout, realm, remotePath)
}

func ReadFile(session *xconn.Session, remotePath string) ([]byte, error) {
	buf := &bytes.Buffer{}
	err := streamCat(session, ProcedureFileCat, buf, remotePath)
	return buf.Bytes(), err
}

func ReadFileViaProxy(localSession *xconn.Session, realm, remotePath string) ([]byte, error) {
	buf := &bytes.Buffer{}
	err := streamCat(localSession, ProcedureProxyCat, buf, realm, remotePath)
	return buf.Bytes(), err
}

func streamCat(session *xconn.Session, procedure string, out io.Writer, prefixArgs ...any) error {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, os.Interrupt, syscall.SIGTERM)
	defer signal.Stop(sigCh)
	SafeGo(func() {
		select {
		case <-sigCh:
			cancel()
		case <-ctx.Done():
		}
	})

	publicKey, privateKey, err := CreateX25519KeyPair()
	if err != nil {
		return fmt.Errorf("failed to generate keypair: %w", err)
	}

	callArgs := append(prefixArgs, publicKey)

	var (
		receiveKey     []byte
		firstServerMsg = true
		receivedData   bool
		transferErr    error
	)

	callResp := session.Call(procedure).
		ProgressReceiver(func(progress *xconn.ProgressResult) {
			if transferErr != nil {
				return
			}

			if firstServerMsg {
				firstServerMsg = false
				data, argErr := progress.ArgBytes(0)
				if argErr != nil || !bytes.HasPrefix(data, []byte("KEY:")) {
					transferErr = fmt.Errorf("expected key exchange message from server")
					cancel()
					return
				}
				var err error
				_, receiveKey, err = ClientKeyExchangeKeys(privateKey, data[4:])
				if err != nil {
					transferErr = fmt.Errorf("key exchange failed: %w", err)
					cancel()
					return
				}
				return
			}

			encrypted, err := progress.ArgBytes(0)
			if err != nil {
				transferErr = fmt.Errorf("invalid message format: %w", err)
				cancel()
				return
			}

			chunk, err := DecryptPayload(encrypted, receiveKey)
			if err != nil {
				transferErr = fmt.Errorf("failed to decrypt chunk: %w", err)
				cancel()
				return
			}

			if _, err := out.Write(chunk); err != nil {
				transferErr = err
				cancel()
				return
			}
			receivedData = true
		}).Args(callArgs...).DoContext(ctx)

	if transferErr != nil {
		return transferErr
	}

	if callResp.Err != nil && !receivedData {
		return callResp.Err
	}

	return nil
}
