package userd

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	log "github.com/sirupsen/logrus"
	bolt "go.etcd.io/bbolt"
	berrors "go.etcd.io/bbolt/errors"

	"github.com/xconnio/deskconn/internal/common"
	"github.com/xconnio/wampproto-go/auth"
	"github.com/xconnio/xconn-go"
)

// StandaloneUser may connect in standalone mode with its password, any of its authorized
// keys, or either, as the user it is with its role: common.RoleAdmin, who may also add and
// remove users, or common.RoleMember, the default. Only the config file's users are admins.
type StandaloneUser struct {
	Username       string   `json:"username"`
	Password       string   `json:"password,omitempty"`
	AuthorizedKeys []string `json:"authorized_keys,omitempty"`
	Role           string   `json:"role,omitempty"`
}

// role is the role user logs in with.
func (user StandaloneUser) role() string {
	if user.Role == common.RoleAdmin {
		return common.RoleAdmin
	}
	return common.RoleMember
}

// StandaloneUserInfo is a user as ProcedureStandaloneUsersList lists it: never its password.
type StandaloneUserInfo struct {
	Username       string   `json:"username"`
	AuthorizedKeys []string `json:"authorized_keys"`
	Password       bool     `json:"password"` // whether it logs in with one
	Role           string   `json:"role"`
	// Config is set for the users of the config file, which only it changes.
	Config bool `json:"config"`
}

// storedUser is a user added through the API, as the database keeps it.
type storedUser struct {
	AuthorizedKeys []string `json:"authorized_keys,omitempty"`
	// PasswordKey is the user's WAMP-CRA key, what clients derive from its password with
	// PBKDF2 and Salt, rather than the password itself.
	PasswordKey string    `json:"password_key,omitempty"`
	Salt        string    `json:"salt,omitempty"`
	Iterations  int       `json:"iterations,omitempty"`
	KeyLength   int       `json:"key_length,omitempty"`
	Added       time.Time `json:"added"`
}

// The database's buckets: the users by username, and their keys, each to its user.
var (
	usersBucket = []byte("users") //nolint:gochecknoglobals // bbolt takes bucket names as bytes
	keysBucket  = []byte("keys")  //nolint:gochecknoglobals // bbolt takes bucket names as bytes
)

const (
	// passwordIterations and passwordKeyLength are how a password is turned into its
	// WAMP-CRA key.
	passwordIterations = 100000
	passwordKeyLength  = 32
	// openTimeout bounds waiting for another process to let go of the database.
	openTimeout = time.Second
)

// ErrStandaloneUserExists and ErrNoStandaloneUser are what adding or removing users fails
// with.
var (
	ErrStandaloneUserExists = errors.New("there's a user by that name already")
	ErrNoStandaloneUser     = errors.New("there's no such user")
)

// StandaloneUsers are who may connect in standalone mode: the config file's, fixed, and
// those added since through the API, kept in a database. It's the standalone server's
// authenticator, so the ones added or removed are let in or kept out from then on.
type StandaloneUsers struct {
	db *bolt.DB

	// The config file's users, by username, and their keys, each to its user.
	configKeys  map[string]string
	configUsers map[string]StandaloneUser
	config      []StandaloneUser
}

// OpenStandaloneUsers opens the database at path, creating it if needed, with the config
// file's users, which must have been checked already (no two with a name or key). It fails
// if the database has a user or key of theirs too, which it would be unclear which to use.
func OpenStandaloneUsers(path string, config []StandaloneUser) (*StandaloneUsers, error) {
	db, err := bolt.Open(path, 0o600, &bolt.Options{Timeout: openTimeout})
	if err != nil {
		if errors.Is(err, berrors.ErrTimeout) {
			return nil, fmt.Errorf("open %s: another desk serve is using it", path)
		}
		return nil, fmt.Errorf("open %s: %w", path, err)
	}
	err = db.Update(func(tx *bolt.Tx) error {
		for _, bucket := range [][]byte{usersBucket, keysBucket} {
			if _, err := tx.CreateBucketIfNotExists(bucket); err != nil {
				return err
			}
		}
		for _, user := range config {
			if tx.Bucket(usersBucket).Get([]byte(user.Username)) != nil {
				return fmt.Errorf("%s is in the config file and was added since too: remove them from one", user.Username)
			}
			for _, key := range user.AuthorizedKeys {
				if owner := tx.Bucket(keysBucket).Get([]byte(strings.ToLower(key))); owner != nil {
					return fmt.Errorf("key %s is %s's in the config file and %s's added since: remove it from one",
						key, user.Username, owner)
				}
			}
		}
		return nil
	})
	if err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("open %s: %w", path, err)
	}

	u := &StandaloneUsers{db: db, configKeys: map[string]string{}, configUsers: map[string]StandaloneUser{},
		config: config}
	for _, user := range config {
		for _, key := range user.AuthorizedKeys {
			u.configKeys[strings.ToLower(key)] = user.Username
		}
		u.configUsers[user.Username] = user
	}
	return u, nil
}

