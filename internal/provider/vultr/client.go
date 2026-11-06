// Package vultr implements the Provider interface for Vultr Cloud GPU.
//
// Mental model: "plain REST over a public plan catalog with an authenticated
// account surface." GET /v2/plans carries every Cloud GPU plan with
// whole-instance hourly USD pricing and the per-plan region list (spread over
// two plan types since ~2026-10-12, see listGPUPlans); instances
// are classic VMs (POST /v2/instances with a numeric os_id and base64
// cloud-init user_data). The catalog endpoint is PUBLIC — it answers 200 even
// with a bad token — so ListAvailable probes GET /v2/account first to keep a
// dead key loud instead of polling a catalog we could never provision from.
package vultr

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/gpuai-dev/gpuai-oss/internal/provider"
)

// truncate bounds operator-log bodies to n bytes.
func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

const defaultBaseURL = "https://api.vultr.com/v2"

const maxRetryAttempts = 3

// Per-operation default timeouts. provisionTimeout has headroom for the
// os_id resolution read that can precede the create.
const (
	listTimeout      = 15 * time.Second
	provisionTimeout = 60 * time.Second
	statusTimeout    = 10 * time.Second
	// terminateTimeout bounds a terminate for a DEADLINE-LESS caller (the
	// reconciler). It must cover Vultr's LOCK window: Vultr refuses a DELETE
	// with "server is currently locked" (400, or 409 during an upstream
	// incident) while a freshly created instance is still settling, and a
	// short-lived attempt ladder simply gives up inside that window and
	// STRANDS a billing VM. That is the 2026-09-06 leak: a 409 during a Vultr
	// API outage exhausted the old 3-attempt/~7s ladder, the smoke instance
	// survived teardown and billed for 44h / ~$41, taking the account
	// negative. 4 min gives ample margin over the settle window; a caller with its own deadline (the lifecycle
	// harness's wall clock) keeps that instead — contextWithDefaultTimeout.
	terminateTimeout = 4 * time.Minute
)

// Client is an HTTP client for the Vultr v2 API.
type Client struct {
	apiKey     string
	baseURL    string
	httpClient *http.Client
}

// ClientOption configures the client.
type ClientOption func(*Client)

// WithBaseURL overrides the API endpoint — the httptest seam.
func WithBaseURL(url string) ClientOption { return func(c *Client) { c.baseURL = url } }

// WithHTTPClient overrides the HTTP client.
func WithHTTPClient(hc *http.Client) ClientOption { return func(c *Client) { c.httpClient = hc } }

// NewClient creates a Vultr API client. Auth is `Authorization: Bearer <key>`
// (live-verified 2026-08-11).
func NewClient(apiKey string, opts ...ClientOption) *Client {
	c := &Client{
		apiKey:     apiKey,
		baseURL:    defaultBaseURL,
		httpClient: &http.Client{Timeout: 30 * time.Second},
	}
	for _, opt := range opts {
		opt(c)
	}
	return c
}

// httpStatusError carries the status code so callers can branch on 404
// (idempotent terminate, gone → terminated) without string matching. Body is
// kept in-package (privacy contract): never echo the upstream body verbatim into user-facing errors.
// A launch failure's error string lands in customer-visible operation detail
// (SetOperationFailure → GET /v1/operations), so Error() surfaces only the
// status code; the body is logged operator-side in do().
type httpStatusError struct {
	StatusCode int
	Body       string
}

func (e *httpStatusError) Error() string {
	return fmt.Sprintf("HTTP %d", e.StatusCode)
}

