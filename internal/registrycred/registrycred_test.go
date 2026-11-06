package registrycred

import (
	"crypto/rand"
	"encoding/base64"
	"testing"
)

func testKey(t *testing.T) string {
	t.Helper()
	k := make([]byte, 32)
	if _, err := rand.Read(k); err != nil {
		t.Fatal(err)
	}
	return base64.StdEncoding.EncodeToString(k)
}

func TestSealOpenRoundTrip(t *testing.T) {
	s, err := NewSealer(testKey(t))
	if err != nil {
		t.Fatal(err)
	}
	blob, err := s.Seal("cred-1", "hunter2-token")
	if err != nil {
		t.Fatal(err)
	}
	got, err := s.Open("cred-1", blob)
	if err != nil {
		t.Fatal(err)
	}
	if got != "hunter2-token" {
		t.Fatalf("round trip = %q", got)
	}
	// AAD binding: the same blob under a different credential id must fail.
	if _, err := s.Open("cred-2", blob); err == nil {
		t.Fatal("blob opened under wrong credential id")
	}
	// Tamper: flip a ciphertext byte.
	blob[len(blob)-1] ^= 1
	if _, err := s.Open("cred-1", blob); err == nil {
		t.Fatal("tampered blob opened")
	}
}

func TestDisabledAndBadKeys(t *testing.T) {
	s, err := NewSealer("")
	if err != nil {
		t.Fatalf("empty key must mean disabled, not error: %v", err)
	}
	if s.Enabled() {
		t.Fatal("empty key sealer reports enabled")
	}
	if _, err := s.Seal("c", "p"); err != ErrDisabled {
		t.Fatalf("disabled Seal err = %v", err)
	}
	if _, err := NewSealer("not-base64!!"); err == nil {
		t.Fatal("malformed key accepted")
	}
	if _, err := NewSealer(base64.StdEncoding.EncodeToString(make([]byte, 16))); err == nil {
		t.Fatal("16-byte key accepted")
	}
}
