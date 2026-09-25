package xlink

import (
	"net"
	"time"

	"github.com/xconnio/deskconn/common"
)

// streamOpReadTimeout bounds how long ReadStreamOp waits for the leading
// RoutingFrame before giving up on a stalled/malicious stream.
const streamOpReadTimeout = 30 * time.Second

// ReadStreamOp reads the leading RoutingFrame off a freshly accepted QUIC
// stream and returns which feature the rest of the stream belongs to,
// without needing to understand any of that feature's own payload -- this
// is exactly what xlink needs to classify an inbound stream and relay
// it to the right local backend. The routing frame is consumed; whatever
// comes after it on stream is untouched and ready for the backend to read.
func ReadStreamOp(stream net.Conn) (common.FSOp, error) {
	_ = stream.SetReadDeadline(time.Now().Add(streamOpReadTimeout))
	var route common.RoutingFrame
	if err := common.ReadMsg(stream, &route); err != nil {
		return "", err
	}
	return route.Op, nil
}
