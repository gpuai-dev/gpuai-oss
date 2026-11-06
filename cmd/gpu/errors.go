package main

import (
	"errors"
	"fmt"

	"github.com/gpuai-dev/gpuai-oss/internal/apiclient"
)

// mapClientError converts apiclient errors to user-facing messages. Called by
// every RunE that issues HTTP requests. Centralized so error UX evolves in
// one place.
//
// The 401 branch is intentional: it tells the user EXACTLY what to do (run
// `gpu login` or set GPUAI_API_KEY) — never just "unauthenticated".
func mapClientError(err error) error {
	if err == nil {
		return nil
	}
	var pe *apiclient.ProblemError
	if errors.As(err, &pe) {
		switch pe.Status {
		case 401:
			return fmt.Errorf("not authenticated — run `gpu login` or set GPUAI_API_KEY")
		case 403:
			if pe.Code == "insufficient_scope" {
				return fmt.Errorf("API key lacks required scope (%s) — create a new key with `gpuctl apikey create`", pe.Detail)
			}
			if pe.Code == "quota_exceeded" {
				return fmt.Errorf("quota exceeded — %s", pe.Detail)
			}
			return fmt.Errorf("forbidden: %s (request_id=%s)", pe.Detail, pe.RequestID)
		case 404:
			// A 404 with a COMPLETELY empty problem body did not come from a
			// handler — every handler that refuses a resource fills in at least
			// a title. It came from the mux, which means the route is not
			// registered on that server at all: a feature switched off for this
			// deployment, or a CLI newer than the API it is pointed at.
			//
			// "not found: " with nothing after the colon is the alternative, and
			// it reads as a missing resource, sending someone to hunt for an id
			// that was never the problem.
			if pe.Title == "" && pe.Detail == "" && pe.Code == "" {
				return fmt.Errorf("this server has no such endpoint — the feature is not enabled on this deployment, or the CLI is newer than the API it is pointed at")
			}
			return fmt.Errorf("not found: %s", pe.Detail)
		case 400, 422:
			return fmt.Errorf("validation failed: %s", pe.Detail)
		case 429:
			return fmt.Errorf("rate limited — retry shortly")
		case 409:
			return fmt.Errorf("conflict: %s (%s)", pe.Detail, pe.Code)
		default:
			return fmt.Errorf("%s (status=%d, code=%s, request_id=%s)", pe.Detail, pe.Status, pe.Code, pe.RequestID)
		}
	}
	var ne *apiclient.NetworkError
	if errors.As(err, &ne) {
		return fmt.Errorf("could not reach the API server (%s) — check that gpuctl is running, or set --api-base / GPUAI_API_BASE", ne.Err)
	}
	return err
}
