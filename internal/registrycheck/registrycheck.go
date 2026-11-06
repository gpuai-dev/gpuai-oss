// Package registrycheck is the pre-flight pullability check for customer
// images (Phase 1 of custom images). Before any money is spent on a GPU, the
// create-instance handler asks this client whether the image actually exists,
// is anonymously pullable, and runs on linux/amd64 — and captures the image
// config's Entrypoint/Cmd so the bootstrap can start the customer's workload
// (the SSH contract: our bootstrap owns PID 1, then launches the image's own
// entrypoint).
//
// The client speaks the OCI distribution HTTP API directly: anonymous bearer
// token via the WWW-Authenticate challenge (Docker Hub, GHCR, quay, nvcr all
// implement it), manifest GET with list/index resolution to linux/amd64, then
// the config blob. Every response read is size-capped and the transport
// refuses to dial private/loopback/link-local addresses — the registry host is
// customer-controlled, so this is the dynamic half of the SSRF guard
// (imageref.Parse is the static half).
package registrycheck

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"syscall"
	"time"

	"github.com/gpuai-dev/gpuai-oss/internal/imageref"
)

// Typed, customer-safe errors. The handler maps these onto 422s (and the
// unreachable case onto a 502) with the message verbatim.
var (
	ErrNotFound = errors.New("image not found in registry (check the name and tag)")
	// Docker Hub answers 401 for repositories that do not exist at all, so
	// this error deliberately covers both "private" and "no such repo".
	ErrPrivate     = errors.New("image is private or does not exist (for a private image, pass registry_credential_id with a stored registry credential)")
	ErrPlatform    = errors.New("image has no linux/amd64 variant (GPU instances require linux/amd64 images)")
	ErrUnreachable = errors.New("registry could not be reached to verify the image")
	// ErrAuthFailed replaces ErrPrivate when CREDENTIALS were supplied and the
	// registry still refused — the login is wrong, not the image's visibility.
	ErrAuthFailed = errors.New("registry authentication failed (check the credential's username and password)")
)

const (
	maxManifestBytes = 4 << 20 // manifest lists on big multi-arch repos run large
	maxConfigBytes   = 4 << 20
	requestTimeout   = 15 * time.Second
	// overallTimeout bounds the WHOLE pre-flight (token + up to two manifests
	// + config blob), so the synchronous create handler is never held for
	// 4 × requestTimeout by a hostile slow registry.
	overallTimeout = 20 * time.Second
)

// Result is what the pre-flight learned about the image.
type Result struct {
	// Digest is the registry's digest for the TOP-LEVEL manifest the tag
	// resolves to (the list digest for multi-arch images) — suitable for
	// digest-pinning the launch so it runs exactly the validated bytes.
	Digest string
	// Entrypoint and Cmd are the image config's process defaults, captured so
	// the bootstrap can exec the image's own workload after tunnel/sshd setup.
	Entrypoint []string
	Cmd        []string
}

// Checker performs pre-flight checks. Zero value is not usable; call New.
type Checker struct {
	client *http.Client
	// scheme + hubHost are test seams: production always uses https and the
	// canonical Docker Hub registry endpoint.
	scheme  string
	hostmap func(registry string) string
}

// Option customizes a Checker (test seams only).
type Option func(*Checker)

// WithTestTransport points the checker at a plain-HTTP test server and lifts
// the private-address dial guard. Tests only.
func WithTestTransport(hostOverride func(registry string) string) Option {
	return func(c *Checker) {
		c.scheme = "http"
		c.hostmap = hostOverride
		c.client = &http.Client{Timeout: requestTimeout}
	}
}

