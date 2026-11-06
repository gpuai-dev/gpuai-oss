package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestInstancesList_JSONOutput(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/instances" {
			t.Errorf("path: %q", r.URL.Path)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"data": []map[string]any{{
				"id": "gpu-abc", "status": "running", "gpu_type": "h100_sxm",
				"gpu_count": 1, "tier": "on_demand", "region": "us-east",
				"price_per_hour": 2.5, "created_at": "2026-05-14T00:00:00Z",
			}},
			"next_cursor": nil,
		})
	}))
	defer srv.Close()
	t.Setenv("GPUAI_API_KEY", "gpuai_test_key")
	t.Setenv("GPUAI_API_BASE", srv.URL)
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())

	root := newRootCmd()
	root.SetArgs([]string{"instances", "list", "--output", "json"})
	buf := &bytes.Buffer{}
	root.SetOut(buf)
	root.SetErr(buf)
	if err := root.Execute(); err != nil {
		t.Fatalf("execute: %v", err)
	}
	if !strings.Contains(buf.String(), "gpu-abc") {
		t.Errorf("missing id in output: %s", buf.String())
	}
}

func TestInstancesList_401SuggestsLogin(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/problem+json")
		w.WriteHeader(http.StatusUnauthorized)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"type":   "https://errors.example.com/unauthenticated",
			"title":  "Unauthenticated",
			"detail": "invalid token",
			"code":   "unauthenticated",
		})
	}))
	defer srv.Close()
	t.Setenv("GPUAI_API_KEY", "gpuai_test_key")
	t.Setenv("GPUAI_API_BASE", srv.URL)
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())

	root := newRootCmd()
	root.SetArgs([]string{"instances", "list"})
	buf := &bytes.Buffer{}
	root.SetOut(buf)
	root.SetErr(buf)
	err := root.Execute()
	if err == nil {
		t.Fatal("expected error from 401")
	}
	if !strings.Contains(err.Error(), "gpu login") {
		t.Errorf("expected `gpu login` hint, got: %v", err)
	}
}