func (u *StandaloneUsers) Close() error {
	return u.db.Close()
}

// Count is how many users there are, the config file's and the database's.
func (u *StandaloneUsers) Count() (int, error) {
	count := len(u.config)
	err := u.db.View(func(tx *bolt.Tx) error {
		count += tx.Bucket(usersBucket).Stats().KeyN
		return nil
	})
	return count, err
}

// Methods are both: users added later may log in either way.
func (u *StandaloneUsers) Methods() []auth.Method {
	return []auth.Method{auth.MethodCryptoSign, auth.WAMPCRA}
}

// Authenticate lets in a client with a user's key, or a user's name and password, as that
// user with its role. A key logs in as the user it's listed for, whatever username its
// holder presents, so that no one passes for another user, an admin say.
func (u *StandaloneUsers) Authenticate(request auth.Request) (auth.Response, error) {
	switch r := request.(type) {
	case *auth.RequestCryptoSign:
		key := strings.ToLower(r.PublicKey())
		if username, ok := u.configKeys[key]; ok {
			return auth.NewResponse(username, u.configUsers[username].role(), 0)
		}
		var username string
		err := u.db.View(func(tx *bolt.Tx) error {
			username = string(tx.Bucket(keysBucket).Get([]byte(key)))
			return nil
		})
		if err != nil {
			return nil, err
		}
		if username == "" {
			return nil, errors.New("unknown publickey")
		}
		return auth.NewResponse(username, common.RoleMember, 0)
	default:
		if request.AuthMethod() != auth.WAMPCRA {
			return nil, fmt.Errorf("unsupported authmethod %s", request.AuthMethod())
		}
		if user, ok := u.configUsers[request.AuthID()]; ok {
			if user.Password == "" {
				return nil, fmt.Errorf("%s logs in with a key", request.AuthID())
			}
			return auth.NewCRAResponse(request.AuthID(), user.role(), user.Password, 0), nil
		}
		user, err := u.stored(request.AuthID())
		if err != nil {
			return nil, err
		}
		if user == nil || user.PasswordKey == "" {
			return nil, fmt.Errorf("unknown authid %s", request.AuthID())
		}
		// The client derives the same key from its password, with the salt it's sent.
		return auth.NewCRAResponseSalted(request.AuthID(), common.RoleMember, user.PasswordKey, user.Salt,
			user.Iterations, user.KeyLength, 0), nil
	}
}

// stored is the database's user named username, or nil if there's none.
func (u *StandaloneUsers) stored(username string) (*storedUser, error) {
	var user *storedUser
	err := u.db.View(func(tx *bolt.Tx) error {
		data := tx.Bucket(usersBucket).Get([]byte(username))
		if data == nil {
			return nil
		}
		user = &storedUser{}
		return json.Unmarshal(data, user)
	})
	return user, err
}

// List lists the users, the config file's first, then the database's by name.
func (u *StandaloneUsers) List() ([]StandaloneUserInfo, error) {
	users := make([]StandaloneUserInfo, 0, len(u.config))
	for _, user := range u.config {
		users = append(users, StandaloneUserInfo{Username: user.Username,
			AuthorizedKeys: append([]string{}, user.AuthorizedKeys...), Password: user.Password != "",
			Role: user.role(), Config: true})
	}
	err := u.db.View(func(tx *bolt.Tx) error {
		return tx.Bucket(usersBucket).ForEach(func(name, data []byte) error {
			var user storedUser
			if err := json.Unmarshal(data, &user); err != nil {
				return err
			}
			users = append(users, StandaloneUserInfo{Username: string(name),
				AuthorizedKeys: append([]string{}, user.AuthorizedKeys...), Password: user.PasswordKey != "",
				Role: common.RoleMember})
			return nil
		})
	})
	return users, err
}