// New builds a production Checker with the SSRF-guarded transport.
func New(opts ...Option) *Checker {
	dialer := &net.Dialer{
		Timeout: 5 * time.Second,
		Control: func(network, address string, _ syscall.RawConn) error {
			host, _, err := net.SplitHostPort(address)
			if err != nil {
				return err
			}
			ip := net.ParseIP(host)
			if ip == nil || imageref.DisallowedRegistryIP(ip) {
				return fmt.Errorf("registry address %s is not publicly routable", host)
			}
			return nil
		},
	}
	c := &Checker{
		scheme: "https",
		hostmap: func(registry string) string {
			if registry == "docker.io" {
				return "registry-1.docker.io"
			}
			return registry
		},
		client: &http.Client{
			Timeout: requestTimeout,
			Transport: &http.Transport{
				DialContext:         dialer.DialContext,
				MaxIdleConns:        4,
				TLSHandshakeTimeout: 5 * time.Second,
			},
		},
	}
	for _, o := range opts {
		o(c)
	}
	return c
}

const acceptManifests = "application/vnd.docker.distribution.manifest.list.v2+json, " +
	"application/vnd.oci.image.index.v1+json, " +
	"application/vnd.docker.distribution.manifest.v2+json, " +
	"application/vnd.oci.image.manifest.v1+json"

type manifestEnvelope struct {
	MediaType string `json:"mediaType"`
	Manifests []struct {
		Digest   string `json:"digest"`
		Platform struct {
			Architecture string `json:"architecture"`
			OS           string `json:"os"`
		} `json:"platform"`
	} `json:"manifests"`
	Config struct {
		Digest string `json:"digest"`
	} `json:"config"`
}

type imageConfig struct {
	Architecture string `json:"architecture"`
	OS           string `json:"os"`
	Config       struct {
		Entrypoint []string `json:"Entrypoint"`
		Cmd        []string `json:"Cmd"`
	} `json:"config"`
}

// Credentials is a private-registry login for an authenticated pre-flight
// (custom images Phase 2). The password is a live secret: it exists only in
// memory on the launch path and must never be logged or persisted by callers.
type Credentials struct {
	Username string
	Password string
}

