package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

// TestInstancesCreate_AutoTerminateHoursFlag pins the CLI half of the
// flag reaches the wire when set, and — the case that matters — is ABSENT when
// it is not. The server refuses an explicit 0 (it is a broken request, not "no
// limit"), so a CLI that sent its own flag default would make every launch fail
// for a limit the user never asked for.
func TestInstancesCreate_AutoTerminateHoursFlag(t *testing.T) {
	cases := []struct {
		name        string
		args        []string
		wantPresent bool
		wantHours   float64
	}{
		{name: "flag omitted sends nothing", args: nil, wantPresent: false},
		{name: "explicit zero sends nothing", args: []string{"--auto-terminate-hours", "0"}, wantPresent: false},
		{name: "explicit hours are sent", args: []string{"--auto-terminate-hours", "8"}, wantPresent: true, wantHours: 8},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var body map[string]any
			mux := http.NewServeMux()
			mux.HandleFunc("/instances", func(w http.ResponseWriter, r *http.Request) {
				_ = json.NewDecoder(r.Body).Decode(&body)
				w.Header().Set("Operation-Id", "op-1")
				w.WriteHeader(http.StatusAccepted)
				_ = json.NewEncoder(w).Encode(map[string]any{
					"operation_id": "op-1", "kind": "instance.create", "state": "pending",
					"created_at": "2026-09-02T00:00:00Z", "updated_at": "2026-09-02T00:00:00Z",
				})
			})
			srv := httptest.NewServer(mux)
			defer srv.Close()
			t.Setenv("GPUAI_API_KEY", "gpuai_test_key")
			t.Setenv("GPUAI_API_BASE", srv.URL)
			t.Setenv("XDG_CONFIG_HOME", t.TempDir())

			root := newRootCmd()
			// --max-price 0 opts out of the pricing round-trip so this test
			// exercises only the field under test.
			args := append([]string{"instances", "create", "-t", "h100_sxm",
				"--region", "us-east", "--max-price", "0", "--wait-timeout", "0", "--output", "json"}, tc.args...)
			root.SetArgs(args)
			out, errOut := &bytes.Buffer{}, &bytes.Buffer{}
			root.SetOut(out)
			root.SetErr(errOut)
			if err := root.Execute(); err != nil {
				t.Fatalf("execute: %v (stderr: %s)", err, errOut.String())
			}

			got, present := body["auto_terminate_hours"]
			if !tc.wantPresent {
				if present {
					t.Fatalf("auto_terminate_hours must be absent, got %v — the server refuses 0 and "+
						"an unasked-for limit would break every launch", got)
				}
				return
			}
			if !present {
				t.Fatalf("auto_terminate_hours missing from the request body: %v", body)
			}
			if got.(float64) != tc.wantHours {
				t.Errorf("auto_terminate_hours = %v, want %v", got, tc.wantHours)
			}
		})
	}
}
