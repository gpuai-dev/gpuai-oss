// Package registrycred is the at-rest sealing primitive for private-registry
// credentials (custom images Phase 2) — the platform's FIRST at-rest
// encryption primitive (everything stored before this was either a hash or an
// HMAC secret whose exposure doesn't compromise a third-party account).
//
// A registry password is a CUSTOMER'S credential to an external system, so it
// is sealed with AES-256-GCM under a platform key before it touches Postgres:
// a database snapshot, a stray SELECT, or a leaked dump yields nonce||ciphertext
// that is useless without REGISTRY_CREDENTIALS_KEY (which lives only in the
// service environment). The key is 32 bytes,
// base64 (std or url) encoded in the env var.
//
// No key => Enabled() is false and the credential API surfaces 503 — the
// feature ships dark until the environment is provisioned, with no boot-order
// dependency.
package registrycred

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
)

// NewID returns a random v4 UUID string. Generated app-side (not by the DB
// default) because Seal binds the credential id as AAD — the id must exist
// before the password can be sealed. crypto/rand only; no uuid dependency.
func NewID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic(err) // crypto/rand failure is unrecoverable
	}
	b[6] = (b[6] & 0x0f) | 0x40 // version 4
	b[8] = (b[8] & 0x3f) | 0x80 // RFC 4122 variant
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

// Sealer seals and opens registry passwords. Zero value = disabled.
type Sealer struct {
	aead cipher.AEAD
}

// ErrDisabled is returned when no key is configured.
var ErrDisabled = errors.New("registry credentials are not enabled on this environment")

// NewSealer builds a Sealer from the base64-encoded 32-byte key. An empty key
// returns a DISABLED sealer (not an error) — absence is a valid deployment
// state; a present-but-malformed key is a hard error so a typo can't silently
// dark the feature.
func NewSealer(b64Key string) (*Sealer, error) {
	if b64Key == "" {
		return &Sealer{}, nil
	}
	key, err := base64.StdEncoding.DecodeString(b64Key)
	if err != nil {
		if key, err = base64.URLEncoding.DecodeString(b64Key); err != nil {
			return nil, fmt.Errorf("registrycred: key is not valid base64")
		}
	}
	if len(key) != 32 {
		return nil, fmt.Errorf("registrycred: key must decode to 32 bytes, got %d", len(key))
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, fmt.Errorf("registrycred: %w", err)
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("registrycred: %w", err)
	}
	return &Sealer{aead: aead}, nil
}

// Enabled reports whether a key is configured.
func (s *Sealer) Enabled() bool { return s != nil && s.aead != nil }

// Seal encrypts a password into nonce||ciphertext. The credential id is bound
// as AAD so a sealed blob copied onto another row fails to open.
func (s *Sealer) Seal(credentialID, password string) ([]byte, error) {
	if !s.Enabled() {
		return nil, ErrDisabled
	}
	nonce := make([]byte, s.aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return nil, err
	}
	return s.aead.Seal(nonce, nonce, []byte(password), []byte(credentialID)), nil
}

// Open decrypts a Seal-produced blob.
func (s *Sealer) Open(credentialID string, blob []byte) (string, error) {
	if !s.Enabled() {
		return "", ErrDisabled
	}
	ns := s.aead.NonceSize()
	if len(blob) < ns+1 {
		return "", errors.New("registrycred: sealed blob too short")
	}
	pt, err := s.aead.Open(nil, blob[:ns], blob[ns:], []byte(credentialID))
	if err != nil {
		return "", errors.New("registrycred: decryption failed")
	}
	return string(pt), nil
}
