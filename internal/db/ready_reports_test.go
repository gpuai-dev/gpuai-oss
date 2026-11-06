package db

import (
	"context"
	"os"
	"strings"
	"testing"
)

func TestReadyReportNormalization(t *testing.T) {
	for _, bad := range []map[string]string{
		{"token": "do-not-store"}, {"driver_version": strings.Repeat("x", 129)},
		{"docker_up": "unknown"}, {"cuda_version": "12.4\nextra"},
	} {
		if _, err := normalizeReadyReport(bad); err == nil {
			t.Fatalf("accepted unsupported report fields")
		}
	}
	got, err := normalizeReadyReport(map[string]string{"driver_version": "550.54.14", "container_id": ""})
	if err != nil || len(got) != 1 {
		t.Fatal(got, err)
	}
}

// The startup GPU check reports with the ready callback; without these keys
// every report sent while the check is on would be rejected whole.
func TestReadyReportKeepsTheGPUCheckResult(t *testing.T) {
	got, err := normalizeReadyReport(map[string]string{"driver_version": "580.126.09", "docker_up": "false",
		"gpu_check": "fail", "gpu_check_mode": "enforce", "gpus_seen": "1",
		"gpu_check_detail": "host: GPU 0 compute check failed: context returned 999, nvidia-uvm open: Input/output error"})
	if err != nil || len(got) != 6 || got["gpu_check"] != "fail" || !strings.Contains(got["gpu_check_detail"], "Input/output error") {
		t.Fatalf("check result not kept: %v %v", got, err)
	}
	// warn: a framework the environment did not promise could not use the GPU
	// (reported, never blocking). Rejecting it would drop the whole report.
	if got, err := normalizeReadyReport(map[string]string{"gpu_check": "warn", "gpu_check_detail": "environment: torch 2.4.1+cpu cannot use the GPU"}); err != nil || got["gpu_check"] != "warn" {
		t.Fatalf("warn result not kept: %v %v", got, err)
	}
	for _, bad := range []map[string]string{
		{"gpu_check": "maybe"}, {"gpu_check_mode": "always"}, {"gpus_seen": "1;"},
		{"gpu_check_detail": `fail" injected`}, {"gpu_check_detail": strings.Repeat("x", 161)},
	} {
		if _, err := normalizeReadyReport(bad); err == nil {
			t.Errorf("accepted %v", bad)
		}
	}
}

func readyReportFixture(t *testing.T) (*Pool, string, string) {
	t.Helper()
	url := os.Getenv("DATABASE_TEST_URL")
	if url == "" {
		t.Skip("DATABASE_TEST_URL not set")
	}
	return rescueEventFixture(t, context.Background(), url, "readyreport")
}