// TestInstancesCreate_PriceCapContract pins the placement-mode price
// contract: an unpinned create with no explicit --max-price defaults the cap
// to the cheapest currently-listed matching row (count-filtered,
// availability-filtered, disk uplift folded in); --max-price 0 opts out;
// a pinned launch sends no derived cap; an explicit cap passes through.
func TestInstancesCreate_PriceCapContract(t *testing.T) {
	pricingRows := []map[string]any{
		// cheapest matching 1-GPU row — the expected default cap basis
		{"gpu_type": "h100_sxm", "gpu_count": 1, "region": "us-east", "tier": "on_demand",
			"price_per_hour": 2.0, "available": 3, "instance_disk_gb": 100,
			"disk_configurable": true, "disk_price_per_gb_hour": 0.001, "offering_id": "aaa"},
		// cheaper but zero availability — must be skipped
		{"gpu_type": "h100_sxm", "gpu_count": 1, "region": "us-east", "tier": "on_demand",
			"price_per_hour": 1.4, "available": 0, "offering_id": "bbb"},
		// larger node — qualifies for smaller requests (placement matches
		// GPUCount >=) but is never the cheapest 1-GPU basis here
		{"gpu_type": "h100_sxm", "gpu_count": 8, "region": "us-east", "tier": "on_demand",
			"price_per_hour": 10.0, "available": 5, "offering_id": "ccc"},
		// non-cent-aligned disk economics (sub-cent per-GB rates): row 2.29 +
		// 100 GB extra x 0.000115 = 2.3015 raw. The server cent-rounds its
		// own single fold (2.31 with the same inputs), so the derived cap
		// must ceil to 2.31 — a raw 2.3015 cap would spuriously price-fail
		// the very row it was quoted from.
		{"gpu_type": "l40s", "gpu_count": 1, "region": "us-east", "tier": "on_demand",
			"price_per_hour": 2.29, "available": 2, "instance_disk_gb": 100,
			"disk_configurable": true, "disk_price_per_gb_hour": 0.000115, "offering_id": "ddd"},
	}
	cases := []struct {
		name    string
		args    []string
		wantCap *float64
		// wantViewed is the expected viewed_price_per_hour on the wire.
		// nil on every pre-existing case on purpose: the flag is explicit-only,
		// so a CLI launch that doesn't pass it must be byte-for-byte what it was
		// before the field existed.
		wantViewed *float64
		wantPin    string
		wantHits   int // expected /pricing calls
		wantErr    bool
	}{
		{name: "default caps at cheapest match",
			args: []string{}, wantCap: f64p(2.0), wantHits: 1},
		{name: "disk uplift folds into the default cap",
			args: []string{"--disk", "200"}, wantCap: f64p(2.1), wantHits: 1},
		{name: "non-cent-aligned disk fold ceils to the next cent",
			args: []string{"-t", "l40s", "--disk", "200"}, wantCap: f64p(2.31), wantHits: 1},
		{name: "larger nodes qualify when no exact-count row exists",
			args: []string{"--count", "4"}, wantCap: f64p(10.0), wantHits: 1},
		{name: "explicit zero opts out",
			args: []string{"--max-price", "0"}, wantCap: nil, wantHits: 0},
		{name: "explicit cap passes through",
			args: []string{"--max-price", "1.5"}, wantCap: f64p(1.5), wantHits: 0},
		{name: "negative cap is a validation error",
			args: []string{"--max-price", "-1"}, wantErr: true},
		{name: "pinned launch derives no cap",
			args: []string{"--offering-id", "aaa"}, wantCap: nil, wantPin: "aaa", wantHits: 0},
		// The viewed price is a separate contract from the cap: it can
		// only refuse a launch whose price moved, never authorize a bigger buy,
		// so it rides alongside the pin without becoming one.
		{name: "viewed price rides the pin without becoming a cap",
			args:    []string{"--offering-id", "aaa", "--viewed-price", "2.0"},
			wantCap: nil, wantViewed: f64p(2.0), wantPin: "aaa", wantHits: 0},
		{name: "zero viewed price is not sent",
			args:    []string{"--offering-id", "aaa", "--viewed-price", "0"},
			wantCap: nil, wantViewed: nil, wantPin: "aaa", wantHits: 0},
		{name: "negative viewed price is a validation error",
			args: []string{"--viewed-price", "-1"}, wantErr: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var body map[string]any
			pricingHits := 0
			mux := http.NewServeMux()
			mux.HandleFunc("/pricing", func(w http.ResponseWriter, r *http.Request) {
				pricingHits++
				// Mirror the real server's gpu_type filter — quoteCheapest
				// relies on it rather than re-filtering client-side.
				gt := r.URL.Query().Get("gpu_type")
				filtered := make([]map[string]any, 0, len(pricingRows))
				for _, row := range pricingRows {
					if gt == "" || row["gpu_type"] == gt {
						filtered = append(filtered, row)
					}
				}
				_ = json.NewEncoder(w).Encode(map[string]any{"data": filtered, "next_cursor": nil})
			})
			mux.HandleFunc("/instances", func(w http.ResponseWriter, r *http.Request) {
				_ = json.NewDecoder(r.Body).Decode(&body)
				w.Header().Set("Operation-Id", "op-1")
				w.WriteHeader(http.StatusAccepted)
				_ = json.NewEncoder(w).Encode(map[string]any{
					"operation_id": "op-1", "kind": "instance.create", "state": "pending",
					"created_at": "2026-08-05T00:00:00Z", "updated_at": "2026-08-05T00:00:00Z",
				})
			})
			srv := httptest.NewServer(mux)
			defer srv.Close()
			t.Setenv("GPUAI_API_KEY", "gpuai_test_key")
			t.Setenv("GPUAI_API_BASE", srv.URL)
			t.Setenv("XDG_CONFIG_HOME", t.TempDir())

			root := newRootCmd()
			args := append([]string{"instances", "create", "-t", "h100_sxm",
				"--region", "us-east", "--wait-timeout", "0", "--output", "json"}, tc.args...)
			root.SetArgs(args)
			out, errOut := &bytes.Buffer{}, &bytes.Buffer{}
			root.SetOut(out)
			root.SetErr(errOut)
			err := root.Execute()
			if tc.wantErr {
				if err == nil {
					t.Fatalf("want a validation error, got success (body: %v)", body)
				}
				if body != nil {
					t.Fatalf("validation must fail before any create request, got body %v", body)
				}
				return
			}
			if err != nil {
				t.Fatalf("execute: %v (stderr: %s)", err, errOut.String())
			}

			got, present := body["max_price_per_hour"]
			if tc.wantCap == nil {
				if present {
					t.Errorf("max_price_per_hour must be absent, got %v", got)
				}
			} else if !present || got.(float64) != *tc.wantCap {
				t.Errorf("max_price_per_hour: want %v, got %v (present=%v)", *tc.wantCap, got, present)
			}
			gotViewed, viewedPresent := body["viewed_price_per_hour"]
			if tc.wantViewed == nil {
				if viewedPresent {
					t.Errorf("viewed_price_per_hour must be absent without an explicit --viewed-price, got %v", gotViewed)
				}
			} else if !viewedPresent || gotViewed.(float64) != *tc.wantViewed {
				t.Errorf("viewed_price_per_hour: want %v, got %v (present=%v)", *tc.wantViewed, gotViewed, viewedPresent)
			}
			if tc.wantPin != "" && body["offering_id"] != tc.wantPin {
				t.Errorf("offering_id: want %q, got %v", tc.wantPin, body["offering_id"])
			}
			if pricingHits != tc.wantHits {
				t.Errorf("pricing lookups: want %d, got %d", tc.wantHits, pricingHits)
			}
			// The placement mode must be stated on stderr in every mode.
			if !strings.Contains(errOut.String(), "launch") {
				t.Errorf("placement-mode message missing from stderr: %q", errOut.String())
			}
		})
	}
}