// Add adds user, a member, who may connect from then on.
func (u *StandaloneUsers) Add(user StandaloneUser) error {
	if err := checkStandaloneUser(user); err != nil {
		return err
	}
	if user.Role != "" && user.Role != common.RoleMember {
		return fmt.Errorf("user %q: only the config file's users can be %ss", user.Username, user.Role)
	}
	if slices.ContainsFunc(u.config, func(c StandaloneUser) bool { return c.Username == user.Username }) {
		return ErrStandaloneUserExists
	}
	keys := make([]string, 0, len(user.AuthorizedKeys))
	for _, key := range user.AuthorizedKeys {
		key = strings.ToLower(key)
		// A key logs in as its user: one of two users' would be either.
		if owner, ok := u.configKeys[key]; ok {
			return fmt.Errorf("key %s is already %s's", key, owner)
		}
		if !slices.Contains(keys, key) {
			keys = append(keys, key)
		}
	}

	stored := storedUser{AuthorizedKeys: keys, Added: time.Now().UTC()}
	if user.Password != "" {
		salt := make([]byte, 16)
		if _, err := rand.Read(salt); err != nil {
			return err
		}
		stored.Salt = base64.StdEncoding.EncodeToString(salt)
		stored.Iterations, stored.KeyLength = passwordIterations, passwordKeyLength
		stored.PasswordKey = string(auth.DeriveWAMPCRAKey(stored.Salt, user.Password, stored.Iterations,
			stored.KeyLength))
	}
	data, err := json.Marshal(stored)
	if err != nil {
		return err
	}

	return u.db.Update(func(tx *bolt.Tx) error {
		users, keyOwners := tx.Bucket(usersBucket), tx.Bucket(keysBucket)
		if users.Get([]byte(user.Username)) != nil {
			return ErrStandaloneUserExists
		}
		for _, key := range keys {
			if owner := keyOwners.Get([]byte(key)); owner != nil {
				return fmt.Errorf("key %s is already %s's", key, owner)
			}
			if err := keyOwners.Put([]byte(key), []byte(user.Username)); err != nil {
				return err
			}
		}
		return users.Put([]byte(user.Username), data)
	})
}

// Remove removes the database's user named username, who can't connect from then on. It
// refuses to remove the last user, which would leave no one able to connect.
func (u *StandaloneUsers) Remove(username string) error {
	if slices.ContainsFunc(u.config, func(c StandaloneUser) bool { return c.Username == username }) {
		return fmt.Errorf("%s is in the config file: remove them there", username)
	}
	return u.db.Update(func(tx *bolt.Tx) error {
		if tx.Bucket(usersBucket).Get([]byte(username)) == nil {
			return ErrNoStandaloneUser
		}
		if len(u.config) == 0 && tx.Bucket(usersBucket).Stats().KeyN == 1 {
			return errors.New("that's the last user: no one could connect without them")
		}
		return removeStored(tx, username)
	})
}

// removeStored removes the database's user named username, and its keys.
func removeStored(tx *bolt.Tx, username string) error {
	users := tx.Bucket(usersBucket)
	data := users.Get([]byte(username))
	if data == nil {
		return ErrNoStandaloneUser
	}
	var user storedUser
	if err := json.Unmarshal(data, &user); err != nil {
		return err
	}
	for _, key := range user.AuthorizedKeys {
		if err := tx.Bucket(keysBucket).Delete([]byte(key)); err != nil {
			return err
		}
	}
	return users.Delete([]byte(username))
}

// checkStandaloneUser checks user could log in: a username that WAMP-CRA can carry, and a
// password or valid keys.
func checkStandaloneUser(user StandaloneUser) error {
	switch {
	case user.Username == "":
		return errors.New("a user needs a username")
	case strings.Contains(user.Username, ":"):
		return fmt.Errorf("user %q: a username can't contain ':'", user.Username)
	case user.Password == "" && len(user.AuthorizedKeys) == 0:
		return fmt.Errorf("user %q: needs a password or authorized keys", user.Username)
	}
	for _, key := range user.AuthorizedKeys {
		if !common.IsPublicKey(key) {
			return fmt.Errorf("user %q: key %q isn't a public key (64 hex digits, see desk keygen)",
				user.Username, key)
		}
	}
	return nil
}

