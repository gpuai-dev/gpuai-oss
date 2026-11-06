// Package apiclient is a hand-rolled typed client for the GPU.ai Open Source
// API (/v1/*). Each endpoint is one method on Client; the generic do[T]
// core handles JSON marshalling, RFC 7807 problem responses, and request
// IDs.
package apiclient

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"runtime"
	"strings"
	"time"
)

// Client is a hand-rolled typed API client for the /v1/* API. Construct via
// New; each endpoint has one method that delegates to the generic do[T] core.
type Client struct {
	BaseURL    string
	Token      string
	HTTPClient *http.Client
	Logger     *slog.Logger
	UserAgent  string
}

// New constructs a Client. baseURL trailing slashes are trimmed. logger may be
// nil — a discard logger is used in that case.
func New(baseURL, token string, logger *slog.Logger) *Client {
	if logger == nil {
		logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	return &Client{
		BaseURL:    strings.TrimRight(baseURL, "/"),
		Token:      token,
		HTTPClient: &http.Client{Timeout: 30 * time.Second},
		Logger:     logger,
		UserAgent:  fmt.Sprintf("gpu/dev (%s/%s)", runtime.GOOS, runtime.GOARCH),
	}
}

// WithUserAgent overrides the default UA (used by main.go to inject the
// ldflag-injected version once known).
func (c *Client) WithUserAgent(ua string) *Client {
	c.UserAgent = ua
	return c
}

// do is the generic core. Returns *ProblemError on 4xx/5xx (decoded from the
// RFC 7807 envelope), *NetworkError on transport failures, or T on success.
// On 204 No Content, returns zero value + nil error.
//
// On non-2xx, ProblemError.Status is set from resp.StatusCode and RequestID
// is sourced from the X-Request-Id response header so it shows up in
// mapClientError even when the server omits it from the body.
//
// The second return value is the value of the Operation-Id response header
// (empty when absent). Callers that POST/DELETE async resources use this to
// poll /operations/{id}.
func do[T any](ctx context.Context, c *Client, method, path string, body any, extraHeaders map[string]string) (T, string, error) {
	var zero T
	var rdr io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return zero, "", err
		}
		rdr = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.BaseURL+path, rdr)
	if err != nil {
		return zero, "", err
	}
	if c.Token != "" {
		req.Header.Set("Authorization", "Bearer "+c.Token)
	}
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set("User-Agent", c.UserAgent)
	for k, v := range extraHeaders {
		req.Header.Set(k, v)
	}
	c.Logger.Debug("http", "method", method, "url", req.URL.String())
	resp, err := c.HTTPClient.Do(req)
	if err != nil {
		return zero, "", &NetworkError{Err: err}
	}
	defer resp.Body.Close()
	opID := resp.Header.Get("Operation-Id")
	if resp.StatusCode >= 400 {
		var p ProblemError
		_ = json.NewDecoder(resp.Body).Decode(&p)
		p.Status = resp.StatusCode
		if p.RequestID == "" {
			p.RequestID = resp.Header.Get("X-Request-Id")
		}
		return zero, opID, &p
	}
	if resp.StatusCode == http.StatusNoContent {
		return zero, opID, nil
	}
	var out T
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return zero, opID, err
	}
	return out, opID, nil
}
