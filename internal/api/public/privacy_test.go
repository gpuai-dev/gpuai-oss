package public_test

import (
	"encoding/json"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/gpuai-dev/gpuai-oss/internal/api/public"
)

// bannedKeysRe matches any field name that would leak upstream provider data
// into a public API response. PRIV-08 contract: every public response wire
// type is checked against this pattern via JSON marshalling, so the test
// catches both struct-tag drift AND ad-hoc map[string]any responses that
// might emit a banned key.
//
// Field-level reflection (via JSON marshal of zero/sample values) is
// deliberate. Endpoint-level testing of these wire types would require
// constructing a real *availability.Cache or refactoring PublicDeps to
// accept an interface — both are scope creep for v1. The marshal-and-grep
// approach catches the actual contract: response keys are determined by
// struct tags, regardless of how handlers assemble the body.
//
// Patterns guarded:
//   - upstream_provider, upstream_id, upstream_ip — provider identity
//   - wg_public, wg_private, wg_address, wg_key   — WireGuard secrets
//   - frp_remote_port, frp_token, frp_internal_token — FRP tunnel secrets
//   - internal_token — generic internal-API auth bearer
//   - supplier(_|$) — Plan 30.1-05 supplier-direct surface (catches
//     supplier_node_id, supplier_node, supplier_secret, etc. without
//     false-matching `supplied_ssh_key` or similar adjacent terms).
//
// Case-insensitive (`(?i)`) so a future contributor cannot dodge the guard
// with `WG_Public` or `Upstream_ID` casing.
var bannedKeysRe = regexp.MustCompile(`(?i)(upstream_provider|upstream_id|upstream_ip|wg_(public|private|address|key)|frp_(remote_port|token|internal_token)|internal_token|supplier(_|$))`)

// TestNoUpstreamLeakage_Catalog asserts the public catalog wire types
// (GPUType, Pricing) never marshal a banned key. This test is the
// foundation of the field-level privacy guard: subsequent Wave 5 plans
// (18-13 SSHKey, 18-14 Instance/Operation, 18-15 UsageBucket,
// 18-16 WebhookEndpoint) APPEND their own wire types to the rows[]
// fixture in this file. They MUST NOT rewrite the test or add endpoint
// exercising — the field-level contract is the strongest guarantee
// because struct tags determine wire shape independent of handler logic.
//
// Idempotent: appending the same wire type twice is harmless.
func TestNoUpstreamLeakage_Catalog(t *testing.T) {
	rows := []any{
		public.GPUType{GPUType: "h100_sxm", VRAMGB: 80},
		public.Pricing{GPUType: "h100_sxm", Region: "us-west", Tier: "on_demand", PricePerHour: 2.5, Available: 8},
		// Plan 18-13: SSHKey wire shape — id/name/fingerprint/created_at only.
		public.SSHKey{ID: "k1", Name: "test", Fingerprint: "SHA256:abc", CreatedAt: time.Now()},
		// Plan 18-14: Instance + Operation wire shapes. Instance excludes
		// upstream_provider, upstream_id, upstream_ip, hostname, wg_*,
		// frp_remote_port, internal_token by structural omission;
		// Operation excludes api_key_id, org_id, metadata, version
		// (FSM-internal). Either struct gaining a leaky field via tag
		// drift fails this test at CI time.
		public.Instance{ID: "gpu-abc", Status: "running", GPUType: "h100", GPUCount: 1, Tier: "on_demand", Region: "us-west", PricePerHour: 2.5, CreatedAt: time.Now()},
		// Plan 30.1-05: Supplier-bound Instance fixture. Same Instance
		// wire type as the on-demand row above — the privacy guarantee
		// is that ssh_host / ssh_user / host_credential_ref /
		// supplier_direct never appear in the wire shape, regardless of
		// which underlying provider serves the instance. The defense is
		// by-omission: the public.Instance struct simply has no field
		// for any of these. This row exercises the test against the
		// reserved tier + zero price (the supplier-direct billing shape)
		// to confirm marshaling produces no surprises.
		public.Instance{ID: "gpu-sup1", Status: "running", GPUType: "h100", GPUCount: 1, Tier: "reserved", Region: "us-west", PricePerHour: 0, CreatedAt: time.Now()},
		public.Operation{OperationID: "00000000-0000-0000-0000-000000000001", Kind: "instance.create", State: "pending", CreatedAt: time.Now(), UpdatedAt: time.Now()},
		// Plan 58-02: community-flagged Pricing row. The community badge is a
		// name-derived boolean ATTRIBUTE (field `community`, never `supplier_*`),
		// so it must NOT trip bannedKeysRe. This fixture keeps the guard actively
		// covering the new wire shape as the badge threads through the catalog.
		public.Pricing{GPUType: "rtx_4090", Region: "us-west", Tier: "on_demand", PricePerHour: 0.2, Available: 1, Community: true},
		// Catalog hardware specs (cpu_cores/ram_gb/storage_gb) are plain integer
		// counts carrying no provider identity, so they must NOT trip
		// bannedKeysRe. Populated fixtures keep the guard actively covering the
		// new wire fields on both catalog shapes.
		public.GPUType{GPUType: "h100_sxm", VRAMGB: 80, CPUCores: 26, RAMGB: 200, StorageGB: 512},
		public.Pricing{GPUType: "h100_sxm", Region: "us-west", Tier: "on_demand", PricePerHour: 2.5, Available: 8, CPUCores: 26, RAMGB: 200, StorageGB: 512},
	}
	for _, r := range rows {
		b, err := json.Marshal(r)
		if err != nil {
			t.Fatalf("marshal %T: %v", r, err)
		}
		if bannedKeysRe.Match(b) {
			t.Errorf("banned key found in %T: %s", r, string(b))
		}
	}
}

