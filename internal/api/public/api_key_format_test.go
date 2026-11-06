package public

import (
	"errors"
	"regexp"
	"strings"
	"testing"
)

func TestGenerateAPIKey_Shape(t *testing.T) {
	raw, hash, prefix, last4, err := GenerateAPIKey()
	if err != nil {
		t.Fatalf("generate: %v", err)
	}
	if !strings.HasPrefix(raw, "gpuai_live_") {
		t.Errorf("raw prefix: got %q", raw[:11])
	}
	if len(raw) != len(KeyPrefixLive)+24 {
		t.Errorf("raw len: got %d, want %d", len(raw), len(KeyPrefixLive)+24)
	}
	if len(hash) != 64 {
		t.Errorf("hash len: got %d, want 64", len(hash))
	}
	if !regexp.MustCompile(`^[0-9a-f]{64}$`).MatchString(hash) {
		t.Errorf("hash not lowercase hex: %s", hash)
	}
	if prefix != raw[:len(KeyPrefixLive)+4] {
		t.Errorf("prefix: got %q, want first 15 chars of raw", prefix)
	}
	if last4 != raw[len(raw)-4:] {
		t.Errorf("last4: got %q, want last 4 chars of raw", last4)
	}
}

func TestGenerateAPIKey_Uniqueness(t *testing.T) {
	seen := map[string]bool{}
	for i := 0; i < 100; i++ {
		raw, _, _, _, err := GenerateAPIKey()
		if err != nil {
			t.Fatalf("iter %d: %v", i, err)
		}
		if seen[raw] {
			t.Fatalf("collision at iter %d: %q", i, raw)
		}
		seen[raw] = true
	}
}

func TestParseRawKey(t *testing.T) {
	cases := []struct {
		in   string
		want error
	}{
		{"gpuai_live_" + strings.Repeat("a", 24), nil},
		{"gpuai_test_" + strings.Repeat("a", 24), ErrUnsupportedKeyEnvironment},
		{"unknownprefix_xxxxxxxxxxxxxxxxxxxxxxxxx", ErrMalformedKey},
		{"too-short", ErrMalformedKey},
		{"", ErrMalformedKey},
	}
	for _, c := range cases {
		err := ParseRawKey(c.in)
		if !errors.Is(err, c.want) {
			t.Errorf("ParseRawKey(%q): got %v, want %v", c.in, err, c.want)
		}
	}
}

func TestHashKey_Stable(t *testing.T) {
	h1 := HashKey("gpuai_live_test_static")
	h2 := HashKey("gpuai_live_test_static")
	if h1 != h2 {
		t.Fatalf("HashKey not deterministic: %q vs %q", h1, h2)
	}
}
