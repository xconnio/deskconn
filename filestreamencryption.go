package deskconn

import (
	"encoding/json"
	"fmt"
	"io"
	"time"

	"github.com/pion/webrtc/v4"
)

const (
	P2PMsgControl byte = iota // encrypted JSON control message (FSRequest/FSResponse)
	P2PMsgData                // encrypted raw chunk bytes
)

const (
	// FileStreamChunkSize is the size of each binary data channel message
	// used while streaming a byte range, in either direction. Set just under
	// pion's default SCTP max message size (math.MaxUint16 = 65535 bytes),
	// leaving room for the 1-byte envelope kind prefix and the 28-byte
	// ChaCha20-Poly1305 nonce+tag overhead.
	FileStreamChunkSize = 65024

	// FileStreamMaxBuffered/FileStreamBufferedLow mirror the backpressure
	// thresholds used for the main WAMP peer in xconn-webrtc-go's peer.go, so a
	// slow reader can't make the send buffer grow unbounded.
	FileStreamMaxBuffered = 512 * 1024 // 512KB
	FileStreamBufferedLow = 256 * 1024 // 256KB

	// FileStreamRequestTimeout bounds how long a write handler waits between
	// binary messages before giving up on a stalled sender.
	FileStreamRequestTimeout = 10 * time.Second

	// FileStreamSessionIdleTimeout bounds how long a read/write channel
	// waits for the next chunk request from its worker before giving up and
	// closing -- normally the worker either sends another request right away
	// or closes the channel itself once its share of the transfer is done.
	FileStreamSessionIdleTimeout = 30 * time.Second
)

// SendWebRTCJSON JSON-marshals v and sends it on channel as a plaintext
// text message -- used only for the key exchange, before either side has a
// key to encrypt anything with.
func SendWebRTCJSON(channel MessageChannel, v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	return channel.SendText(string(b))
}

// SendEncryptedJSON JSON-marshals v, encrypts it with key, and sends it on
// channel as a P2PMsgControl envelope.
func SendEncryptedJSON(channel MessageChannel, v any, key []byte) error {
	plaintext, err := json.Marshal(v)
	if err != nil {
		return err
	}
	return SendEncryptedEnvelope(channel, P2PMsgControl, plaintext, key)
}

func SendEncryptedEnvelope(channel MessageChannel, kind byte, plaintext, key []byte) error {
	ciphertext, err := EncryptPayload(plaintext, key)
	if err != nil {
		return err
	}
	envelope := make([]byte, 1+len(ciphertext))
	envelope[0] = kind
	copy(envelope[1:], ciphertext)
	return channel.Send(envelope)
}

// DecryptEnvelope splits a received binary message into its kind byte and
// decrypted plaintext.
func DecryptEnvelope(data []byte, key []byte) (kind byte, plaintext []byte, err error) {
	if len(data) < 1 {
		return 0, nil, fmt.Errorf("empty message")
	}
	plaintext, err = DecryptPayload(data[1:], key)
	if err != nil {
		return 0, nil, err
	}
	return data[0], plaintext, nil
}

// SendEncryptedBytes writes data to channel as FileStreamChunkSize
// plaintext chunks, each individually encrypted and sent as its own
// P2PMsgData envelope, blocking on sendReady/closed whenever the channel's
// send buffer is over FileStreamMaxBuffered.
func SendEncryptedBytes(channel MessageChannel, closed, sendReady <-chan struct{}, data, key []byte) error {
	for len(data) > 0 {
		select {
		case <-closed:
			return io.ErrClosedPipe
		default:
		}

		n := FileStreamChunkSize
		if n > len(data) {
			n = len(data)
		}

		ciphertext, err := EncryptPayload(data[:n], key)
		if err != nil {
			return err
		}
		envelope := make([]byte, 1+len(ciphertext))
		envelope[0] = P2PMsgData
		copy(envelope[1:], ciphertext)

		for channel.BufferedAmount()+uint64(len(envelope)) > FileStreamMaxBuffered {
			select {
			case <-sendReady:
			case <-closed:
				return io.ErrClosedPipe
			}
		}

		if err := channel.Send(envelope); err != nil {
			return err
		}
		data = data[n:]
	}
	return nil
}

// P2PServerKeyExchange performs the server side of the per-channel key
// exchange. firstMessage is the client's plaintext public key, already
// consumed by xlink's channel classification, so it's parsed directly
// rather than read again off the channel. Sends back our own plaintext
// public key and returns the derived session keys; every message from here
// on is encrypted.
func P2PServerKeyExchange(channel MessageChannel, firstMessage []byte) (sendKey, receiveKey []byte, err error) {
	var clientKey KeyExchangeMsg
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
	if err := SendWebRTCJSON(channel, KeyExchangeMsg{PublicKey: publicKey}); err != nil {
		return nil, nil, err
	}
	return sendKey, receiveKey, nil
}

// p2pClientKeyExchange performs the client side of the per-channel key
// exchange on a freshly opened channel: send our plaintext public key as
// the channel's first message, wait for the peer's, derive session keys.
func p2pClientKeyExchange(channel MessageChannel, closed <-chan struct{}) (sendKey, receiveKey []byte, err error) {
	publicKey, privateKey, err := CreateX25519KeyPair()
	if err != nil {
		return nil, nil, err
	}

	peerKeyCh := make(chan KeyExchangeMsg, 1)
	channel.OnMessage(func(msg webrtc.DataChannelMessage) {
		if !msg.IsString {
			return
		}
		var peerKey KeyExchangeMsg
		if json.Unmarshal(msg.Data, &peerKey) == nil {
			select {
			case peerKeyCh <- peerKey:
			default:
			}
		}
	})

	if err := SendWebRTCJSON(channel, KeyExchangeMsg{PublicKey: publicKey}); err != nil {
		return nil, nil, err
	}

	peerKey, err := RecvPriority(peerKeyCh, closed, P2PRequestTimeout)
	if err != nil {
		return nil, nil, err
	}
	if len(peerKey.PublicKey) != 32 {
		return nil, nil, fmt.Errorf("invalid peer public key length: %d", len(peerKey.PublicKey))
	}

	return ClientKeyExchangeKeys(privateKey, peerKey.PublicKey)
}