// standaloneUsersCall checks inv's caller may manage users, being one of the machine's admins,
// or returns the error to reply with.
func standaloneUsersCall(inv *xconn.Invocation) *xconn.InvocationResult {
	if inv.CallerAuthRole() != common.RoleAdmin {
		return xconn.NewInvocationError(common.ErrNotAuthorized, "only the machine's admins may manage its users")
	}
	return nil
}

// standaloneUserFrom is the user an add call's kwargs describe: username, password, and
// authorized_keys, a list of keys or a single one.
func standaloneUserFrom(inv *xconn.Invocation) (StandaloneUser, error) {
	var user StandaloneUser
	for name, value := range inv.Kwargs() {
		var ok bool
		switch name {
		case "username":
			user.Username, ok = value.(string)
		case "password":
			user.Password, ok = value.(string)
		case "role":
			user.Role, ok = value.(string)
		case "authorized_keys":
			user.AuthorizedKeys, ok = standaloneKeysFrom(value)
		default:
			return user, fmt.Errorf("unknown field %q: want username, password or authorized_keys", name)
		}
		if !ok {
			return user, fmt.Errorf("invalid %s %v", name, value)
		}
	}
	return user, nil
}

func standaloneKeysFrom(value any) ([]string, bool) {
	if key, ok := value.(string); ok {
		return []string{key}, true
	}
	list, ok := value.([]any)
	if !ok {
		return nil, false
	}
	keys := make([]string, 0, len(list))
	for _, item := range list {
		key, ok := item.(string)
		if !ok {
			return nil, false
		}
		keys = append(keys, key)
	}
	return keys, true
}

// handleStandaloneUsersList returns the machine's users, each a dict of its username,
// authorized_keys, password (whether it logs in with one), role and config (whether it's
// one of the config file's).
func (d *Deskconn) handleStandaloneUsersList(_ context.Context, inv *xconn.Invocation) *xconn.InvocationResult {
	if errResult := standaloneUsersCall(inv); errResult != nil {
		return errResult
	}
	users, err := d.standaloneUsers.List()
	if err != nil {
		return xconn.NewInvocationError(common.ErrOperationFailed, err.Error())
	}
	result := make([]any, 0, len(users))
	for _, user := range users {
		keys := make([]any, 0, len(user.AuthorizedKeys))
		for _, key := range user.AuthorizedKeys {
			keys = append(keys, key)
		}
		result = append(result, map[string]any{
			"username": user.Username, "authorized_keys": keys, "password": user.Password,
			"role": user.Role, "config": user.Config,
		})
	}
	return xconn.NewInvocationResult(result)
}

// handleStandaloneUsersAdd adds the user its kwargs describe (see standaloneUserFrom), as a
// member.
func (d *Deskconn) handleStandaloneUsersAdd(_ context.Context, inv *xconn.Invocation) *xconn.InvocationResult {
	if errResult := standaloneUsersCall(inv); errResult != nil {
		return errResult
	}
	user, err := standaloneUserFrom(inv)
	if err == nil {
		err = d.standaloneUsers.Add(user)
	}
	if err != nil {
		return xconn.NewInvocationError(common.ErrInvalidArgument, err.Error())
	}
	log.Printf("standalone: %s added %s", inv.CallerAuthID(), user.Username)
	return xconn.NewInvocationResult()
}

// handleStandaloneUsersRemove removes the user its argument names.
func (d *Deskconn) handleStandaloneUsersRemove(_ context.Context, inv *xconn.Invocation) *xconn.InvocationResult {
	if errResult := standaloneUsersCall(inv); errResult != nil {
		return errResult
	}
	username, err := inv.ArgString(0)
	if err == nil {
		err = d.standaloneUsers.Remove(username)
	}
	if err != nil {
		return xconn.NewInvocationError(common.ErrInvalidArgument, err.Error())
	}
	log.Printf("standalone: %s removed %s", inv.CallerAuthID(), username)
	return xconn.NewInvocationResult()
}
