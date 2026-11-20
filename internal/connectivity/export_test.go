package connectivity

import api "github.com/xconnio/xconn/bindings/go"

var (
	ListenLoopbackApp  = listenLoopbackApp
	ConnectLoopbackApp = connectLoopbackApp
)

func (a *Authenticator) AddKey(event *api.Event) error     { return a.addKey(event) }
func (a *Authenticator) RemoveKeys(event *api.Event) error { return a.removeKeys(event) }
