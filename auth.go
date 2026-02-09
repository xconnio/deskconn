package deskconn

import (
	"fmt"
	"slices"
	"sync"

	"github.com/xconnio/wampproto-go/auth"
)

const ProcedureListKeys = "io.xconn.deskconn.desktop.access.key.list"

type CryptosignPrincipal struct {
	AuthID         string   `json:"authid"`
	AuthorizedKeys []string `json:"authorized_keys"`
	AuthRole       string   `json:"authrole"`
}

type Authenticator struct {
	principalByAuthID map[string]*CryptosignPrincipal
	sync.Mutex
}

func NewAuthenticator(principals []*CryptosignPrincipal) *Authenticator {
	authenticator := &Authenticator{}
	authenticator.SetPrincipals(principals)
	return authenticator
}

func (a *Authenticator) Methods() []auth.Method {
	return []auth.Method{auth.MethodCryptoSign}
}

func (a *Authenticator) Authenticate(request auth.Request) (auth.Response, error) {
	switch request.AuthMethod() {
	case auth.MethodCryptoSign:
		cryptosignRequest, ok := request.(*auth.RequestCryptoSign)
		if !ok {
			return nil, fmt.Errorf("invalid request")
		}

		principal, ok := a.principalByAuthID[cryptosignRequest.AuthID()]
		if !ok {
			return nil, fmt.Errorf("unknown authid %s", cryptosignRequest.AuthID())
		}
		if slices.Contains(principal.AuthorizedKeys, cryptosignRequest.PublicKey()) {
			return auth.NewResponse(cryptosignRequest.AuthID(), principal.AuthRole, 0)
		}

		return nil, fmt.Errorf("unknown publickey")

	default:
		return nil, fmt.Errorf("unknown authentication method: %v", request.AuthMethod())
	}
}

func (a *Authenticator) SetPrincipals(principals []*CryptosignPrincipal) {
	principalByAuthID := make(map[string]*CryptosignPrincipal, len(principals))
	for _, principal := range principals {
		principalByAuthID[principal.AuthID] = principal
	}
	a.Lock()
	defer a.Unlock()
	a.principalByAuthID = principalByAuthID
}
