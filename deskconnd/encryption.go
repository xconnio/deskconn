package deskconnd

import (
	"github.com/xconnio/deskconn/common"
)

// ServerKeyExchange generates an ephemeral key pair, performs X25519 with the client's
// public key, and derives sendKey ("backendToFrontend") and receiveKey ("frontendToBackend").
// Returns the server's public key so the caller can forward it to the client.
func ServerKeyExchange(clientPublicKey []byte) (serverPublicKey, sendKey, receiveKey []byte, err error) {
	serverPublicKey, serverPrivateKey, err := common.CreateX25519KeyPair()
	if err != nil {
		return
	}
	sharedSecret, err := common.PerformKeyExchange(serverPrivateKey, clientPublicKey)
	if err != nil {
		return nil, nil, nil, err
	}
	sendKey, err = common.DeriveKeyHKDF(sharedSecret, []byte("backendToFrontend"))
	if err != nil {
		return nil, nil, nil, err
	}
	receiveKey, err = common.DeriveKeyHKDF(sharedSecret, []byte("frontendToBackend"))
	return
}
