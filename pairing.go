package deskconn

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	log "github.com/sirupsen/logrus"
	"golang.org/x/crypto/ssh"

	"github.com/xconnio/xconn-go"
)

const (
	ProcedurePair        = "io.xconn.deskconnd.pair"
	ProcedureKeyExchange = "exchange.public.keys"
	ProcedureStartPair   = "io.xconn.deskconnd.start_pairing"

	activePairFile     = "deskconn_active_pair.json"
	pairedDevicesDB    = "deskconn_paired_devices.json"
	DeskconnDir        = ".deskconn"
	DesktopPrivKey     = "id_ed25519"
	DesktopPubKey      = "id_ed25519.pub"
	AuthorizedKeysFile = "authorized_keys"
)

type ActivePair struct {
	Code      string    `json:"code"`
	Expiry    time.Time `json:"expiry"`
	SessionID string    `json:"session_id,omitempty"`
}

type PairedDevice struct {
	ID        string    `json:"id"`
	Label     string    `json:"label"`
	LastSeen  time.Time `json:"last_seen"`
	PublicKey string    `json:"public_key,omitempty"`
	PairedAt  time.Time `json:"paired_at"`
}

type PairManager struct {
	mu              sync.Mutex
	activePairings  map[string]*ActivePair
	pairingSessions map[string]*PairingSession
}

type PairingSession struct {
	Code       string
	Expiry     time.Time
	DeviceID   string
	Completed  bool
	PublicKey  string
	DesktopKey string
	Session    *xconn.Session
}

func NewPairManager() *PairManager {
	return &PairManager{
		activePairings:  make(map[string]*ActivePair),
		pairingSessions: make(map[string]*PairingSession),
	}
}

func deskconnPath(file string) (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	dir := filepath.Join(home, DeskconnDir)
	return filepath.Join(dir, file), nil
}

func ensureDesktopKeys() (string, error) {
	pubPath, err := deskconnPath(DesktopPubKey)
	if err != nil {
		return "", err
	}

	if _, err := os.Stat(pubPath); os.IsNotExist(err) {
		publicKey, privateKey, err := ed25519.GenerateKey(rand.Reader)
		if err != nil {
			return "", err
		}

		privPath, _ := deskconnPath(DesktopPrivKey)
		privData := make([]byte, ed25519.PrivateKeySize)
		copy(privData[:ed25519.SeedSize], privateKey.Seed())
		copy(privData[ed25519.SeedSize:], publicKey)
		_ = os.WriteFile(privPath, privData, 0600)

		sshPub, _ := ssh.NewPublicKey(publicKey)
		_ = os.WriteFile(pubPath, ssh.MarshalAuthorizedKey(sshPub), 0600)

		authPath, _ := deskconnPath(AuthorizedKeysFile)
		_ = os.WriteFile(authPath, []byte{}, 0600)

	}

	data, err := os.ReadFile(pubPath)
	if err != nil {
		return "", err
	}
	lines := strings.Fields(string(data))
	if len(lines) >= 2 {
		return extractRawKeyFromSSH(lines[1])
	}
	return "", fmt.Errorf("invalid public key format")
}

func (pm *PairManager) StartPairing() string {
	pm.mu.Lock()
	defer pm.mu.Unlock()

	digits := "0123456789"
	codeBytes := make([]byte, 6)
	_, _ = rand.Read(codeBytes)
	for i := range codeBytes {
		codeBytes[i] = digits[int(codeBytes[i])%10]
	}
	code := string(codeBytes)

	pm.activePairings[code] = &ActivePair{
		Code:   code,
		Expiry: time.Now().Add(3 * time.Minute),
	}

	ap := ActivePair{
		Code:   code,
		Expiry: time.Now().Add(3 * time.Minute),
	}
	raw, _ := json.MarshalIndent(ap, "", "  ")
	path, _ := deskconnPath(activePairFile)
	_ = os.WriteFile(path, raw, 0600)

	log.WithField("code", code).Info("Started new pairing session")
	return code
}

func (pm *PairManager) ValidatePairing(deviceID, code, label string) (bool, string) {
	pm.mu.Lock()
	defer pm.mu.Unlock()

	ap, exists := pm.activePairings[code]
	if !exists {
		path, err := deskconnPath(activePairFile)
		if err != nil {
			return false, "pairing not found"
		}

		data, err := os.ReadFile(path)
		if err != nil {
			return false, "pairing not found"
		}

		var fileAP ActivePair
		_ = json.Unmarshal(data, &fileAP)

		if fileAP.Code != code {
			return false, "invalid code"
		}

		if time.Now().After(fileAP.Expiry) {
			os.Remove(path)
			return false, "code expired"
		}

		ap = &fileAP
	}

	if time.Now().After(ap.Expiry) {
		delete(pm.activePairings, code)
		path, _ := deskconnPath(activePairFile)
		os.Remove(path)
		return false, "code expired"
	}

	sessionID := fmt.Sprintf("%s-%d", deviceID, time.Now().Unix())
	pm.pairingSessions[sessionID] = &PairingSession{
		Code:      code,
		Expiry:    ap.Expiry,
		DeviceID:  deviceID,
		Completed: false,
	}

	devicesPath, _ := deskconnPath(pairedDevicesDB)
	var devices []PairedDevice
	if data, err := os.ReadFile(devicesPath); err == nil {
		_ = json.Unmarshal(data, &devices)
	}

	devices = append(devices, PairedDevice{
		ID:       deviceID,
		Label:    label,
		LastSeen: time.Now(),
		PairedAt: time.Now(),
	})

	data, _ := json.MarshalIndent(devices, "", "  ")
	_ = os.WriteFile(devicesPath, data, 0600)

	delete(pm.activePairings, code)
	path, _ := deskconnPath(activePairFile)
	os.Remove(path)

	log.WithFields(log.Fields{
		"device": deviceID,
		"code":   code,
	}).Info("Pairing validated successfully")

	return true, sessionID
}

