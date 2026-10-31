package relay

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"

	log "github.com/sirupsen/logrus"

	"github.com/xconnio/xconn-core/bindings/go"
)

type relayIdentity interface {
	Realm() string
	AuthID() string
	AuthRole() string
}

type connBroker struct {
	broker    *xconn.StreamBroker
	userRoles map[string]bool
}

func newConnBroker(ctx context.Context, userRoles map[string]bool) (*connBroker, error) {
	broker, err := xconn.NewStreamBroker(ctx, func(source *xconn.SessionDetails, key string,
		target *xconn.SessionDetails) bool {
		return source.Realm() == key && target.Realm() == key &&
			userRoles[source.AuthRole()] && isMachineSession(target)
	})
	if err != nil {
		return nil, err
	}
	return &connBroker{broker: broker, userRoles: userRoles}, nil
}

func (r *connBroker) onDeviceConnect(ctx context.Context, session *xconn.SessionDetails,
	conn *xconn.QUICConnection) {
	if !isMachineSession(session) {
		return
	}
	registration, err := r.broker.RegisterQUIC(ctx, session.Realm(), session, conn)
	if err != nil {
		log.Errorf("failed to register device: %v", err)
		return
	}
	defer registration.Close()
	log.Infof("device registered: realm=%s authid=%s", session.Realm(), session.AuthID())
	<-ctx.Done()
	log.Infof("device unregistered: realm=%s", session.Realm())
}

type relayRequest struct {
	Realm     string `json:"realm"`
	Op        string `json:"op"`
	Path      string `json:"path"`
	Recursive bool   `json:"recursive,omitempty"`
}

func (r *connBroker) onClientStream(ctx context.Context, cliStream *xconn.QUICStream, session *xconn.SessionDetails) {
	defer cliStream.Close()
	stop := context.AfterFunc(ctx, func() { _ = cliStream.Close() })
	defer stop()

	var req relayRequest
	if err := readRelayMsg(cliStream, &req); err != nil {
		return
	}

	if req.Realm == "" {
		_ = writeRelayError(cliStream, "missing realm in request")
		return
	}

	// Only relay to the realm the stream's connection authenticated to, and only for
	// user sessions: Config.Authenticate decided they may join that realm.
	if req.Realm != session.Realm() || !r.userRoles[session.AuthRole()] {
		_ = writeRelayError(cliStream, "not authorized for realm")
		return
	}

	devStream, err := r.broker.Open(ctx, session, req.Realm)
	if err != nil {
		_ = writeRelayError(cliStream, fmt.Sprintf("failed to open device stream: %v", err))
		return
	}
	defer devStream.Close()

	if err := writeRelayMsg(devStream, req); err != nil {
		return
	}

	_, _ = xconn.ForwardStreams(ctx, cliStream, devStream)
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
