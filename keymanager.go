package deskconn

import "sync"

type keyManager struct {
	keys map[uint64]*shellEncryption
	sync.Mutex
}

func newKeyManager() *keyManager {
	return &keyManager{
		keys: make(map[uint64]*shellEncryption),
	}
}

func (k *keyManager) store(sessionID uint64, enc *shellEncryption) {
	k.Lock()
	defer k.Unlock()
	k.keys[sessionID] = enc
}

func (k *keyManager) fetch(sessionID uint64) (*shellEncryption, bool) {
	k.Lock()
	defer k.Unlock()
	enc, ok := k.keys[sessionID]
	return enc, ok
}

func (k *keyManager) delete(sessionID uint64) {
	k.Lock()
	defer k.Unlock()
	delete(k.keys, sessionID)
}
