package deskconn

import (
	"fmt"
	"slices"
	"sync"

	log "github.com/sirupsen/logrus"

	"github.com/xconnio/wampproto-go/auth"
	"github.com/xconnio/xconn-go"
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

func (a *Authenticator) RetrievePrincipal(authid string) (*CryptosignPrincipal, bool) {
	a.Lock()
	defer a.Unlock()
	cryptosignPrincipal, exists := a.principalByAuthID[authid]
	return cryptosignPrincipal, exists
}

func (a *Authenticator) SetPrincipal(authid string, principal *CryptosignPrincipal) {
	a.Lock()
	defer a.Unlock()
	a.principalByAuthID[authid] = principal
}

func (a *Authenticator) handleAddKey(event *xconn.Event) {
	authid, err := event.ArgString(0)
	if err != nil {
		log.Println("error parsing authid from event:", err)
	}
	pubKey, err := event.ArgString(1)
	if err != nil {
		log.Println("error parsing publickey from event:", err)
	}
	authrole, err := event.ArgString(2)
	if err != nil {
		log.Println("error parsing authrole from event:", err)
	}

	principal, exists := a.RetrievePrincipal(authid)
	if !exists {
		a.SetPrincipal(authid, principal)
	} else {
		principal.AuthorizedKeys = append(principal.AuthorizedKeys, pubKey)
		a.SetPrincipal(authid, principal)
	}

	principals, err := ReadPrincipalsFromFile()
	if err != nil {
		log.Println("error reading principals from file:", err)
	}

	principalUpdated := false
	for _, cryptosignPrincipal := range principals {
		if cryptosignPrincipal.AuthID == authid {
			cryptosignPrincipal.AuthorizedKeys = append(cryptosignPrincipal.AuthorizedKeys, pubKey)
			principalUpdated = true
			break
		}
	}
	if !principalUpdated {
		principals = append(principals, &CryptosignPrincipal{
			AuthID:         authid,
			AuthorizedKeys: []string{pubKey},
			AuthRole:       authrole,
		})
	}
	if err := WritePrincipalsToFile(principals); err != nil {
		log.Println("error writing principals to file:", err)
	}
}

func (a *Authenticator) SubscribeEvents(session *xconn.Session, machineID string) error {
	addSubResp := session.Subscribe(fmt.Sprintf("io.xconn.deskconn.desktop.%s.key.add", machineID), a.handleAddKey).Do()
	if addSubResp.Err != nil {
		return addSubResp.Err
	}

	return nil
}
