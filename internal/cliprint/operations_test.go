package cliprint

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/gpuai-dev/gpuai-oss/internal/apiclient"
)

func baseOperation() apiclient.Operation {
	return apiclient.Operation{
		OperationID: "op_abc123",
		Kind:        "instance.create",
		State:       "pending",
		CreatedAt:   time.Date(2026, 7, 29, 10, 0, 0, 0, time.UTC),
		UpdatedAt:   time.Date(2026, 7, 29, 10, 0, 30, 0, time.UTC),
	}
}

func succeededOperation() apiclient.Operation {
	op := baseOperation()
	op.State = "succeeded"
	resourceID := "inst_xyz789"
	op.ResourceID = &resourceID
	completed := time.Date(2026, 7, 29, 10, 2, 0, 0, time.UTC)
	op.CompletedAt = &completed
	return op
}

func failedOperation() apiclient.Operation {
	op := baseOperation()
	op.State = "failed"
	op.Error = &apiclient.OpError{Code: "capacity_unavailable", Detail: "no H100 capacity in us-west-2"}
	completed := time.Date(2026, 7, 29, 10, 1, 0, 0, time.UTC)
	op.CompletedAt = &completed
	return op
}

func TestOperation_TableSucceeded(t *testing.T) {
	var b bytes.Buffer
	if err := Operation(FormatTable, &b, succeededOperation()); err != nil {
		t.Fatalf("Operation: %v", err)
	}
	out := b.String()
	for _, want := range []string{
		"Operation ID:", "op_abc123",
		"Kind:", "instance.create",
		"State:", "succeeded",
		"Resource ID:", "inst_xyz789",
		"Created:", "2026-07-29T10:00:00Z",
		"Updated:", "2026-07-29T10:00:30Z",
		"Completed:", "2026-07-29T10:02:00Z",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("table output missing %q\n---\n%s", want, out)
		}
	}
}

func TestOperation_TableFailedShowsError(t *testing.T) {
	var b bytes.Buffer
	if err := Operation(FormatTable, &b, failedOperation()); err != nil {
		t.Fatalf("Operation: %v", err)
	}
	out := b.String()
	for _, want := range []string{
		"State:", "failed",
		"Error Code:", "capacity_unavailable",
		"Error Detail:", "no H100 capacity in us-west-2",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("table output missing %q\n---\n%s", want, out)
		}
	}
	if strings.Contains(out, "Resource ID") {
		t.Errorf("failed op has no resource id — line should be omitted\n%s", out)
	}
}

// TestOperation_TablePendingNilPointers proves the nil-guards: a pending
// operation carries no resource id, error, result, or completion time.
func TestOperation_TablePendingNilPointers(t *testing.T) {
	var b bytes.Buffer
	if err := Operation(FormatTable, &b, baseOperation()); err != nil {
		t.Fatalf("Operation: %v", err)
	}
	out := b.String()
	for _, want := range []string{"Operation ID:", "Kind:", "State:", "pending", "Created:", "Updated:"} {
		if !strings.Contains(out, want) {
			t.Errorf("table output missing %q\n---\n%s", want, out)
		}
	}
	for _, unwanted := range []string{"Resource ID", "Error Code", "Error Detail", "Completed:"} {
		if strings.Contains(out, unwanted) {
			t.Errorf("pending op should not render %q\n---\n%s", unwanted, out)
		}
	}
}

// TestOperation_TableNeverRendersResult keeps the result payload out of this
// renderer; app coordinates belong to `gpu instances get/open`. (OperationResult
// has no password field at all; the server never sends it.)
func TestOperation_TableNeverRendersResult(t *testing.T) {
	op := succeededOperation()
	op.Result = &apiclient.OperationResult{
		AppURL:           "https://app.example.com",
		AppBasicAuthUser: "gpuai-deploy-user",
	}
	var b bytes.Buffer
	if err := Operation(FormatTable, &b, op); err != nil {
		t.Fatalf("Operation: %v", err)
	}
	for _, leak := range []string{"https://app.example.com", "gpuai-deploy-user"} {
		if strings.Contains(b.String(), leak) {
			t.Errorf("table mode leaked the operation result field %q:\n%s", leak, b.String())
		}
	}
}

func TestOperation_JSON(t *testing.T) {
	for _, format := range []string{FormatJSON, ""} {
		var b bytes.Buffer
		if err := Operation(format, &b, failedOperation()); err != nil {
			t.Fatalf("Operation(%q): %v", format, err)
		}
		var decoded apiclient.Operation
		if err := json.Unmarshal(b.Bytes(), &decoded); err != nil {
			t.Fatalf("operation json is not valid JSON: %v\n%s", err, b.String())
		}
		if decoded.State != "failed" || decoded.OperationID != "op_abc123" {
			t.Errorf("json did not round-trip the operation: %+v", decoded)
		}
		if decoded.Error == nil || decoded.Error.Code != "capacity_unavailable" {
			t.Errorf("json did not round-trip the error payload: %+v", decoded.Error)
		}
	}
}

func TestOperation_UnknownFormat(t *testing.T) {
	var b bytes.Buffer
	if err := Operation("yaml", &b, baseOperation()); err == nil {
		t.Error("expected unknown-format error")
	}
}
