// Package generic implements a multiplexing Provider adapter for GPU suppliers
// that implement the GPU.ai Supplier Integration API (REST spec).
package generic

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"time"

	"github.com/gpuai-dev/gpuai-oss/internal/supplier"
)

const (
	maxRetryAttempts = 3

	// Per-operation default timeouts.
	listTimeout      = 10 * time.Second
	provisionTimeout = 30 * time.Second
	statusTimeout    = 10 * time.Second
	terminateTimeout = 30 * time.Second
)

// Client is an HTTP client for calling supplier REST endpoints.
type Client struct {
	httpClient *http.Client
	logger     *slog.Logger
}

// ClientOption configures the generic Client.
type ClientOption func(*Client)

// WithHTTPClient overrides the default HTTP client.
func WithHTTPClient(hc *http.Client) ClientOption {
	return func(c *Client) {
		c.httpClient = hc
	}
}

// WithLogger overrides the default logger.
func WithLogger(l *slog.Logger) ClientOption {
	return func(c *Client) {
		c.logger = l
	}
}

// NewClient creates a new generic supplier HTTP client.
func NewClient(opts ...ClientOption) *Client {
	c := &Client{
		httpClient: &http.Client{
			Timeout: 30 * time.Second,
		},
		logger: slog.Default(),
	}
	for _, opt := range opts {
		opt(c)
	}
	return c
}

// do sends an HTTP request and unmarshals the response into result.
// It sets Content-Type and Authorization headers, checks for HTTP errors,
// and returns the raw response for retry logic inspection.
func (c *Client) do(ctx context.Context, method, url string, body any, result any, token string) (*http.Response, error) {
	var bodyReader io.Reader
	if body != nil {
		data, err := json.Marshal(body)
		if err != nil {
			return nil, fmt.Errorf("marshal request body: %w", err)
		}
		bodyReader = bytes.NewReader(data)
	}

	httpReq, err := http.NewRequestWithContext(ctx, method, url, bodyReader)
	if err != nil {
		return nil, fmt.Errorf("create request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	if token != "" {
		httpReq.Header.Set("Authorization", "Bearer "+token)
	}

	resp, err := c.httpClient.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("execute request: %w", err)
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return resp, fmt.Errorf("read response: %w", err)
	}

	if resp.StatusCode >= 400 {
		return resp, fmt.Errorf("HTTP %d: %s", resp.StatusCode, string(respBody))
	}

	if result != nil && len(respBody) > 0 {
		if err := json.Unmarshal(respBody, result); err != nil {
			return resp, fmt.Errorf("unmarshal response: %w", err)
		}
	}

	return resp, nil
}

// doWithRetry executes an HTTP request with retry logic for transient errors.
// It retries up to maxAttempts times with exponential backoff for 5xx and network errors,
// respects Retry-After headers on 429, and returns immediately for non-retryable 4xx errors.
func (c *Client) doWithRetry(ctx context.Context, op string, maxAttempts int, method, url string, body any, result any, token string) error {
	var lastErr error
	for attempt := 0; attempt < maxAttempts; attempt++ {
		if attempt > 0 {
			c.logger.Warn("retrying supplier API call",
				"op", op,
				"attempt", attempt+1,
				"max_attempts", maxAttempts,
			)
		}

		resp, err := c.do(ctx, method, url, body, result, token)
		if err == nil {
			return nil
		}
		lastErr = err

		// Don't retry context cancellation.
		if ctx.Err() != nil {
			return ctx.Err()
		}

		// Determine if retryable based on HTTP status code.
		if resp != nil {
			switch {
			case resp.StatusCode == http.StatusTooManyRequests:
				delay := parseRetryAfter(resp.Header.Get("Retry-After"))
				c.logger.Warn("supplier rate limited",
					"op", op,
					"retry_after_seconds", delay.Seconds(),
				)
				select {
				case <-ctx.Done():
					return ctx.Err()
				case <-time.After(delay):
				}
				continue
			case resp.StatusCode >= 500:
				// Server error -- retryable with exponential backoff.
			case resp.StatusCode >= 400 && resp.StatusCode < 500:
				// Client error (not 429) -- not retryable.
				return err
			}
		}

		// Exponential backoff: 1s, 2s, 4s.
		delay := time.Duration(1<<uint(attempt)) * time.Second
		c.logger.Warn("supplier API call failed, backing off",
			"op", op,
			"attempt", attempt+1,
			"delay", delay,
			"error", err,
		)
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(delay):
		}
	}

	return fmt.Errorf("%s failed after %d attempts: %w", op, maxAttempts, lastErr)
}

