package deskconnd

import (
	"encoding/json"

	"github.com/xconnio/deskconn/common"
	"github.com/xconnio/xconn-go"
)

// callerKeys returns the keys the caller set up with ProcedureKeyExchange, or the error
// to return if it hasn't.
func (d *Deskconn) callerKeys(inv *xconn.Invocation) (*encryptionKeys, *xconn.InvocationResult) {
	enc, ok := d.keys.fetch(inv.Caller())
	if !ok {
		return nil, xconn.NewInvocationError(common.ErrInvalidArgument, "no session keys found, call key exchange first")
	}
	return enc, nil
}

// decryptArgs decrypts the call's single encrypted argument and unmarshals its JSON into out.
func decryptArgs(enc *encryptionKeys, inv *xconn.Invocation, out any) *xconn.InvocationResult {
	encrypted, err := inv.ArgBytes(0)
	if err != nil {
		return xconn.NewInvocationError(common.ErrInvalidArgument, err.Error())
	}
	plaintext, err := common.DecryptPayload(encrypted, enc.receiveKey)
	if err != nil {
		return xconn.NewInvocationError(common.ErrInvalidArgument, err.Error())
	}
	if err := json.Unmarshal(plaintext, out); err != nil {
		return xconn.NewInvocationError(common.ErrInvalidArgument, err.Error())
	}
	return nil
}

// encryptResult marshals result to JSON and returns it encrypted as the call's single result.
func encryptResult(enc *encryptionKeys, result any) *xconn.InvocationResult {
	resultBytes, err := json.Marshal(result)
	if err != nil {
		return xconn.NewInvocationError(common.ErrOperationFailed, err.Error())
	}
	encryptedResult, err := common.EncryptPayload(resultBytes, enc.sendKey)
	if err != nil {
		return xconn.NewInvocationError(common.ErrOperationFailed, err.Error())
	}
	return xconn.NewInvocationResult(encryptedResult)
}

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