func f64p(v float64) *float64 { return &v }

func TestInstancesCreate_PollsToSuccess(t *testing.T) {
	mux := http.NewServeMux()
	resourceID := "gpu-new-abc"
	mux.HandleFunc("/instances", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("expected POST, got %s", r.Method)
		}
		w.Header().Set("Operation-Id", "op-create-1")
		w.WriteHeader(http.StatusAccepted)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"operation_id": "op-create-1", "kind": "instance.create",
			"state":      "pending",
			"created_at": "2026-05-14T00:00:00Z", "updated_at": "2026-05-14T00:00:00Z",
		})
	})
	mux.HandleFunc("/operations/op-create-1", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"operation_id": "op-create-1", "kind": "instance.create",
			"state": "succeeded", "resource_id": resourceID,
			"created_at": "2026-05-14T00:00:00Z", "updated_at": "2026-05-14T00:00:05Z",
		})
	})
	mux.HandleFunc("/instances/"+resourceID, func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"id": resourceID, "status": "running", "gpu_type": "h100_sxm",
			"gpu_count": 1, "tier": "on_demand", "region": "us-east",
			"price_per_hour": 2.5, "created_at": "2026-05-14T00:00:00Z",
		})
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	t.Setenv("GPUAI_API_KEY", "gpuai_test_key")
	t.Setenv("GPUAI_API_BASE", srv.URL)
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())

	root := newRootCmd()
	root.SetArgs([]string{"instances", "create", "-t", "h100_sxm", "--output", "json"})
	buf := &bytes.Buffer{}
	root.SetOut(buf)
	root.SetErr(buf)
	if err := root.Execute(); err != nil {
		t.Fatalf("execute: %v", err)
	}
	if !strings.Contains(buf.String(), resourceID) {
		t.Errorf("missing resource id in output: %s", buf.String())
	}
}

// --entrypoint "" (single empty element, the docker-CLI clear spelling) must
// map to the API's clear idiom — an explicit empty array — because the API
// rejects empty argv ELEMENTS. Any other argv passes through verbatim.
func TestInstancesCreate_EntrypointClearMapping(t *testing.T) {
	cases := []struct {
		name     string
		args     []string
		want     []any // expected wire argv; nil = field absent
		wantSent bool
		field    string // wire field to assert; "" = entrypoint
	}{
		{name: "single empty element clears",
			args: []string{"--entrypoint", ""}, want: []any{}, wantSent: true},
		{name: "real argv passes through",
			args: []string{"--entrypoint", "python", "--entrypoint", "-u"}, want: []any{"python", "-u"}, wantSent: true},
		{name: "absent flag sends nothing", args: nil, wantSent: false},
		{name: "single empty cmd element clears",
			args: []string{"--cmd", ""}, want: []any{}, wantSent: true, field: "cmd"},
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
					"created_at": "2026-08-05T00:00:00Z", "updated_at": "2026-08-05T00:00:00Z",
				})
			})
			srv := httptest.NewServer(mux)
			defer srv.Close()
			t.Setenv("GPUAI_API_KEY", "gpuai_test_key")
			t.Setenv("GPUAI_API_BASE", srv.URL)
			t.Setenv("XDG_CONFIG_HOME", t.TempDir())

			root := newRootCmd()
			args := append([]string{"instances", "create", "-t", "h100_sxm",
				"--image", "ghcr.io/acme/app:v1", "--max-price", "0",
				"--wait-timeout", "0", "--output", "json"}, tc.args...)
			root.SetArgs(args)
			out, errOut := &bytes.Buffer{}, &bytes.Buffer{}
			root.SetOut(out)
			root.SetErr(errOut)
			if err := root.Execute(); err != nil {
				t.Fatalf("execute: %v (stderr: %s)", err, errOut.String())
			}
			field := tc.field
			if field == "" {
				field = "entrypoint"
			}
			got, present := body[field]
			if !tc.wantSent {
				if present {
					t.Fatalf("%s must be absent, got %v", field, got)
				}
				return
			}
			gotArr, ok := got.([]any)
			if !present || !ok {
				t.Fatalf("entrypoint: want %v, got %v (present=%v)", tc.want, got, present)
			}
			if len(gotArr) != len(tc.want) {
				t.Fatalf("entrypoint: want %v, got %v", tc.want, gotArr)
			}
			for i := range gotArr {
				if gotArr[i] != tc.want[i] {
					t.Fatalf("entrypoint[%d]: want %v, got %v", i, tc.want[i], gotArr[i])
				}
			}
		})
	}
}
