package deskconn

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/signal"
	"syscall"

	"github.com/xconnio/deskconn/internal/common"
	"github.com/xconnio/xconn-go"
)

// file.cat takes the path as an encrypted JSON string and streams the file as encrypted
// chunks, both with the session's key-exchange keys. Through the local daemon's proxy, the
// daemon does the encryption and the chunks arrive in plain text.

func CatFile(session *xconn.Session, remotePath string) error {
	return catFile(session, os.Stdout, remotePath)
}

func CatFileViaProxy(localSession *xconn.Session, realm, remotePath string) error {
	return streamCat(localSession, common.ProcedureProxyCat, os.Stdout, []any{realm, remotePath}, nil)
}

func ReadFile(session *xconn.Session, remotePath string) ([]byte, error) {
	buf := &bytes.Buffer{}
	err := catFile(session, buf, remotePath)
	return buf.Bytes(), err
}

func ReadFileViaProxy(localSession *xconn.Session, realm, remotePath string) ([]byte, error) {
	buf := &bytes.Buffer{}
	err := streamCat(localSession, common.ProcedureProxyCat, buf, []any{realm, remotePath}, nil)
	return buf.Bytes(), err
}

func catFile(session *xconn.Session, out io.Writer, remotePath string) error {
	enc, err := common.ClientKeyExchange(session)
	if err != nil {
		return fmt.Errorf("key exchange failed: %w", err)
	}
	encryptedPath, err := encryptJSON(remotePath, enc.SendKey)
	if err != nil {
		return err
	}
	return streamCat(session, common.ProcedureFileCat, out, []any{encryptedPath}, enc.ReceiveKey)
}

// encryptJSON marshals v to JSON and encrypts it with key.
func encryptJSON(v any, key []byte) ([]byte, error) {
	payload, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	return common.EncryptPayload(payload, key)
}

// streamCat calls procedure with args and writes each progress result's chunk to out,
// decrypting it with receiveKey unless that is nil.
func streamCat(session *xconn.Session, procedure string, out io.Writer, args []any, receiveKey []byte) error {
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

	var (
		receivedData bool
		transferErr  error
	)

	callResp := session.Call(procedure).
		ProgressReceiver(func(progress *xconn.ProgressResult) {
			if transferErr != nil {
				return
			}

			chunk, err := progress.ArgBytes(0)
			if err != nil {
				transferErr = fmt.Errorf("invalid message format: %w", err)
				cancel()
				return
			}
			if receiveKey != nil {
				chunk, err = common.DecryptPayload(chunk, receiveKey)
				if err != nil {
					transferErr = fmt.Errorf("failed to decrypt chunk: %w", err)
					cancel()
					return
				}
			}

			if _, err := out.Write(chunk); err != nil {
				transferErr = err
				cancel()
				return
			}
			receivedData = true
		}).Args(args...).DoContext(ctx)

	if transferErr != nil {
		return transferErr
	}

	if callResp.Err != nil && !receivedData {
		return callResp.Err
	}

	return nil
}
