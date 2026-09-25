package common_test

import (
	"bytes"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/xconnio/deskconn/common"
)

func TestCreateX25519KeyPair(t *testing.T) {
	pub, priv, err := common.CreateX25519KeyPair()
	require.NoError(t, err)
	require.Len(t, pub, 32)
	require.Len(t, priv, 32)
	require.False(t, bytes.Equal(pub, priv))
}

func TestCreateX25519KeyPairIsRandom(t *testing.T) {
	pub1, priv1, err := common.CreateX25519KeyPair()
	require.NoError(t, err)

	pub2, priv2, err := common.CreateX25519KeyPair()
	require.NoError(t, err)

	require.False(t, bytes.Equal(pub1, pub2))
	require.False(t, bytes.Equal(priv1, priv2))
}

func TestPerformKeyExchangeSymmetric(t *testing.T) {
	pubA, privA, err := common.CreateX25519KeyPair()
	require.NoError(t, err)

	pubB, privB, err := common.CreateX25519KeyPair()
	require.NoError(t, err)

	secretAB, err := common.PerformKeyExchange(privA, pubB)
	require.NoError(t, err)

	secretBA, err := common.PerformKeyExchange(privB, pubA)
	require.NoError(t, err)

	require.Equal(t, secretAB, secretBA)
}

func TestPerformKeyExchangeInvalidPrivateKeyLength(t *testing.T) {
	pub, _, err := common.CreateX25519KeyPair()
	require.NoError(t, err)

	_, err = common.PerformKeyExchange(make([]byte, 16), pub)
	require.ErrorContains(t, err, "invalid private key length")
}

func TestPerformKeyExchangeInvalidPublicKeyLength(t *testing.T) {
	_, priv, err := common.CreateX25519KeyPair()
	require.NoError(t, err)

	_, err = common.PerformKeyExchange(priv, make([]byte, 16))
	require.ErrorContains(t, err, "invalid public key length")
}

func TestAllZeros(t *testing.T) {
	require.True(t, common.AllZeros(make([]byte, 32)))
	require.True(t, common.AllZeros([]byte{}))

	nonZero := make([]byte, 32)
	nonZero[0] = 1
	require.False(t, common.AllZeros(nonZero))
}

func TestDeriveKeyHKDF(t *testing.T) {
	secret := bytes.Repeat([]byte{0xAB}, 32)

	k1, err := common.DeriveKeyHKDF(secret, []byte("ctx-a"))
	require.NoError(t, err)
	require.Len(t, k1, 32)

	k2, err := common.DeriveKeyHKDF(secret, []byte("ctx-b"))
	require.NoError(t, err)
	require.Len(t, k2, 32)

	// Different info strings must produce different keys.
	require.False(t, bytes.Equal(k1, k2))
}

func TestDeriveKeyHKDFDeterministic(t *testing.T) {
	secret := bytes.Repeat([]byte{0x01}, 32)
	info := []byte("deterministic")

	k1, err := common.DeriveKeyHKDF(secret, info)
	require.NoError(t, err)

	k2, err := common.DeriveKeyHKDF(secret, info)
	require.NoError(t, err)

	require.Equal(t, k1, k2)
}

func TestEncryptDecryptChaCha20Poly1305RoundTrip(t *testing.T) {
	key := bytes.Repeat([]byte{0x42}, 32)
	plaintext := []byte("hello, ChaCha20-Poly1305")

	ciphertext, nonce, err := common.EncryptChaCha20Poly1305(plaintext, key)
	require.NoError(t, err)
	require.Len(t, nonce, 12)
	require.NotEqual(t, plaintext, ciphertext)

	got, err := common.DecryptChaCha20Poly1305(ciphertext, nonce, key)
	require.NoError(t, err)
	require.Equal(t, plaintext, got)
}

func TestEncryptChaCha20Poly1305InvalidKey(t *testing.T) {
	_, _, err := common.EncryptChaCha20Poly1305([]byte("data"), make([]byte, 16))
	require.Error(t, err)
}

func TestDecryptChaCha20Poly1305WrongKey(t *testing.T) {
	key := bytes.Repeat([]byte{0x42}, 32)
	ciphertext, nonce, err := common.EncryptChaCha20Poly1305([]byte("secret"), key)
	require.NoError(t, err)

	wrongKey := bytes.Repeat([]byte{0xFF}, 32)
	_, err = common.DecryptChaCha20Poly1305(ciphertext, nonce, wrongKey)
	require.ErrorContains(t, err, "failed to decrypt")
}

func TestDecryptChaCha20Poly1305TamperedCiphertext(t *testing.T) {
	key := bytes.Repeat([]byte{0x42}, 32)
	ciphertext, nonce, err := common.EncryptChaCha20Poly1305([]byte("secret"), key)
	require.NoError(t, err)

	ciphertext[0] ^= 0xFF
	_, err = common.DecryptChaCha20Poly1305(ciphertext, nonce, key)
	require.ErrorContains(t, err, "failed to decrypt")
}

func TestEncryptPayloadRoundTrip(t *testing.T) {
	key := bytes.Repeat([]byte{0x55}, 32)
	plaintext := []byte("payload round-trip test")

	payload, err := common.EncryptPayload(plaintext, key)
	require.NoError(t, err)
	// payload = 12-byte nonce + ciphertext
	require.Greater(t, len(payload), 12)

	got, err := common.DecryptPayload(payload, key)
	require.NoError(t, err)
	require.Equal(t, plaintext, got)
}

func TestEncryptPayloadNonceIsRandom(t *testing.T) {
	key := bytes.Repeat([]byte{0x55}, 32)
	plaintext := []byte("same plaintext")

	p1, err := common.EncryptPayload(plaintext, key)
	require.NoError(t, err)

	p2, err := common.EncryptPayload(plaintext, key)
	require.NoError(t, err)

	// Two encryptions of the same plaintext must produce different ciphertexts.
	require.False(t, bytes.Equal(p1, p2))
}

func TestDecryptPayloadTooShort(t *testing.T) {
	key := bytes.Repeat([]byte{0x55}, 32)
	_, err := common.DecryptPayload(make([]byte, 11), key)
	require.ErrorContains(t, err, "payload too short")
}

func TestDecryptPayloadTamperedNonce(t *testing.T) {
	key := bytes.Repeat([]byte{0x55}, 32)
	payload, err := common.EncryptPayload([]byte("data"), key)
	require.NoError(t, err)

	payload[0] ^= 0xFF
	_, err = common.DecryptPayload(payload, key)
	require.Error(t, err)
}

func TestEncryptPayloadEmptyPlaintext(t *testing.T) {
	key := bytes.Repeat([]byte{0x55}, 32)
	payload, err := common.EncryptPayload([]byte{}, key)
	require.NoError(t, err)

	got, err := common.DecryptPayload(payload, key)
	require.NoError(t, err)
	require.Empty(t, got)
}
