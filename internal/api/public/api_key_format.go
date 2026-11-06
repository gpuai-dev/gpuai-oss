package public

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
)

// KeyPrefixLive is the live-environment API key prefix. (Q-2 lock from
// 18-RESEARCH.md.) Reserved KeyPrefixTest will be honored once a sandbox
// environment ships in v2; for now any "gpuai_test_" key is rejected as
// invalid_api_key by the parser.
const (
	KeyPrefixLive = "gpuai_live_"
	KeyPrefixTest = "gpuai_test_"
	keyBodyLen    = 24
)

// base62Alphabet is the URL-safe alphabet used for the random body.
const base62Alphabet = "0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz"

// ErrUnsupportedKeyEnvironment is returned by ParseRawKey for a "gpuai_test_"
// prefix when the test environment is not yet enabled.
var ErrUnsupportedKeyEnvironment = errors.New("test API keys not yet supported")

// ErrMalformedKey is returned by ParseRawKey for any unrecognized prefix.
var ErrMalformedKey = errors.New("malformed API key")

// GenerateAPIKey mints a fresh live-environment API key.
// Returns:
//
//	raw    — the full key shown to the caller exactly once (gpuai_live_<24 chars>).
//	hash   — SHA-256 hex digest of raw, stored in api_keys.key_hash.
//	prefix — raw[:len(KeyPrefixLive)+4]; safe to log or display in dashboard list views.
//	last4  — last 4 chars of raw; for ID-in-list display.
func GenerateAPIKey() (raw, hash, prefix, last4 string, err error) {
	body, err := randomBase62(keyBodyLen)
	if err != nil {
		return "", "", "", "", err
	}
	raw = KeyPrefixLive + body
	sum := sha256.Sum256([]byte(raw))
	hash = hex.EncodeToString(sum[:])
	prefix = raw[:len(KeyPrefixLive)+4]
	last4 = raw[len(raw)-4:]
	return raw, hash, prefix, last4, nil
}

// HashKey returns the canonical SHA-256 hex digest of raw used for DB lookup.
func HashKey(raw string) string {
	sum := sha256.Sum256([]byte(raw))
	return hex.EncodeToString(sum[:])
}

// ParseRawKey validates the raw key prefix.
// Returns ErrMalformedKey for unrecognized prefixes, ErrUnsupportedKeyEnvironment
// for reserved gpuai_test_ until sandbox ships.
func ParseRawKey(raw string) error {
	switch {
	case len(raw) < len(KeyPrefixLive)+keyBodyLen:
		return ErrMalformedKey
	case raw[:len(KeyPrefixLive)] == KeyPrefixLive:
		return nil
	case len(raw) >= len(KeyPrefixTest) && raw[:len(KeyPrefixTest)] == KeyPrefixTest:
		return ErrUnsupportedKeyEnvironment
	default:
		return ErrMalformedKey
	}
}

// randomBase62 returns n base62 chars sourced from crypto/rand.
// Uses reject-sampling to avoid modulo bias: uniform draws from 0..255 are
// only accepted when below 248 (= 4*62), then mapped to base62Alphabet.
func randomBase62(n int) (string, error) {
	out := make([]byte, n)
	for i := 0; i < n; i++ {
		var b [1]byte
		for {
			if _, err := rand.Read(b[:]); err != nil {
				return "", err
			}
			if b[0] < 248 {
				out[i] = base62Alphabet[int(b[0])%62]
				break
			}
		}
	}
	return string(out), nil
}
