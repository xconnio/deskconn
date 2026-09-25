package deskconn

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"os/signal"
	"syscall"

	"github.com/xconnio/deskconn/common"
	"github.com/xconnio/xconn-go"
)

func CatFile(session *xconn.Session, remotePath string) error {
	return streamCat(session, common.ProcedureFileCat, os.Stdout, remotePath)
}

func CatFileViaProxy(localSession *xconn.Session, realm, remotePath string) error {
	return streamCat(localSession, common.ProcedureProxyCat, os.Stdout, realm, remotePath)
}

func ReadFile(session *xconn.Session, remotePath string) ([]byte, error) {
	buf := &bytes.Buffer{}
	err := streamCat(session, common.ProcedureFileCat, buf, remotePath)
	return buf.Bytes(), err
}

func ReadFileViaProxy(localSession *xconn.Session, realm, remotePath string) ([]byte, error) {
	buf := &bytes.Buffer{}
	err := streamCat(localSession, common.ProcedureProxyCat, buf, realm, remotePath)
	return buf.Bytes(), err
}

func streamCat(session *xconn.Session, procedure string, out io.Writer, prefixArgs ...any) error {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, os.Interrupt, syscall.SIGTERM)
	defer signal.Stop(sigCh)
	common.SafeGo(func() {
		select {
		case <-sigCh:
			cancel()
		case <-ctx.Done():
		}
	})

	publicKey, privateKey, err := common.CreateX25519KeyPair()
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
				_, receiveKey, err = common.ClientKeyExchangeKeys(privateKey, data[4:])
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

			chunk, err := common.DecryptPayload(encrypted, receiveKey)
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