// parseRetryAfter parses a Retry-After header value.
// Returns a default of 5 seconds if the header is missing or unparseable.
func parseRetryAfter(val string) time.Duration {
	if val == "" {
		return 5 * time.Second
	}
	seconds, err := strconv.Atoi(val)
	if err != nil {
		return 5 * time.Second
	}
	return time.Duration(seconds) * time.Second
}

// contextWithDefaultTimeout applies a default timeout to a context only if
// the context does not already have a deadline set.
func contextWithDefaultTimeout(ctx context.Context, timeout time.Duration) (context.Context, context.CancelFunc) {
	if _, hasDeadline := ctx.Deadline(); hasDeadline {
		return ctx, func() {} // No-op cancel; caller's deadline takes precedence.
	}
	return context.WithTimeout(ctx, timeout)
}

// --- Supplier endpoint methods ---

// GetAvailable calls GET {baseURL}/v1/gpu/available on a supplier.
func (c *Client) GetAvailable(ctx context.Context, s supplier.Supplier, token string) (*availableResponse, error) {
	ctx, cancel := contextWithDefaultTimeout(ctx, listTimeout)
	defer cancel()

	url := s.BaseURL + "/v1/gpu/available"
	var resp availableResponse
	if err := c.doWithRetry(ctx, "GetAvailable("+s.Slug+")", maxRetryAttempts, http.MethodGet, url, nil, &resp, token); err != nil {
		return nil, err
	}
	return &resp, nil
}

// Provision calls POST {baseURL}/v1/instances on a supplier.
func (c *Client) Provision(ctx context.Context, s supplier.Supplier, token string, req provisionRequest) (*provisionResponse, error) {
	ctx, cancel := contextWithDefaultTimeout(ctx, provisionTimeout)
	defer cancel()

	url := s.BaseURL + "/v1/instances"
	var resp provisionResponse
	if err := c.doWithRetry(ctx, "Provision("+s.Slug+")", maxRetryAttempts, http.MethodPost, url, req, &resp, token); err != nil {
		return nil, err
	}
	return &resp, nil
}

// GetStatus calls GET {baseURL}/v1/instances/{instanceID} on a supplier.
func (c *Client) GetStatus(ctx context.Context, s supplier.Supplier, token string, instanceID string) (*statusResponse, error) {
	ctx, cancel := contextWithDefaultTimeout(ctx, statusTimeout)
	defer cancel()

	url := s.BaseURL + "/v1/instances/" + instanceID
	var resp statusResponse
	if err := c.doWithRetry(ctx, "GetStatus("+s.Slug+")", maxRetryAttempts, http.MethodGet, url, nil, &resp, token); err != nil {
		return nil, err
	}
	return &resp, nil
}

// Terminate calls DELETE {baseURL}/v1/instances/{instanceID} on a supplier.
func (c *Client) Terminate(ctx context.Context, s supplier.Supplier, token string, instanceID string) error {
	ctx, cancel := contextWithDefaultTimeout(ctx, terminateTimeout)
	defer cancel()

	url := s.BaseURL + "/v1/instances/" + instanceID
	if err := c.doWithRetry(ctx, "Terminate("+s.Slug+")", maxRetryAttempts, http.MethodDelete, url, nil, nil, token); err != nil {
		return err
	}
	return nil
}
