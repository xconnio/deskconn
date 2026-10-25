package userd

import (
	"encoding/json"

	"github.com/xconnio/deskconn/internal/common"
	"github.com/xconnio/xconn-core/bindings/go"
)

// callerKeys returns the keys the caller set up with ProcedureKeyExchange, or the error
// to return if it hasn't.
func (d *Deskconn) callerKeys(inv *xconn.Invocation) (*encryptionKeys, *xconn.Result) {
	enc, ok := d.keys.fetch(inv.Caller())
	if !ok {
		return nil, xconn.NewResultError(common.ErrInvalidArgument, "no session keys found, call key exchange first")
	}
	return enc, nil
}

// decryptArgs decrypts the call's single encrypted argument and unmarshals its JSON into out.
func decryptArgs(enc *encryptionKeys, inv *xconn.Invocation, out any) *xconn.Result {
	encrypted, err := inv.ArgBytes(0)
	if err != nil {
		return xconn.NewResultError(common.ErrInvalidArgument, err.Error())
	}
	plaintext, err := common.DecryptPayload(encrypted, enc.receiveKey)
	if err != nil {
		return xconn.NewResultError(common.ErrInvalidArgument, err.Error())
	}
	if err := json.Unmarshal(plaintext, out); err != nil {
		return xconn.NewResultError(common.ErrInvalidArgument, err.Error())
	}
	return nil
}

// encryptResult marshals result to JSON and returns it encrypted as the call's single result.
func encryptResult(enc *encryptionKeys, result any) *xconn.Result {
	resultBytes, err := json.Marshal(result)
	if err != nil {
		return xconn.NewResultError(common.ErrOperationFailed, err.Error())
	}
	encryptedResult, err := common.EncryptPayload(resultBytes, enc.sendKey)
	if err != nil {
		return xconn.NewResultError(common.ErrOperationFailed, err.Error())
	}
	return xconn.NewResult(encryptedResult)
}
