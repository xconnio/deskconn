package common

import (
	"encoding/json"
	"fmt"
	"io"
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
