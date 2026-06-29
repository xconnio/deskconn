package deskconn

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"

	"github.com/xconnio/xconn-go"
)

const ProcedureFileCat = "io.xconn.deskconn.deskconnd.file.cat"

func (d *Deskconn) handleFileCat(ctx context.Context, inv *xconn.Invocation) *xconn.InvocationResult {
	remotePath, err := inv.ArgString(0)
	if err != nil {
		return xconn.NewInvocationError(ErrInvalidArgument, err.Error())
	}

	clientPublicKey, err := inv.ArgBytes(1)
	if err != nil || len(clientPublicKey) != 32 {
		return xconn.NewInvocationError(ErrInvalidArgument, "client public key is required")
	}

	serverPublicKey, serverPrivateKey, err := CreateX25519KeyPair()
	if err != nil {
		return xconn.NewInvocationError(ErrOperationFailed, err.Error())
	}

	sharedSecret, err := PerformKeyExchange(serverPrivateKey, clientPublicKey)
	if err != nil {
		return xconn.NewInvocationError(ErrOperationFailed, err.Error())
	}

	sendKey, err := DeriveKeyHKDF(sharedSecret, []byte("backendToFrontend"))
	if err != nil {
		return xconn.NewInvocationError(ErrOperationFailed, err.Error())
	}

	if err := inv.SendProgress([]any{append([]byte("KEY:"), serverPublicKey...)}, nil); err != nil {
		return xconn.NewInvocationError(ErrOperationFailed, err.Error())
	}

	homeDir, err := os.UserHomeDir()
	if err != nil {
		return xconn.NewInvocationError(ErrOperationFailed, err.Error())
	}

	var resolvedPath string
	if filepath.IsAbs(remotePath) {
		resolvedPath = filepath.Clean(remotePath)
	} else {
		resolvedPath = filepath.Clean(filepath.Join(homeDir, remotePath))
	}

	info, err := os.Lstat(resolvedPath)
	if err != nil {
		if os.IsNotExist(err) {
			return xconn.NewInvocationError(ErrInvalidArgument,
				fmt.Sprintf("%s: no such file or directory", remotePath))
		}
		return xconn.NewInvocationError(ErrOperationFailed, err.Error())
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
	return streamCat(session, ProcedureFileCat, false, remotePath)
}

func CatFileViaProxy(localSession *xconn.Session, realm, remotePath string, p2p bool) error {
	return streamCat(localSession, ProcedureProxyCat, p2p, realm, remotePath)
}

func streamCat(session *xconn.Session, procedure string, p2p bool, prefixArgs ...any) error {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, os.Interrupt, syscall.SIGTERM)
	defer signal.Stop(sigCh)
	go func() {
		select {
		case <-sigCh:
			cancel()
		case <-ctx.Done():
		}
	}()

	publicKey, privateKey, err := CreateX25519KeyPair()
	if err != nil {
		return fmt.Errorf("failed to generate keypair: %w", err)
	}

	callArgs := append(prefixArgs, publicKey)
	if p2p {
		callArgs = append(callArgs, true)
	}

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
				sharedSecret, err := PerformKeyExchange(privateKey, data[4:])
				if err != nil {
					transferErr = fmt.Errorf("key exchange failed: %w", err)
					cancel()
					return
				}
				receiveKey, err = DeriveKeyHKDF(sharedSecret, []byte("backendToFrontend"))
				if err != nil {
					transferErr = fmt.Errorf("key derivation failed: %w", err)
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

			if _, err := os.Stdout.Write(chunk); err != nil {
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