// do issues one request and unmarshals the JSON body into result. Returns the
// *http.Response so retry logic can inspect the status code.
func (c *Client) do(ctx context.Context, method, path string, body, result any) (*http.Response, error) {
	var reqBody io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return nil, fmt.Errorf("marshal request: %w", err)
		}
		reqBody = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.baseURL+path, reqBody)
	if err != nil {
		return nil, fmt.Errorf("create request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+c.apiKey)
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("execute request: %w", err)
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return resp, fmt.Errorf("read response: %w", err)
	}

	if resp.StatusCode >= 400 {
		// Operator-side visibility for the body the wrapped errors deliberately
		// omit (privacy contract on httpStatusError). Truncated; never contains
		// credentials.
		slog.Warn("vultr API error response",
			"method", method, "path", path, "status", resp.StatusCode,
			"body", truncate(string(respBody), 300))
		// Map capacity/out-of-stock phrasing to ErrNoCapacity so the engine
		// fails over to the next candidate instead of posting a hard error.
		// Vultr phrases a region/plan stock-out as "... is not available ..."
		// (e.g. "Plan is not available in your selected region"). The body
		// itself stays out of the error string — it can reach customer-visible
		// operation detail.
		lower := strings.ToLower(string(respBody))
		// Account-entitlement sniff FIRST: these
		// are OUR account's billing/ACL gates at Vultr, not capacity churn — a
		// retry can NEVER succeed until ops act, so they must not map to the
		// retryable ErrNoCapacity. ErrQuotaExceeded makes the API layer alert
		// ops (an error log line) and steer the customer instead of inviting a retry. All
		// three phrases were hit LIVE on 2026-08-14 as opaque HTTP 400/401
		// provision failures:
		//   - "This product requires a minimum deposit of $50" (400)
		//   - "You have reached the maximum monthly fee limit for this
		//     account..." (400) — consumed by deployed monthly LIST price
		//   - "Unauthorized IP address: <ip>" (401) — the API-key IP ACL. If
		//     you restrict the key to known source IPs, include every address
		//     gpuctl egresses from.
		if strings.Contains(lower, "monthly fee limit") ||
			strings.Contains(lower, "minimum deposit") ||
			strings.Contains(lower, "unauthorized ip address") {
			return resp, fmt.Errorf("account entitlement error (HTTP %d): %w", resp.StatusCode, provider.ErrQuotaExceeded)
		}
		if strings.Contains(lower, "not available") ||
			strings.Contains(lower, "no capacity") ||
			strings.Contains(lower, "out of stock") ||
			strings.Contains(lower, "sold out") {
			return resp, fmt.Errorf("HTTP %d: %w", resp.StatusCode, provider.ErrNoCapacity)
		}
		return resp, &httpStatusError{StatusCode: resp.StatusCode, Body: string(respBody)}
	}

	if result != nil && len(respBody) > 0 {
		if err := json.Unmarshal(respBody, result); err != nil {
			return resp, fmt.Errorf("unmarshal response: %w", err)
		}
	}
	return resp, nil
}

// doWithRetry retries transient failures: 3 attempts, exponential backoff
// 1/2/4s, honor Retry-After on 429, retry 5xx + network errors, NOT other
// 4xx, NOT capacity errors (capacity won't appear in seconds).
func (c *Client) doWithRetry(ctx context.Context, op, method, path string, body, result any) error {
	var lastErr error
	for attempt := 0; attempt < maxRetryAttempts; attempt++ {
		resp, err := c.do(ctx, method, path, body, result)
		if err == nil {
			return nil
		}
		lastErr = err

		// Never retry a capacity or account-entitlement error — capacity won't
		// appear in seconds, and an entitlement gate (monthly fee limit, min
		// deposit, IP ACL) won't lift until ops act.
		if errors.Is(err, provider.ErrNoCapacity) || errors.Is(err, provider.ErrQuotaExceeded) {
			return err
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if resp != nil {
			switch {
			case resp.StatusCode == http.StatusTooManyRequests:
				delay := parseRetryAfter(resp.Header.Get("Retry-After"))
				select {
				case <-ctx.Done():
					return ctx.Err()
				case <-time.After(delay):
				}
				continue
			case resp.StatusCode >= 400 && resp.StatusCode < 500:
				// Every non-429 4xx fast-fails here, INCLUDING a "locked"
				// DELETE refusal: terminate lock/conflict retry is owned by the
				// adapter (deleteInstanceRetryingLocked). Keeping an escape here too would make each
				// adapter-level attempt burn its own 1/2/4s inner ladder before
				// the outer steady sleep, for no added coverage.
				return err // client error (not 429) — not retryable
			}
		}
		delay := time.Duration(1<<uint(attempt)) * time.Second // 1s, 2s, 4s
		slog.Warn("vultr API call failed, backing off",
			"op", op, "attempt", attempt+1, "delay", delay, "error", err)
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(delay):
		}
	}
	return fmt.Errorf("%s failed after %d attempts: %w", op, maxRetryAttempts, lastErr)
}

func parseRetryAfter(val string) time.Duration {
	if val == "" {
		return 5 * time.Second
	}
	if s, err := strconv.Atoi(val); err == nil {
		return time.Duration(s) * time.Second
	}
	return 5 * time.Second
}

// contextWithDefaultTimeout applies a default timeout only when the caller's
// context has no deadline yet.
func contextWithDefaultTimeout(ctx context.Context, timeout time.Duration) (context.Context, context.CancelFunc) {
	if _, hasDeadline := ctx.Deadline(); hasDeadline {
		return ctx, func() {}
	}
	return context.WithTimeout(ctx, timeout)
}

