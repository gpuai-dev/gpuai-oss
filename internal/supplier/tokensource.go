package supplier

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

const (
	tokenRequestTimeout = 10 * time.Second

	// tokenExpirySafetyMargin is subtracted from the supplier-reported
	// expires_in so a token is refreshed before it can expire mid-flight —
	// a provision call issued with a token that dies during the request
	// would fail an operation the retry layer won't re-authenticate.
	tokenExpirySafetyMargin = 60 * time.Second

	// defaultTokenTTL is assumed when a supplier omits expires_in. Short
	// enough that a supplier with aggressive TTLs still mostly sees fresh
	// tokens, long enough to amortize the token round-trip across a poll
	// cycle.
	defaultTokenTTL = 5 * time.Minute
)

// OutboundCred is one supplier's outbound OAuth2 client-credentials — the
// client_id/client_secret that supplier issued to GPU.ai. TokenURL empty
// means derive base_url + "/oauth/token" from the supplier row.
type OutboundCred struct {
	ClientID     string
	ClientSecret string
	TokenURL     string
}

// TokenSource exchanges outbound client-credentials for short-lived bearer
// tokens at each supplier's OAuth token endpoint, caching them until just
// before expiry. Its Token method matches generic.TokenFunc.
type TokenSource struct {
	creds      map[string]OutboundCred // keyed by lowercase supplier slug
	httpClient *http.Client
	logger     *slog.Logger
	now        func() time.Time

	mu    sync.Mutex
	cache map[string]cachedToken // keyed by lowercase supplier slug
}

type cachedToken struct {
	token     string
	expiresAt time.Time
}

// TokenSourceOption configures a TokenSource.
type TokenSourceOption func(*TokenSource)

// WithHTTPClient overrides the default HTTP client (test seam).
func WithHTTPClient(hc *http.Client) TokenSourceOption {
	return func(ts *TokenSource) { ts.httpClient = hc }
}

// WithClock overrides the time source (test seam).
func WithClock(now func() time.Time) TokenSourceOption {
	return func(ts *TokenSource) { ts.now = now }
}

// NewTokenSource creates a TokenSource over the given per-slug credentials.
// Logger may be nil (defaults to slog.Default).
func NewTokenSource(creds map[string]OutboundCred, logger *slog.Logger, opts ...TokenSourceOption) *TokenSource {
	if logger == nil {
		logger = slog.Default()
	}
	ts := &TokenSource{
		creds:      creds,
		httpClient: &http.Client{Timeout: tokenRequestTimeout},
		logger:     logger,
		now:        time.Now,
		cache:      make(map[string]cachedToken),
	}
	for _, opt := range opts {
		opt(ts)
	}
	return ts
}

// Token returns a valid bearer token for the supplier, fetching a fresh one
// from the supplier's token endpoint if the cached token is missing or near
// expiry. A supplier with no configured credentials gets ("", nil) — the
// generic adapter then sends unauthenticated calls, preserving the
// pre-OAuth behavior for suppliers that don't require it.
func (ts *TokenSource) Token(ctx context.Context, s Supplier) (string, error) {
	slug := strings.ToLower(s.Slug)
	cred, ok := ts.creds[slug]
	if !ok {
		return "", nil
	}

	ts.mu.Lock()
	if c, ok := ts.cache[slug]; ok && ts.now().Before(c.expiresAt) {
		token := c.token
		ts.mu.Unlock()
		return token, nil
	}
	ts.mu.Unlock()

	token, ttl, err := ts.fetch(ctx, s, cred)
	if err != nil {
		return "", err
	}

	ts.mu.Lock()
	ts.cache[slug] = cachedToken{token: token, expiresAt: ts.now().Add(ttl)}
	ts.mu.Unlock()

	ts.logger.Info("fetched supplier bearer token",
		slog.String("supplier", slug),
		slog.Duration("ttl", ttl),
	)
	return token, nil
}

// tokenEndpoint resolves the token URL for a supplier: an explicit
// per-supplier override wins, otherwise base_url + "/oauth/token".
func tokenEndpoint(s Supplier, cred OutboundCred) string {
	if cred.TokenURL != "" {
		return cred.TokenURL
	}
	return strings.TrimSuffix(s.BaseURL, "/") + "/oauth/token"
}

// oauthTokenResponse is the RFC 6749 Section 5.1 token response subset we read.
type oauthTokenResponse struct {
	AccessToken string `json:"access_token"`
	TokenType   string `json:"token_type"`
	ExpiresIn   int    `json:"expires_in"`
}

// fetch performs the client_credentials grant against the supplier's token
// endpoint. Credentials go in the form body (RFC 6749 Section 2.3.1's
// widely-accepted variant; our own inbound endpoint accepts the same).
func (ts *TokenSource) fetch(ctx context.Context, s Supplier, cred OutboundCred) (token string, ttl time.Duration, err error) {
	ctx, cancel := context.WithTimeout(ctx, tokenRequestTimeout)
	defer cancel()

	form := url.Values{
		"grant_type":    {"client_credentials"},
		"client_id":     {cred.ClientID},
		"client_secret": {cred.ClientSecret},
	}
	endpoint := tokenEndpoint(s, cred)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return "", 0, fmt.Errorf("create token request for %s: %w", s.Slug, err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	resp, err := ts.httpClient.Do(req)
	if err != nil {
		return "", 0, fmt.Errorf("token request to %s: %w", s.Slug, err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return "", 0, fmt.Errorf("read token response from %s: %w", s.Slug, err)
	}
	if resp.StatusCode != http.StatusOK {
		// Body may carry an RFC 6749 error code; it never carries a token,
		// so logging/wrapping it is safe.
		return "", 0, fmt.Errorf("token endpoint for %s returned HTTP %d: %s", s.Slug, resp.StatusCode, truncate(string(body), 256))
	}

	var tr oauthTokenResponse
	if err := json.Unmarshal(body, &tr); err != nil {
		return "", 0, fmt.Errorf("unmarshal token response from %s: %w", s.Slug, err)
	}
	if tr.AccessToken == "" {
		return "", 0, fmt.Errorf("token endpoint for %s returned no access_token", s.Slug)
	}

	ttl = defaultTokenTTL
	if tr.ExpiresIn > 0 {
		ttl = time.Duration(tr.ExpiresIn) * time.Second
	}
	ttl -= tokenExpirySafetyMargin
	if ttl <= 0 {
		// Token shorter than the safety margin: use it once, never cache.
		ttl = 0
	}
	return tr.AccessToken, ttl, nil
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
