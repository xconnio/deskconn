package common

import (
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
)

// WriteFrame/ReadFrame are the raw length-prefixed framing every message on
// a QUIC file-transfer stream uses, whether it carries plaintext JSON (the
// RoutingFrame and key exchange, which the router and the peer must be able
// to read) or an encrypted payload (everything after the key exchange).
func WriteFrame(w io.Writer, data []byte) error {
	var length [4]byte
	binary.BigEndian.PutUint32(length[:], uint32(len(data))) //nolint:gosec
	if _, err := w.Write(length[:]); err != nil {
		return err
	}
	_, err := w.Write(data)
	return err
}

func ReadFrame(r io.Reader) ([]byte, error) {
	var length [4]byte
	if _, err := io.ReadFull(r, length[:]); err != nil {
		return nil, err
	}
	n := binary.BigEndian.Uint32(length[:])
	if n > maxMsgSize {
		return nil, fmt.Errorf("message too large: %d bytes", n)
	}
	buf := make([]byte, n)
	if _, err := io.ReadFull(r, buf); err != nil {
		return nil, err
	}
	return buf, nil
}

func WriteMsg(w io.Writer, v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	return WriteFrame(w, b)
}

func ReadMsg(r io.Reader, v any) error {
	buf, err := ReadFrame(r)
	if err != nil {
		return err
	}
	return json.Unmarshal(buf, v)
}

// WriteEncryptedMsg/ReadEncryptedMsg are WriteMsg/ReadMsg's encrypted
// counterparts, used for every FSRequest/FSResponse once the per-stream key
// exchange has happened.
func WriteEncryptedMsg(w io.Writer, v any, key []byte) error {
	plaintext, err := json.Marshal(v)
	if err != nil {
		return err
	}
	ciphertext, err := EncryptPayload(plaintext, key)
	if err != nil {
		return err
	}
	return WriteFrame(w, ciphertext)
}

func ReadEncryptedMsg(r io.Reader, v any, key []byte) error {
	ciphertext, err := ReadFrame(r)
	if err != nil {
		return err
	}
	plaintext, err := DecryptPayload(ciphertext, key)
	if err != nil {
		return err
	}
	return json.Unmarshal(plaintext, v)
}

// RoutingFrame is the first message on every client-opened QUIC stream.
// deskconn-router's connBroker reads exactly these fields to route the
// stream to the right device, then bridges the rest of the stream through
// unmodified -- so it must be sent in the clear, before the key exchange
// (see KeyExchangeMsg) and the encrypted real request that follow it.
type RoutingFrame struct {
	Realm     string `json:"realm"`
	Op        FSOp   `json:"op"`
	Path      string `json:"path"`
	Recursive bool   `json:"recursive,omitempty"`
}

// KeyExchangeMsg carries one side's ephemeral X25519 public key. Everything
// after the exchange -- every FSRequest/FSResponse and all file data -- is
// ChaCha20-Poly1305 encrypted with the derived keys, since deskconn-router
// terminates QUIC/TLS at each hop rather than tunneling it through, and
// would otherwise see the plaintext it's relaying. Reused as-is for --mode
// p2p (see filestreamencryption.go).
type KeyExchangeMsg struct {
	PublicKey []byte `json:"public_key"`
}

// CopyEncrypted reads exactly n plaintext bytes from src, encrypting and
// framing each chunk as it goes, reporting progress as real (plaintext)
// bytes processed.
func CopyEncrypted(dst io.Writer, src io.Reader, n int64, key []byte, progress *TransferProgress) error {
	buf := make([]byte, encChunkSize)
	var written int64
	for written < n {
		toRead := int64(len(buf))
		if remaining := n - written; remaining < toRead {
			toRead = remaining
		}
		rn, rerr := io.ReadFull(src, buf[:toRead])
		if rn > 0 {
			ciphertext, err := EncryptPayload(buf[:rn], key)
			if err != nil {
				return err
			}
			if err := WriteFrame(dst, ciphertext); err != nil {
				return err
			}
			written += int64(rn)
			if progress != nil {
				progress.Add(int64(rn))
			}
		}
		if rerr != nil {
			return rerr
		}
	}
	return nil
}

// CopyDecrypted is CopyEncrypted's counterpart: reads encrypted chunks from
// src until exactly n plaintext bytes have been written to dst.
func CopyDecrypted(dst io.Writer, src io.Reader, n int64, key []byte, progress *TransferProgress) error {
	var written int64
	for written < n {
		ciphertext, err := ReadFrame(src)
		if err != nil {
			return err
		}
		plaintext, err := DecryptPayload(ciphertext, key)
		if err != nil {
			return err
		}
		wn, werr := dst.Write(plaintext)
		written += int64(wn)
		if progress != nil {
			progress.Add(int64(wn))
		}
		if werr != nil {
			return werr
		}
	}
	return nil
}
