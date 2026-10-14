package relay

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"sync"

	log "github.com/sirupsen/logrus"

	"github.com/xconnio/xconn-go"
)

type connBroker struct {
	sync.RWMutex
	devices map[string]*xconn.QUICConn
	// userRoles are the roles whose sessions may relay streams to their realm's machine.
	userRoles map[string]bool
}

func newConnBroker(userRoles map[string]bool) *connBroker {
	return &connBroker{devices: make(map[string]*xconn.QUICConn), userRoles: userRoles}
}

func (r *connBroker) register(realm string, conn *xconn.QUICConn) {
	r.Lock()
	r.devices[realm] = conn
	r.Unlock()
}

func (r *connBroker) unregister(realm string, conn *xconn.QUICConn) {
	r.Lock()
	if r.devices[realm] == conn {
		delete(r.devices, realm)
	}
	r.Unlock()
}

func (r *connBroker) lookup(realm string) (*xconn.QUICConn, bool) {
	r.RLock()
	conn, ok := r.devices[realm]
	r.RUnlock()
	return conn, ok
}

func (r *connBroker) onDeviceConnect(ctx context.Context, session xconn.BaseSession, conn *xconn.QUICConn) {
	if !isMachineSession(session) {
		return
	}
	realm := session.Realm()
	r.register(realm, conn)
	log.Infof("device registered: realm=%s authid=%s", realm, session.AuthID())
	<-ctx.Done()
	r.unregister(realm, conn)
	log.Infof("device unregistered: realm=%s", realm)
}

type relayRequest struct {
	Realm     string `json:"realm"`
	Op        string `json:"op"`
	Path      string `json:"path"`
	Recursive bool   `json:"recursive,omitempty"`
}

func (r *connBroker) onClientStream(cliStream net.Conn, session xconn.BaseSession) {
	defer cliStream.Close()

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

	deviceConn, ok := r.lookup(req.Realm)
	if !ok {
		_ = writeRelayError(cliStream, "device not connected")
		return
	}

	devStream, err := deviceConn.OpenStream()
	if err != nil {
		_ = writeRelayError(cliStream, fmt.Sprintf("failed to open device stream: %v", err))
		return
	}
	defer devStream.Close()

	if err := writeRelayMsg(devStream, req); err != nil {
		return
	}

	bridge(cliStream, devStream)
}

func bridge(a, b net.Conn) {
	done := make(chan struct{}, 2)
	go func() { _, _ = io.Copy(a, b); done <- struct{}{} }()
	go func() { _, _ = io.Copy(b, a); done <- struct{}{} }()
	<-done
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