// Check verifies ref is pullable for linux/amd64 and returns the resolved
// digest plus the image's Entrypoint/Cmd. creds nil => anonymous (public
// images); non-nil => the token/Basic flow authenticates with them, and
// auth-shaped failures map to ErrAuthFailed instead of ErrPrivate.
func (c *Checker) Check(ctx context.Context, ref imageref.Ref, creds *Credentials) (Result, error) {
	// One overall deadline for the whole pre-flight (up to 4 sequential
	// requests): the per-request client timeout alone would let a
	// slow-dripping hostile registry hold the create handler ~60s.
	ctx, cancel := context.WithTimeout(ctx, overallTimeout)
	defer cancel()
	base := c.scheme + "://" + c.hostmap(ref.Registry)
	target := ref.Tag
	if ref.Digest != "" {
		target = ref.Digest
	}

	auth, err := c.authHeader(ctx, base, ref.Repository, creds)
	if err != nil {
		return Result{}, mapAuthErr(err, creds)
	}

	body, headers, err := c.get(ctx, base+"/v2/"+ref.Repository+"/manifests/"+target, auth, acceptManifests, maxManifestBytes)
	if err != nil {
		return Result{}, mapAuthErr(err, creds)
	}
	// The digest is COMPUTED from the manifest bytes, never taken from the
	// response header: OCI digests are content-addressed (sha256 over the raw
	// manifest body), and the header comes from a customer-controlled server —
	// header bytes spliced into the launch ref would bypass the imageref
	// grammar gate entirely. Deriving it ourselves guarantees the pin is
	// always present (no silent unpinned fallback) and always well-formed by
	// construction. A PRESENT header that disagrees with the computed digest
	// means the registry is lying about its own content — fail closed.
	sum := sha256.Sum256(body)
	res := Result{Digest: "sha256:" + hex.EncodeToString(sum[:])}
	// Cross-check only sha256 headers: the OCI spec permits other algorithms
	// (sha512), and a legitimate non-sha256 header would otherwise always
	// string-mismatch. Every in-scope registry uses sha256, and the computed
	// digest above is authoritative either way.
	if hd := headers.Get("Docker-Content-Digest"); strings.HasPrefix(hd, "sha256:") && hd != res.Digest {
		return Result{}, fmt.Errorf("%w: registry digest header disagrees with manifest content", ErrUnreachable)
	}
	// A digest-pinned request must resolve to exactly the digest asked for.
	if ref.Digest != "" && ref.Digest != res.Digest {
		return Result{}, fmt.Errorf("%w: manifest content does not match the requested digest", ErrNotFound)
	}

	var top manifestEnvelope
	if err := json.Unmarshal(body, &top); err != nil {
		return Result{}, fmt.Errorf("%w: malformed manifest", ErrUnreachable)
	}

	configDigest := top.Config.Digest
	if configDigest == "" {
		// Manifest list / OCI index: resolve the linux/amd64 entry.
		var platDigest string
		for _, m := range top.Manifests {
			if m.Platform.OS == "linux" && m.Platform.Architecture == "amd64" {
				platDigest = m.Digest
				break
			}
		}
		if platDigest == "" {
			return Result{}, ErrPlatform
		}
		platBody, _, err := c.get(ctx, base+"/v2/"+ref.Repository+"/manifests/"+platDigest, auth, acceptManifests, maxManifestBytes)
		if err != nil {
			return Result{}, err
		}
		var plat manifestEnvelope
		if err := json.Unmarshal(platBody, &plat); err != nil || plat.Config.Digest == "" {
			return Result{}, fmt.Errorf("%w: malformed platform manifest", ErrUnreachable)
		}
		configDigest = plat.Config.Digest
	}

	cfgBody, _, err := c.get(ctx, base+"/v2/"+ref.Repository+"/blobs/"+configDigest, auth, "", maxConfigBytes)
	if err != nil {
		return Result{}, err
	}
	var cfg imageConfig
	if err := json.Unmarshal(cfgBody, &cfg); err != nil {
		return Result{}, fmt.Errorf("%w: malformed image config", ErrUnreachable)
	}
	// Single-manifest images declare platform only here. "" tolerated for
	// registries that omit it (rare); a declared mismatch is a hard fail.
	if (cfg.OS != "" && cfg.OS != "linux") || (cfg.Architecture != "" && cfg.Architecture != "amd64") {
		return Result{}, ErrPlatform
	}
	res.Entrypoint = cfg.Config.Entrypoint
	res.Cmd = cfg.Config.Cmd
	return res, nil
}

// authHeader probes GET /v2/ and resolves the Authorization header value for
// subsequent requests: "" (anonymous registry), "Bearer <token>" (challenge
// realm flow, with Basic credentials on the token request when creds are
// given), or "Basic <b64>" (Basic-only registry with creds).
func (c *Checker) authHeader(ctx context.Context, base, repo string, creds *Credentials) (string, error) {
	basic := ""
	if creds != nil {
		basic = "Basic " + base64.StdEncoding.EncodeToString([]byte(creds.Username+":"+creds.Password))
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, base+"/v2/", nil)
	if err != nil {
		return "", fmt.Errorf("%w: %v", ErrUnreachable, err)
	}
	resp, err := c.client.Do(req)
	if err != nil {
		return "", fmt.Errorf("%w (%v)", ErrUnreachable, dialErrSummary(err))
	}
	defer resp.Body.Close()
	io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
	if resp.StatusCode != http.StatusUnauthorized {
		// Anonymous registry — but if creds were supplied, send them anyway:
		// some registries accept everything on /v2/ yet gate repositories.
		return basic, nil
	}
	realm, service := parseBearerChallenge(resp.Header.Get("WWW-Authenticate"))
	if realm == "" {
		// Basic-only registry: creds ride every request; anonymous is refused.
		if basic != "" {
			return basic, nil
		}
		return "", ErrPrivate
	}
	tokURL := realm + "?service=" + service + "&scope=repository:" + repo + ":pull"
	treq, err := http.NewRequestWithContext(ctx, http.MethodGet, tokURL, nil)
	if err != nil {
		return "", fmt.Errorf("%w: %v", ErrUnreachable, err)
	}
	if creds != nil {
		treq.SetBasicAuth(creds.Username, creds.Password)
	}
	tresp, err := c.client.Do(treq)
	if err != nil {
		return "", fmt.Errorf("%w (token service: %v)", ErrUnreachable, dialErrSummary(err))
	}
	defer tresp.Body.Close()
	if tresp.StatusCode != http.StatusOK {
		io.Copy(io.Discard, io.LimitReader(tresp.Body, 4096))
		return "", ErrPrivate
	}
	var tok struct {
		Token       string `json:"token"`
		AccessToken string `json:"access_token"`
	}
	if err := json.NewDecoder(io.LimitReader(tresp.Body, 1<<20)).Decode(&tok); err != nil {
		return "", fmt.Errorf("%w: malformed token response", ErrUnreachable)
	}
	if tok.Token == "" {
		tok.Token = tok.AccessToken
	}
	if tok.Token == "" {
		return "", nil
	}
	return "Bearer " + tok.Token, nil
}

