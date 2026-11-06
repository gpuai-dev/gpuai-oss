package apiclient

import "fmt"

// ProblemError is the decoded RFC 7807 application/problem+json envelope.
// Status is sourced from resp.StatusCode (NOT the body) so callers can branch
// on HTTP semantics. RequestID comes from the X-Request-Id response header.
type ProblemError struct {
	Status    int    `json:"-"`
	Type      string `json:"type"`
	Title     string `json:"title"`
	Detail    string `json:"detail"`
	Code      string `json:"code"`
	RequestID string `json:"request_id"`
}

func (e *ProblemError) Error() string {
	if e.Detail != "" {
		return fmt.Sprintf("%s (%s)", e.Detail, e.Code)
	}
	if e.Title != "" {
		return fmt.Sprintf("%s (%s)", e.Title, e.Code)
	}
	return fmt.Sprintf("HTTP %d (%s)", e.Status, e.Code)
}

// NetworkError wraps a transport-layer failure (DNS, connection refused, TLS).
type NetworkError struct{ Err error }

func (e *NetworkError) Error() string { return "network unreachable: " + e.Err.Error() }
func (e *NetworkError) Unwrap() error { return e.Err }