// --- Typed API surface ---------------------------------------------------

// plan is one GPU row of GET /plans. GPUCount is a json.RawMessage because
// Vultr encodes whole-GPU plans as a bare number (2) and fractional vGPU
// slices as a string fraction ("1/8") in the SAME field. Only type "vcg" rows
// carry the gpu_type/gpu_count/gpu_vram_gb trio; "vdm" rows omit all three and
// the GPU is read from the plan id instead (planGPU).
type plan struct {
	ID             string          `json:"id"`
	VCPUCount      int             `json:"vcpu_count"`
	RAMMB          int             `json:"ram"`
	DiskGB         int             `json:"disk"`
	MonthlyCost    float64         `json:"monthly_cost"`
	HourlyCost     float64         `json:"hourly_cost"`
	Type           string          `json:"type"`
	Locations      []string        `json:"locations"`
	DeployOnDemand bool            `json:"deploy_ondemand"`
	GPUBrand       string          `json:"gpu_brand"`   // "NVIDIA" | "AMD" | "none" on CPU plans
	GPUVRAMGB      int             `json:"gpu_vram_gb"` // TOTAL across all GPUs of the plan
	GPUType        string          `json:"gpu_type"`    // e.g. "NVIDIA_L40S"
	GPUCount       json.RawMessage `json:"gpu_count"`
}

// isGPU reports whether a /plans row is a GPU plan. gpu_brand is the one GPU
// field every GPU row carries regardless of plan type; CPU plans say "none".
func (p plan) isGPU() bool {
	b := strings.ToLower(strings.TrimSpace(p.GPUBrand))
	return b != "" && b != "none"
}

// listGPUPlans returns every Cloud GPU plan. It walks the UNFILTERED catalog
// and keeps rows with a GPU brand rather than asking for ?type=vcg: around
// 2026-10-12 Vultr moved every whole-GPU A16/A40/A100/H100/B200 plan to a new
// "vdm" type (same vcg-* ids, same POST /instances deploy) and left only three
// stocked-out L40S plans under "vcg". The type filter silently shrank our
// catalog to zero deployable rows, so vultr went dark with every poll green;
// the state probe's dead-SKU check was the only thing that noticed. Filtering
// on gpu_brand survives the next re-typing. The endpoint is public; 139 rows
// live 2026-11-04 against a per_page cap of 500, cursor-walked anyway.
func (c *Client) listGPUPlans(ctx context.Context) ([]plan, error) {
	var out []plan
	cursor := ""
	for page := 0; page < 20; page++ {
		path := "/plans?per_page=500"
		if cursor != "" {
			path += "&cursor=" + url.QueryEscape(cursor)
		}
		var resp struct {
			Plans []plan `json:"plans"`
			Meta  struct {
				Links struct {
					Next string `json:"next"`
				} `json:"links"`
			} `json:"meta"`
		}
		if err := c.doWithRetry(ctx, "ListPlans", "GET", path, nil, &resp); err != nil {
			return nil, err
		}
		for _, p := range resp.Plans {
			if p.isGPU() {
				out = append(out, p)
			}
		}
		if resp.Meta.Links.Next == "" {
			return out, nil
		}
		cursor = resp.Meta.Links.Next
	}
	return nil, fmt.Errorf("ListPlans: cursor did not terminate after 20 pages")
}

// checkAuth probes the authenticated account endpoint. GET /plans is PUBLIC
// (answers 200 to a bad token, live-verified 2026-08-11), so without this
// probe a revoked key would keep the catalog green while every provision
// 401s. The response body (account email/billing) is deliberately discarded
// and never logged.
func (c *Client) checkAuth(ctx context.Context) error {
	var discard struct{}
	return c.doWithRetry(ctx, "CheckAuth", "GET", "/account", nil, &discard)
}

// AccountBalance returns the account's remaining prepaid credit runway in USD
// via GET /account, for the supplier balance guard
// (tests/providers/balance_test.go, daily live-test clock). Vultr reports
// deposited credit as a NEGATIVE balance (live 2026-08-14: a $50 PayPal
// deposit shows `"balance": -60, "last_payment_amount": -50`) while
// pending_charges accrues POSITIVE against it, so runway =
// -balance - pending_charges. A postpaid/overdrawn account therefore probes
// negative and reds the guard — exactly the alarm we want before Vultr stops
// customer instances. Only these two figures are decoded; the rest of the
// account payload (name/email/address) is discarded and never logged.
func (c *Client) AccountBalance(ctx context.Context) (float64, error) {
	var resp struct {
		Account struct {
			Balance        float64 `json:"balance"`
			PendingCharges float64 `json:"pending_charges"`
		} `json:"account"`
	}
	if err := c.doWithRetry(ctx, "AccountBalance", "GET", "/account", nil, &resp); err != nil {
		return 0, err
	}
	return -resp.Account.Balance - resp.Account.PendingCharges, nil
}

