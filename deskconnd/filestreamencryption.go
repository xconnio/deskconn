package deskconnd

import (
	"encoding/json"
	"fmt"

	"github.com/xconnio/deskconn/common"
)

// P2PServerKeyExchange performs the server side of the per-channel key
// exchange. firstMessage is the client's plaintext public key, already
// consumed by xlink's channel classification, so it's parsed directly
// rather than read again off the channel. Sends back our own plaintext
// public key and returns the derived session keys; every message from here
// on is encrypted.
func P2PServerKeyExchange(channel common.MessageChannel, firstMessage []byte) (sendKey, receiveKey []byte, err error) {
	var clientKey common.KeyExchangeMsg
	if err := json.Unmarshal(firstMessage, &clientKey); err != nil {
		return nil, nil, err
	}
	if len(clientKey.PublicKey) != 32 {
		return nil, nil, fmt.Errorf("invalid client public key length: %d", len(clientKey.PublicKey))
	}

	publicKey, sendKey, receiveKey, err := ServerKeyExchange(clientKey.PublicKey)
	if err != nil {
		return nil, nil, err
	}
	if err := common.SendWebRTCJSON(channel, common.KeyExchangeMsg{PublicKey: publicKey}); err != nil {
		return nil, nil, err
	}
	return sendKey, receiveKey, nil
}
