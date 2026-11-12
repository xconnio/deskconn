package common

import "fmt"

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
