package common

import (
	"crypto/ed25519"
	"crypto/subtle"
	"encoding/hex"
	"fmt"

	packetcrypto "github.com/xconnio/xconn-core/bindings/go/crypto"
)

func CreateX25519KeyPair() (publicKey []byte, privateKey []byte, err error) {
	return packetcrypto.GenerateKeyPair()
}

func PerformKeyExchange(privateKey, peerPublicKey []byte) ([]byte, error) {
	return packetcrypto.SharedSecret(privateKey, peerPublicKey)
}

func AllZeros(b []byte) bool {
	zero := make([]byte, len(b))
	return subtle.ConstantTimeCompare(b, zero) == 1
}

func DeriveKeyHKDF(sharedSecret, info []byte) ([]byte, error) {
	return packetcrypto.DeriveKey(sharedSecret, info)
}

// ClientKeyExchangeKeys derives sendKey ("frontendToBackend") and receiveKey ("backendToFrontend")
// from the client's private key and the server's public key.
func ClientKeyExchangeKeys(privateKey, serverPublicKey []byte) (sendKey, receiveKey []byte, err error) {
	sharedSecret, err := PerformKeyExchange(privateKey, serverPublicKey)
	if err != nil {
		return
	}
	sendKey, err = DeriveKeyHKDF(sharedSecret, []byte("frontendToBackend"))
	if err != nil {
		return
	}
	receiveKey, err = DeriveKeyHKDF(sharedSecret, []byte("backendToFrontend"))
	return
}

// ServerKeyExchange generates an ephemeral key pair, performs X25519 with the client's
// public key, and derives sendKey ("backendToFrontend") and receiveKey ("frontendToBackend").
// Returns the server's public key so the caller can forward it to the client.
func ServerKeyExchange(clientPublicKey []byte) (serverPublicKey, sendKey, receiveKey []byte, err error) {
	serverPublicKey, serverPrivateKey, err := CreateX25519KeyPair()
	if err != nil {
		return
	}
	sharedSecret, err := PerformKeyExchange(serverPrivateKey, clientPublicKey)
	if err != nil {
		return nil, nil, nil, err
	}
	sendKey, err = DeriveKeyHKDF(sharedSecret, []byte("backendToFrontend"))
	if err != nil {
		return nil, nil, nil, err
	}
	receiveKey, err = DeriveKeyHKDF(sharedSecret, []byte("frontendToBackend"))
	return
}

func EncryptChaCha20Poly1305(plaintext, key []byte) ([]byte, []byte, error) {
	packet, err := packetcrypto.Encrypt(plaintext, key, nil)
	if err != nil {
		return nil, nil, err
	}
	return packet[12:], packet[:12], nil
}

func DecryptChaCha20Poly1305(ciphertext, nonce, key []byte) ([]byte, error) {
	if len(nonce) != 12 {
		return nil, fmt.Errorf("invalid nonce length: %d", len(nonce))
	}
	packet := make([]byte, 0, len(nonce)+len(ciphertext))
	packet = append(packet, nonce...)
	packet = append(packet, ciphertext...)
	plaintext, err := packetcrypto.Decrypt(packet, key, nil)
	if err != nil {
		return nil, fmt.Errorf("failed to decrypt: %w", err)
	}
	return plaintext, nil
}

// EncryptPayload retains DeskConn's nonce-prefixed packet format.
func EncryptPayload(plaintext, key []byte) ([]byte, error) {
	return packetcrypto.Encrypt(plaintext, key, nil)
}

func DecryptPayload(data, key []byte) ([]byte, error) {
	return packetcrypto.Decrypt(data, key, nil)
}

// IsPublicKey reports whether s is a public key as desk keygen prints it: 64 hex digits.
func IsPublicKey(s string) bool {
	raw, err := hex.DecodeString(s)
	return err == nil && len(raw) == ed25519.PublicKeySize
}
