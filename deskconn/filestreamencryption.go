package deskconn

import (
	"encoding/json"
	"fmt"

	"github.com/pion/webrtc/v4"

	"github.com/xconnio/deskconn/common"
)

// p2pClientKeyExchange performs the client side of the per-channel key
// exchange on a freshly opened channel: send our plaintext public key as
// the channel's first message, wait for the peer's, derive session keys.
func p2pClientKeyExchange(channel common.MessageChannel, closed <-chan struct{}) (sendKey, receiveKey []byte,
	err error) {
	publicKey, privateKey, err := common.CreateX25519KeyPair()
	if err != nil {
		return nil, nil, err
	}

	peerKeyCh := make(chan common.KeyExchangeMsg, 1)
	channel.OnMessage(func(msg webrtc.DataChannelMessage) {
		if !msg.IsString {
			return
		}
		var peerKey common.KeyExchangeMsg
		if json.Unmarshal(msg.Data, &peerKey) == nil {
			select {
			case peerKeyCh <- peerKey:
			default:
			}
		}
	})

	if err := common.SendWebRTCJSON(channel, common.KeyExchangeMsg{PublicKey: publicKey}); err != nil {
		return nil, nil, err
	}

	peerKey, err := common.RecvPriority(peerKeyCh, closed, common.P2PRequestTimeout)
	if err != nil {
		return nil, nil, err
	}
	if len(peerKey.PublicKey) != 32 {
		return nil, nil, fmt.Errorf("invalid peer public key length: %d", len(peerKey.PublicKey))
	}

	return common.ClientKeyExchangeKeys(privateKey, peerKey.PublicKey)
}
