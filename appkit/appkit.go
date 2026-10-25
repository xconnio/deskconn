// Package appkit is what an app served next to deskconnd needs, like media-app or
// remotecontrol-app (see the README's "Adding an app"): where deskconnd's local socket is,
// how to register the app's apps and follow whether they're enabled, the end-to-end
// encryption every app's procedures use, and the thumbnails deskconnd makes.
package appkit

import (
	"path/filepath"

	"github.com/xconnio/deskconn/internal/common"
	"github.com/xconnio/xconn-core/bindings/go"
)

// LocalRealm is the realm on deskconnd's local socket (see SocketURI), which apps join
// anonymously to register their procedures.
const LocalRealm = common.LocalRealm

// ProcedureAppsRegister registers the apps a session serves, with a JSON list of
// {"id", "name", "key_exchange", "requires", "default_off"}. They're listed until the
// session leaves.
const ProcedureAppsRegister = common.ProcedureAppsRegister

// ProcedureCapabilitiesList lists every app, with whether it's enabled, and
// TopicCapabilitiesChanged is published with the same list when an app is enabled or
// disabled.
const (
	ProcedureCapabilitiesList = common.ProcedureCapabilitiesList
	TopicCapabilitiesChanged  = common.TopicCapabilitiesChanged
)

const (
	ErrInvalidArgument = common.ErrInvalidArgument
	ErrNotAuthorized   = common.ErrNotAuthorized
	ErrOperationFailed = common.ErrOperationFailed
)

// CfgDirectory is deskconnd's config directory, ~/.deskconn, where its socket and
// config.yml are.
func CfgDirectory() (string, error) {
	return common.CfgDirectory()
}

// SocketURI is the URI of deskconnd's local socket in cfgDirectory.
func SocketURI(cfgDirectory string) string {
	return common.UnixSocketURI(filepath.Join(cfgDirectory, "deskconn.sock"))
}

// Handler serves one of an app's procedures: req is the decrypted request, nil when there
// is none, and the result is sent as JSON. Return an *xconn.Error to choose the error URI
// the client gets; any other error is ErrOperationFailed.
type Handler = common.AppHandler

// Encryption serves an app's key exchange and end-to-end encrypts its procedures with each
// caller's keys: Register its key exchange, then wrap each procedure's Handler with
// Encrypted.
type Encryption = common.AppEncryption

func NewEncryption() *Encryption {
	return common.NewAppEncryption()
}

// SessionKeys are the keys a client derives from a key exchange (see ClientKeyExchange).
type SessionKeys = common.SessionKeys

// ClientKeyExchange exchanges keys with an app's keyExchange procedure as a client does,
// e.g. to test the app.
func ClientKeyExchange(session *xconn.Session, keyExchange string) (*SessionKeys, error) {
	return common.ClientKeyExchangeWith(session, keyExchange)
}

// EncryptedCall calls procedure as a client does, with payload encrypted with keys, and
// returns the decrypted result.
func EncryptedCall(session *xconn.Session, procedure string, payload []byte, keys *SessionKeys) ([]byte, error) {
	return common.EncryptedCall(session, procedure, payload, keys)
}

// MaxThumbnailSourceSize is the largest image ImageThumbnail is used for, and
// ThumbnailMaxDim is the longest side of a thumbnail, in pixels.
const (
	MaxThumbnailSourceSize = common.MaxThumbnailSourceSize
	ThumbnailMaxDim        = common.ThumbnailMaxDim
)

// ImageThumbnail, VideoThumbnail and PDFThumbnail return a base64 JPEG thumbnail of the
// file at path, like deskconnd's file browser shows, or "" if it can't be made. Videos
// need ffmpeg and PDFs pdftocairo.
func ImageThumbnail(path string) string { return common.ImageThumbnail(path) }
func VideoThumbnail(path string) string { return common.VideoThumbnail(path) }
func PDFThumbnail(path string) string   { return common.PDFThumbnail(path) }
