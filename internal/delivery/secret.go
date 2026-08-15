// Package delivery is the shared "something happened later, tell an
// external party" primitive: an HTTP-callback outbox worker used by both
// deferred/periodic location reports (LRR) and async-completion result
// delivery, which otherwise reduce to the same problem. It's deliberately
// independent of internal/orchestrator's Diameter-facing worker — see
// Worker's own doc comment for why that separation matters, not just for
// package layering.
package delivery

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"errors"
	"fmt"
	"io"
)

// ErrInvalidKey means the configured encryption key isn't a valid AES-256
// key. Checked eagerly wherever a key is used, not just at startup, so a
// misconfiguration fails a specific delivery loudly rather than panicking
// the whole worker.
var ErrInvalidKey = errors.New("delivery: encryption key must be 32 bytes (AES-256)")

// EncryptSecret encrypts a caller-supplied callback secret for storage at
// rest. Unlike client bearer tokens (storage.Client.CredentialHash, a
// one-way hash — the raw token is never needed again, only compared
// against), a callback secret must be recovered in full to compute the
// HMAC-SHA256 delivery signature, so this has to be reversible encryption,
// not hashing. Output is nonce||ciphertext (GCM's standard construction),
// so DecryptSecret needs nothing beyond the key to reverse it.
func EncryptSecret(key, plaintext []byte) ([]byte, error) {
	gcm, err := newGCM(key)
	if err != nil {
		return nil, err
	}
	nonce := make([]byte, gcm.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return nil, fmt.Errorf("delivery: generate nonce: %w", err)
	}
	return gcm.Seal(nonce, nonce, plaintext, nil), nil
}

// DecryptSecret is the inverse of EncryptSecret.
func DecryptSecret(key, ciphertext []byte) ([]byte, error) {
	gcm, err := newGCM(key)
	if err != nil {
		return nil, err
	}
	if len(ciphertext) < gcm.NonceSize() {
		return nil, errors.New("delivery: ciphertext shorter than nonce")
	}
	nonce, ct := ciphertext[:gcm.NonceSize()], ciphertext[gcm.NonceSize():]
	return gcm.Open(nil, nonce, ct, nil)
}

func newGCM(key []byte) (cipher.AEAD, error) {
	if len(key) != 32 {
		return nil, ErrInvalidKey
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	return cipher.NewGCM(block)
}
