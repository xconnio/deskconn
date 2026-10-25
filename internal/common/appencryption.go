package common

import (
	"context"
	"encoding/json"
	"errors"
	"sync"

	"github.com/xconnio/xconn-core/bindings/go"
)

// AppHandler serves one of an app's procedures: req is the decrypted request, nil when
// there is none, and the result is sent as JSON. Return an *xconn.Error to choose the
// error URI the client gets; any other error is ErrOperationFailed.
type AppHandler func(ctx context.Context, req []byte) (any, error)

// AppEncryption serves an app's key exchange and end-to-end encrypts its procedures with
// each caller's keys, the same way userd does its own (see the README's "Adding an
// app").
type AppEncryption struct {
	mu sync.Mutex
	// keys are each caller's keys, by its session ID, from the app's side: SendKey
	// encrypts results and ReceiveKey decrypts requests.
	keys map[uint64]*SessionKeys
}

func NewAppEncryption() *AppEncryption {
	return &AppEncryption{keys: map[uint64]*SessionKeys{}}
}

// Register registers keyExchange on session, and forgets callers' keys when they leave.
func (e *AppEncryption) Register(session *xconn.Session, keyExchange string) error {
	if resp := session.Register(keyExchange, e.handleKeyExchange).Execute(); resp.Err != nil {
		return resp.Err
	}
	resp := session.Subscribe("wamp.session.on_leave", func(event *xconn.Event) {
		if sessionID, err := event.ArgUInt64(0); err == nil {
			e.mu.Lock()
			delete(e.keys, sessionID)
			e.mu.Unlock()
		}
	}).Execute()
	return resp.Err
}

// handleKeyExchange takes the client's X25519 public key and returns the app's.
func (e *AppEncryption) handleKeyExchange(_ context.Context, inv *xconn.Invocation) *xconn.Result {
	clientPublicKey, err := inv.ArgBytes(0)
	if err != nil {
		return xconn.NewResultError(ErrInvalidArgument, err.Error())
	}
	publicKey, sendKey, receiveKey, err := ServerKeyExchange(clientPublicKey)
	if err != nil {
		return xconn.NewResultError(ErrOperationFailed, err.Error())
	}
	e.mu.Lock()
	e.keys[inv.Caller()] = &SessionKeys{SendKey: sendKey, ReceiveKey: receiveKey}
	e.mu.Unlock()
	return xconn.NewResult(publicKey)
}

// Encrypted adapts h to a procedure taking its request as a single encrypted argument,
// empty or absent when there is none, and returning a single encrypted result.
func (e *AppEncryption) Encrypted(h AppHandler) xconn.InvocationHandler {
	return func(ctx context.Context, inv *xconn.Invocation) *xconn.Result {
		e.mu.Lock()
		keys, ok := e.keys[inv.Caller()]
		e.mu.Unlock()
		if !ok {
			return xconn.NewResultError(ErrInvalidArgument, "no session keys found, call key exchange first")
		}

		var req []byte
		if len(inv.Args()) > 0 {
			encrypted, err := inv.ArgBytes(0)
			if err != nil {
				return xconn.NewResultError(ErrInvalidArgument, err.Error())
			}
			if req, err = DecryptPayload(encrypted, keys.ReceiveKey); err != nil {
				return xconn.NewResultError(ErrInvalidArgument, err.Error())
			}
			if len(req) == 0 {
				req = nil
			}
		}

		result, err := h(ctx, req)
		if err != nil {
			var wampErr *xconn.Error
			if errors.As(err, &wampErr) {
				return xconn.NewResultError(wampErr.URI, wampErr.Args...)
			}
			return xconn.NewResultError(ErrOperationFailed, err.Error())
		}
		data, err := json.Marshal(result)
		if err != nil {
			return xconn.NewResultError(ErrOperationFailed, err.Error())
		}
		encrypted, err := EncryptPayload(data, keys.SendKey)
		if err != nil {
			return xconn.NewResultError(ErrOperationFailed, err.Error())
		}
		return xconn.NewResult(encrypted)
	}
}
