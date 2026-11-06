package main

import (
	"strings"
	"testing"

	"github.com/gpuai-dev/gpuai-oss/internal/apiclient"
)

// A route that is not registered answers with Go's plain-text 404, which
// decodes into a ProblemError with nothing in it. That is the shape the CLI
// sees when it calls an endpoint the server does not register (a disabled
// feature, or a CLI newer than the server), and it must not be reported as a
// missing resource.
func TestMapClientError_EmptyNotFoundIsADisabledFeature(t *testing.T) {
	err := mapClientError(&apiclient.ProblemError{Status: 404})

	got := err.Error()
	if strings.HasPrefix(got, "not found:") {
		t.Fatalf("empty 404 rendered as a missing resource: %q", got)
	}
	if !strings.Contains(got, "not enabled on this deployment") {
		t.Errorf("message does not name the likely cause: %q", got)
	}
}

// The other half: a handler's 404 still reads as a missing resource. Instances
// are addressed by id, and "instance not found" is a real answer the CLI must
// keep giving.
func TestMapClientError_HandlerNotFoundStillReadsAsMissing(t *testing.T) {
	err := mapClientError(&apiclient.ProblemError{
		Status: 404,
		Title:  "Not Found",
		Detail: "instance not found",
		Code:   "not_found",
	})

	got := err.Error()
	if !strings.Contains(got, "instance not found") {
		t.Fatalf("handler detail was swallowed: %q", got)
	}
	if strings.Contains(got, "not enabled on this deployment") {
		t.Errorf("a real missing resource was reported as a disabled feature: %q", got)
	}
}
