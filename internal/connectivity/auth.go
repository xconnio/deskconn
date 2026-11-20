package connectivity

import (
	"encoding/json"
	"fmt"
	"maps"
	"os"
	"slices"
	"sync"
	"time"

	api "github.com/xconnio/xconn/bindings/go"
	"github.com/xconnio/xconn/bindings/go/auth"
)

type CryptosignPrincipal struct {
	AuthID         string   `json:"authid"`
	AuthorizedKeys []string `json:"authorized_keys"`
	AuthRole       string   `json:"authrole"`
	// KeyExpiresAt maps the keys that expire (login keys) to when, in Unix seconds.
	KeyExpiresAt map[string]int64 `json:"key_expires_at,omitempty"`
}

// Authenticator owns a synchronized snapshot of the cloud-authorized keys.
type Authenticator struct {
	mu         sync.Mutex
	principals map[string]CryptosignPrincipal
	path       string
}

func NewAuthenticator(principals []*CryptosignPrincipal, path string) *Authenticator {
	policy := &Authenticator{path: path}
	policy.SetPrincipals(principals)
	return policy
}

func (a *Authenticator) Methods() []auth.Method { return []auth.Method{auth.MethodCryptoSign} }

func (a *Authenticator) Authenticate(request auth.Request) (auth.Response, error) {
	cryptosign, ok := request.(*auth.RequestCryptoSign)
	if !ok {
		return nil, fmt.Errorf("cryptosign authentication required")
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	principal, exists := a.principals[request.AuthID()]
	if !exists || !slices.Contains(principal.AuthorizedKeys, cryptosign.PublicKey()) {
		return nil, fmt.Errorf("unknown identity or public key")
	}
	if expiresAt, expires := principal.KeyExpiresAt[cryptosign.PublicKey()]; expires && time.Now().Unix() >= expiresAt {
		return nil, fmt.Errorf("public key expired")
	}
	return auth.NewResponse(principal.AuthID, principal.AuthRole, 0)
}

func (a *Authenticator) SetPrincipals(principals []*CryptosignPrincipal) {
	snapshot := make(map[string]CryptosignPrincipal, len(principals))
	for _, principal := range principals {
		if principal == nil {
			continue
		}
		copy := *principal
		copy.AuthorizedKeys = slices.Clone(principal.AuthorizedKeys)
		copy.KeyExpiresAt = maps.Clone(principal.KeyExpiresAt)
		snapshot[copy.AuthID] = copy
	}
	a.mu.Lock()
	a.principals = snapshot
	a.mu.Unlock()
}

func ReadPrincipalsFromFile(path string) ([]*CryptosignPrincipal, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var principals []*CryptosignPrincipal
	err = json.Unmarshal(data, &principals)
	return principals, err
}

func WritePrincipalsToFile(path string, principals []*CryptosignPrincipal) error {
	return writePrivateJSON(path, principals)
}

func (a *Authenticator) persistLocked() error {
	if a.path == "" {
		return nil
	}
	principals := make([]CryptosignPrincipal, 0, len(a.principals))
	for _, principal := range a.principals {
		principals = append(principals, principal)
	}
	slices.SortFunc(principals, func(left, right CryptosignPrincipal) int {
		if left.AuthID < right.AuthID {
			return -1
		}
		if left.AuthID > right.AuthID {
			return 1
		}
		return 0
	})
	return writePrivateJSON(a.path, principals)
}

func (a *Authenticator) addKey(event *api.Event) error {
	id, err := event.ArgString(0)
	if err != nil {
		return err
	}
	key, err := event.ArgString(1)
	if err != nil {
		return err
	}
	role, err := event.ArgString(2)
	if err != nil {
		return err
	}
	// Login keys come with their expiry; other keys, and keys from older account services, don't.
	var expiresAt int64
	expires := event.ArgsLen() > 3
	if expires {
		if expiresAt, err = event.ArgInt64(3); err != nil {
			return err
		}
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	principal := a.principals[id]
	principal.AuthID, principal.AuthRole = id, role
	known := slices.Contains(principal.AuthorizedKeys, key)
	if !known {
		principal.AuthorizedKeys = append(principal.AuthorizedKeys, key)
	}
	// As in the account service, a key added without an expiry never expires, and one added
	// twice with an expiry expires last.
	previous, expiring := principal.KeyExpiresAt[key]
	switch {
	case !expires:
		delete(principal.KeyExpiresAt, key)
	case !known || expiring:
		if principal.KeyExpiresAt == nil {
			principal.KeyExpiresAt = map[string]int64{}
		}
		principal.KeyExpiresAt[key] = max(previous, expiresAt)
	}
	a.principals[id] = principal
	return a.persistLocked()
}

func (a *Authenticator) removeKeys(event *api.Event) error {
	var removed map[string][]string
	value, err := event.ArgDict(0)
	if err != nil {
		return err
	}
	if err := value.Decode(&removed); err != nil {
		return err
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	for id, keys := range removed {
		principal, exists := a.principals[id]
		if !exists {
			continue
		}
		principal.AuthorizedKeys = slices.DeleteFunc(
			principal.AuthorizedKeys,
			func(key string) bool { return slices.Contains(keys, key) },
		)
		for _, key := range keys {
			delete(principal.KeyExpiresAt, key)
		}
		if len(principal.AuthorizedKeys) == 0 {
			delete(a.principals, id)
		} else {
			a.principals[id] = principal
		}
	}
	return a.persistLocked()
}
