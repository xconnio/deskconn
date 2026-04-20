package deskconn

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"fmt"
	"io"

	"golang.org/x/crypto/chacha20poly1305"
	"golang.org/x/crypto/curve25519"
	"golang.org/x/crypto/hkdf"
)

func CreateX25519KeyPair() (publicKey []byte, privateKey []byte, err error) {
	privateKey = make([]byte, 32)
	if _, err = rand.Read(privateKey); err != nil {
		return nil, nil, fmt.Errorf("failed to generate private key: %w", err)
	}

	// Generate the corresponding public key
	publicKey, err = curve25519.X25519(privateKey, curve25519.Basepoint)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to generate public key: %w", err)
	}

	return publicKey, privateKey, nil
}

func PerformKeyExchange(privateKey, peerPublicKey []byte) ([]byte, error) {
	if len(privateKey) != 32 {
		return nil, fmt.Errorf("invalid private key length: %d", len(privateKey))
	}
	if len(peerPublicKey) != 32 {
		return nil, fmt.Errorf("invalid public key length: %d", len(peerPublicKey))
	}

	sharedSecret, err := curve25519.X25519(privateKey, peerPublicKey)
	if err != nil {
		return nil, fmt.Errorf("failed to compute shared secret: %w", err)
	}

	if AllZeros(sharedSecret) {
		return nil, fmt.Errorf("computed shared secret is weak (all zeros)")
	}

	return sharedSecret, nil
}

func AllZeros(b []byte) bool {
	zero := make([]byte, len(b))
	return subtle.ConstantTimeCompare(b, zero) == 1
}

func DeriveKeyHKDF(sharedSecret, info []byte) ([]byte, error) {
	hk := hkdf.New(sha256.New, sharedSecret, nil, info)

	key := make([]byte, 32)
	if _, err := io.ReadFull(hk, key); err != nil {
		return nil, fmt.Errorf("failed to derive key: %w", err)
	}

	return key, nil
}

func EncryptChaCha20Poly1305(plaintext, key []byte) ([]byte, []byte, error) {
	aead, err := chacha20poly1305.New(key)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to create ChaCha20-Poly1305 cipher: %w", err)
	}

	nonce := make([]byte, aead.NonceSize())
	if _, err = rand.Read(nonce); err != nil {
		return nil, nil, fmt.Errorf("failed to generate nonce: %w", err)
	}

	ciphertext := aead.Seal(nil, nonce, plaintext, nil)

	return ciphertext, nonce, nil
}

func DecryptChaCha20Poly1305(ciphertext, nonce, key []byte) ([]byte, error) {
	aead, err := chacha20poly1305.New(key)
	if err != nil {
		return nil, fmt.Errorf("failed to create ChaCha20-Poly1305 cipher: %w", err)
	}

	plaintext, err := aead.Open(nil, nonce, ciphertext, nil)
	if err != nil {
		return nil, fmt.Errorf("failed to decrypt: %w", err)
	}

	return plaintext, nil
}

// EncryptPayload encrypts plaintext and returns nonce+ciphertext concatenated.
func EncryptPayload(plaintext, key []byte) ([]byte, error) {
	ciphertext, nonce, err := EncryptChaCha20Poly1305(plaintext, key)
	if err != nil {
		return nil, err
	}
	return append(nonce, ciphertext...), nil
}

// DecryptPayload decrypts a payload produced by EncryptPayload (nonce+ciphertext).
func DecryptPayload(data, key []byte) ([]byte, error) {
	if len(data) < 12 {
		return nil, fmt.Errorf("payload too short: %d bytes", len(data))
	}
	return DecryptChaCha20Poly1305(data[12:], data[:12], key)
}
