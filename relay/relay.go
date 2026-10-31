package relay

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"io"

	"github.com/xconnio/xconn-core/bindings/go"
)

type relayIdentity interface {
	Realm() string
	AuthID() string
	AuthRole() string
}

type relayRequest struct {
	Realm     string `json:"realm"`
	Op        string `json:"op"`
	Path      string `json:"path"`
	Recursive bool   `json:"recursive,omitempty"`
}

func streamRouting(userRoles []xconn.RealmRole) xconn.StreamRouting {
	roles := make(map[string]bool, len(userRoles))
	for _, role := range userRoles {
		roles[role.Name] = true
	}
	return xconn.StreamRouting{
		Destination: func(identity *xconn.SessionDetails) string {
			if isMachineSession(identity) {
				return identity.Realm()
			}
			return ""
		},
		Authorize: func(source *xconn.SessionDetails, key string, target *xconn.SessionDetails) bool {
			return source.Realm() == key && target.Realm() == key && roles[source.AuthRole()] && isMachineSession(target)
		},
		Route: func(_ context.Context, source *xconn.SessionDetails, stream *xconn.QUICStream) (xconn.StreamRoute, error) {
			var request relayRequest
			if err := readRelayMsg(stream, &request); err != nil {
				return xconn.StreamRoute{}, err
			}
			if request.Realm == "" {
				_ = writeRelayError(stream, "missing realm in request")
				return xconn.StreamRoute{}, errors.New("missing realm")
			}
			if request.Realm != source.Realm() || !roles[source.AuthRole()] {
				_ = writeRelayError(stream, "not authorized for realm")
				return xconn.StreamRoute{}, errors.New("not authorized for realm")
			}
			var prefix, failure bytes.Buffer
			if err := writeRelayMsg(&prefix, request); err != nil {
				return xconn.StreamRoute{}, err
			}
			_ = writeRelayError(&failure, "device not connected")
			return xconn.StreamRoute{Destination: request.Realm, Prefix: prefix.Bytes(), Failure: failure.Bytes()}, nil
		},
	}
}

func writeRelayMsg(w io.Writer, v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	var length [4]byte
	binary.BigEndian.PutUint32(length[:], uint32(len(b))) //nolint:gosec
	if _, err = w.Write(length[:]); err != nil {
		return err
	}
	_, err = w.Write(b)
	return err
}

func readRelayMsg(r io.Reader, v any) error {
	var length [4]byte
	if _, err := io.ReadFull(r, length[:]); err != nil {
		return err
	}
	n := binary.BigEndian.Uint32(length[:])
	buf := make([]byte, n)
	if _, err := io.ReadFull(r, buf); err != nil {
		return err
	}
	return json.Unmarshal(buf, v)
}

func writeRelayError(w io.Writer, msg string) error {
	return writeRelayMsg(w, map[string]string{"error": msg})
}