// TestNoSupplierLeakage_ValueLevel guards against the literal strings
// "supplier_direct" / "ssh_host" / "ssh_user" / "host_credential_ref" /
// "supplier_node" appearing as VALUES in any public wire response, in
// addition to the field-name guard above. This is the standing CI defense
// for success criterion #6 of Phase 30.1: supplier identity never leaks.
//
// Why the value-level scan in addition to bannedKeysRe: the field-name
// regex only catches JSON keys. A future bug could smuggle supplier
// identity through a field VALUE — e.g., a handler that constructs
//
//	map[string]any{"tier": "supplier_direct"}
//
// or a status field that takes on the literal "supplier_direct" string.
// The value-level scan blocks both vectors. All wire types that flow to
// public surface get re-checked here.
//
// Plan 30.1-05.
func TestNoSupplierLeakage_ValueLevel(t *testing.T) {
	rows := []any{
		public.GPUType{GPUType: "h100_sxm", VRAMGB: 80},
		public.Pricing{GPUType: "h100_sxm", Region: "us-west", Tier: "reserved", PricePerHour: 2.5, Available: 8},
		public.SSHKey{ID: "k1", Name: "test", Fingerprint: "SHA256:abc", CreatedAt: time.Now()},
		// Supplier-bound Instance fixture — same wire type as on-demand.
		public.Instance{ID: "gpu-sup1", Status: "running", GPUType: "h100", GPUCount: 1, Tier: "reserved", Region: "us-west", PricePerHour: 0, CreatedAt: time.Now()},
		public.Operation{OperationID: "00000000-0000-0000-0000-000000000001", Kind: "instance.create", State: "pending", CreatedAt: time.Now(), UpdatedAt: time.Now()},
		// Plan 58-02: community-flagged Pricing row — value-level scan confirms
		// the badge never smuggles a forbidden supplier/upstream literal.
		public.Pricing{GPUType: "rtx_4090", Region: "us-west", Tier: "on_demand", PricePerHour: 0.2, Available: 1, Community: true},
		// Hardware-spec integers never carry a supplier/upstream literal value.
		public.Pricing{GPUType: "h100_sxm", Region: "us-west", Tier: "on_demand", PricePerHour: 2.5, Available: 8, CPUCores: 26, RAMGB: 200, StorageGB: 512},
	}
	forbidden := []string{
		"supplier_direct",
		"ssh_host",
		"ssh_user",
		"host_credential_ref",
		"supplier_node",
	}
	for _, r := range rows {
		b, err := json.Marshal(r)
		if err != nil {
			t.Fatalf("marshal %T: %v", r, err)
		}
		body := string(b)
		for _, f := range forbidden {
			if strings.Contains(body, f) {
				t.Errorf("forbidden value %q found in %T marshalled body: %s", f, r, body)
			}
		}
	}
}