// mapAuthErr converts anonymous-shaped auth failures into the credentialed
// error when credentials were in play: a 401/403 WITH creds means the login is
// wrong, not that the image is private.
func mapAuthErr(err error, creds *Credentials) error {
	if err != nil && creds != nil && errors.Is(err, ErrPrivate) {
		return ErrAuthFailed
	}
	return err
}

// get performs one authorized GET. auth is the FULL Authorization header
// value ("Bearer …" or "Basic …"), or "" for anonymous.
func (c *Checker) get(ctx context.Context, url, auth, accept string, cap int64) ([]byte, http.Header, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, nil, fmt.Errorf("%w: %v", ErrUnreachable, err)
	}
	if accept != "" {
		req.Header.Set("Accept", accept)
	}
	if auth != "" {
		req.Header.Set("Authorization", auth)
	}
	resp, err := c.client.Do(req)
	if err != nil {
		return nil, nil, fmt.Errorf("%w (%v)", ErrUnreachable, dialErrSummary(err))
	}
	defer resp.Body.Close()
	switch resp.StatusCode {
	case http.StatusOK:
	case http.StatusNotFound:
		io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
		return nil, nil, ErrNotFound
	case http.StatusUnauthorized, http.StatusForbidden:
		io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
		return nil, nil, ErrPrivate
	default:
		io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
		return nil, nil, fmt.Errorf("%w (registry returned HTTP %d)", ErrUnreachable, resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, cap))
	if err != nil {
		return nil, nil, fmt.Errorf("%w: %v", ErrUnreachable, err)
	}
	return body, resp.Header, nil
}

// parseBearerChallenge extracts realm and service from a WWW-Authenticate
// Bearer challenge. Returns "" realm when the challenge is not Bearer.
func parseBearerChallenge(h string) (realm, service string) {
	if !strings.HasPrefix(h, "Bearer ") {
		return "", ""
	}
	for _, part := range strings.Split(h[len("Bearer "):], ",") {
		part = strings.TrimSpace(part)
		if v, ok := strings.CutPrefix(part, "realm="); ok {
			realm = strings.Trim(v, `"`)
		}
		if v, ok := strings.CutPrefix(part, "service="); ok {
			service = strings.Trim(v, `"`)
		}
	}
	return realm, service
}

// dialErrSummary keeps transport errors short and free of internal detail
// before they reach a customer-facing message.
func dialErrSummary(err error) string {
	var nerr net.Error
	if errors.As(err, &nerr) && nerr.Timeout() {
		return "timeout"
	}
	if strings.Contains(err.Error(), "not publicly routable") {
		return "registry address is not publicly routable"
	}
	return "connection failed"
}