// os is one row of GET /os.
type osImage struct {
	ID   int    `json:"id"`
	Name string `json:"name"`
	Arch string `json:"arch"`
}

// listOS returns the OS image catalog (for resolving a requested OS name to
// Vultr's numeric os_id).
func (c *Client) listOS(ctx context.Context) ([]osImage, error) {
	var resp struct {
		OS []osImage `json:"os"`
	}
	if err := c.doWithRetry(ctx, "ListOS", "GET", "/os?per_page=500", nil, &resp); err != nil {
		return nil, err
	}
	return resp.OS, nil
}

// instance is the wire shape of a Vultr instance (create/get/list).
type instance struct {
	ID           string `json:"id"`
	OS           string `json:"os"`
	RAMMB        int    `json:"ram"`
	DiskGB       int    `json:"disk"`
	MainIP       string `json:"main_ip"`
	VCPUCount    int    `json:"vcpu_count"`
	Region       string `json:"region"`
	Plan         string `json:"plan"`
	Status       string `json:"status"`        // pending | active | suspended | closed (+ resizing)
	PowerStatus  string `json:"power_status"`  // running | stopped
	ServerStatus string `json:"server_status"` // none | locked | installingbooting | isomounting | ok
	Label        string `json:"label"`
	Hostname     string `json:"hostname"`
	DateCreated  string `json:"date_created"`
}

// createInstanceRequest is the POST /instances body. user_data MUST be
// base64; cloud-init on the booted VM decodes and runs it (a `#!` script or a
// `#cloud-config` document both work — Vultr does not validate the shape).
type createInstanceRequest struct {
	Region          string   `json:"region"`
	Plan            string   `json:"plan"`
	OSID            int      `json:"os_id"`
	Label           string   `json:"label"`
	Hostname        string   `json:"hostname"`
	UserData        string   `json:"user_data,omitempty"`
	Backups         string   `json:"backups"`
	ActivationEmail bool     `json:"activation_email"`
	DDOSProtection  bool     `json:"ddos_protection"`
	Tags            []string `json:"tags,omitempty"`
}

// createInstance is deliberately a SINGLE attempt: POST /instances is NOT
// idempotent (no idempotency key in the API). A 5xx or transport error AFTER
// Vultr has already created the VM would make a retry create a SECOND one, and
// we would return only the second id — the first then bills forever behind a
// live DB row the reconciler's orphan pass deliberately ignores (its label
// encodes the same instance id).
func (c *Client) createInstance(ctx context.Context, req createInstanceRequest) (*instance, error) {
	var resp struct {
		Instance instance `json:"instance"`
	}
	if _, err := c.do(ctx, "POST", "/instances", req, &resp); err != nil {
		return nil, fmt.Errorf("CreateInstance: %w", err)
	}
	return &resp.Instance, nil
}

func (c *Client) getInstance(ctx context.Context, id string) (*instance, error) {
	var resp struct {
		Instance instance `json:"instance"`
	}
	if err := c.doWithRetry(ctx, "GetInstance", "GET", "/instances/"+id, nil, &resp); err != nil {
		return nil, err
	}
	return &resp.Instance, nil
}

func (c *Client) deleteInstance(ctx context.Context, id string) error {
	return c.doWithRetry(ctx, "DeleteInstance", "DELETE", "/instances/"+id, nil, nil)
}

// listInstancesPage returns one cursor page of the account's instances plus
// the next-page cursor ("" when exhausted).
func (c *Client) listInstancesPage(ctx context.Context, cursor string) ([]instance, string, error) {
	path := "/instances?per_page=500"
	if cursor != "" {
		// Cursors are base64 — a raw "+" would decode server-side as a space.
		path += "&cursor=" + url.QueryEscape(cursor)
	}
	var resp struct {
		Instances []instance `json:"instances"`
		Meta      struct {
			Links struct {
				Next string `json:"next"`
			} `json:"links"`
		} `json:"meta"`
	}
	if err := c.doWithRetry(ctx, "ListInstances", "GET", path, nil, &resp); err != nil {
		return nil, "", err
	}
	return resp.Instances, resp.Meta.Links.Next, nil
}