func (pm *PairManager) ExchangeKeys(sessionID, deviceID, mobilePubB64 string) (string, bool, error) {
	pm.mu.Lock()
	defer pm.mu.Unlock()

	session, exists := pm.pairingSessions[sessionID]
	if !exists {
		return "", false, fmt.Errorf("pairing session not found")
	}

	if session.Completed {
		return "", false, fmt.Errorf("pairing already completed")
	}

	if session.DeviceID != deviceID {
		return "", false, fmt.Errorf("device ID mismatch")
	}

	mobilePubBytes, err := base64.StdEncoding.DecodeString(mobilePubB64)
	if err != nil {
		return "", false, fmt.Errorf("invalid base64: %w", err)
	}

	if len(mobilePubBytes) != ed25519.PublicKeySize {
		return "", false, fmt.Errorf("invalid public key size")
	}

	mobilePub := ed25519.PublicKey(mobilePubBytes)
	sshPub, err := ssh.NewPublicKey(mobilePub)
	if err != nil {
		return "", false, fmt.Errorf("invalid public key: %w", err)
	}

	authPath, _ := deskconnPath(AuthorizedKeysFile)
	authLine := ssh.MarshalAuthorizedKey(sshPub)
	line := strings.TrimSpace(string(authLine)) + " " + deviceID + "\n"

	f, err := os.OpenFile(authPath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0600)
	if err != nil {
		return "", false, fmt.Errorf("failed to save key: %w", err)
	}
	defer f.Close()

	_, _ = f.WriteString(line)

	desktopPubB64, err := ensureDesktopKeys()
	if err != nil {
		return "", false, fmt.Errorf("failed to get desktop key: %w", err)
	}

	session.Completed = true
	session.PublicKey = mobilePubB64
	session.DesktopKey = desktopPubB64

	go func() {
		time.Sleep(30 * time.Second)
		pm.mu.Lock()
		delete(pm.pairingSessions, sessionID)
		pm.mu.Unlock()
		log.WithField("session", sessionID).Info("Cleaned up pairing session")
	}()

	log.WithFields(log.Fields{
		"device":  deviceID,
		"session": sessionID,
	}).Info("Key exchange completed successfully")

	return desktopPubB64, true, nil
}

func (pm *PairManager) Register(sess *xconn.Session) {
	sess.Register(ProcedureStartPair,
		func(_ context.Context, inv *xconn.Invocation) *xconn.InvocationResult {
			code := pm.StartPairing()

			return xconn.NewInvocationResult(code)
		},
	).Do()

	sess.Register(ProcedurePair,
		func(_ context.Context, inv *xconn.Invocation) *xconn.InvocationResult {
			deviceID, _ := inv.ArgString(0)
			code, _ := inv.ArgString(1)
			label := "Mobile Device"
			if inv.ArgsLen() > 2 {
				label, _ = inv.ArgString(2)
			}

			ok, sessionID := pm.ValidatePairing(deviceID, code, label)
			if !ok {
				return xconn.NewInvocationResult(false, "pairing failed")
			}
			return xconn.NewInvocationResult(true, sessionID)
		},
	).Do()

	sess.Register(ProcedureKeyExchange,
		func(_ context.Context, inv *xconn.Invocation) *xconn.InvocationResult {
			if inv.ArgsLen() < 3 {
				return xconn.NewInvocationResult(false, "missing arguments")
			}

			sessionID, _ := inv.ArgString(0)
			deviceID, _ := inv.ArgString(1)
			mobilePubB64, _ := inv.ArgString(2)

			desktopPubB64, ok, err := pm.ExchangeKeys(sessionID, deviceID, mobilePubB64)
			if err != nil {
				return xconn.NewInvocationResult(false, err.Error())
			}

			if !ok {
				return xconn.NewInvocationResult(false, "key exchange failed")
			}

			return xconn.NewInvocationResult(true, desktopPubB64)
		},
	).Do()

}

func extractRawKeyFromSSH(sshB64 string) (string, error) {
	data, err := base64.StdEncoding.DecodeString(sshB64)
	if err != nil {
		return "", err
	}

	if len(data) < 4 {
		return "", fmt.Errorf("invalid SSH key")
	}

	typeLen := int(data[0])<<24 | int(data[1])<<16 | int(data[2])<<8 | int(data[3])
	if len(data) < 4+typeLen+4 {
		return "", fmt.Errorf("invalid SSH key length")
	}

	keyOffset := 4 + typeLen
	keyLen := int(data[keyOffset])<<24 | int(data[keyOffset+1])<<16 |
		int(data[keyOffset+2])<<8 | int(data[keyOffset+3])

	if len(data) < keyOffset+4+keyLen {
		return "", fmt.Errorf("invalid SSH key length")
	}

	rawKey := data[keyOffset+4 : keyOffset+4+keyLen]
	return base64.StdEncoding.EncodeToString(rawKey), nil
}
