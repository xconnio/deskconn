package xlink

import (
	"io"

	"github.com/xconnio/deskconn/common"
)

func WriteRelayHeader(w io.Writer, h common.RelayHeader) error {
	return common.WriteMsg(w, h)
}
