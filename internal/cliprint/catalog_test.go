package cliprint

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"github.com/gpuai-dev/gpuai-oss/internal/apiclient"
)

func samplePricingRows() []apiclient.PricingRow {
	return []apiclient.PricingRow{
		{GPUType: "rtx_4090", GPUCount: 1, Region: "us-west", Tier: "on_demand", PricePerHour: 0.20, Available: 1, InstantBoot: true, Community: true},
		{GPUType: "h100_sxm", GPUCount: 8, Region: "us-east", Tier: "on_demand", PricePerHour: 24.00, Available: 5, InstantBoot: true, Community: false},
	}
}

// TestPricing_Table_SourceColumn asserts the table renderer prints a SOURCE
// header and labels each row by capacity class — the server's per-offering
// capacity_class verbatim when present, else the Community-bool fallback
// (`community` / `secure`) for older servers. A class label only; never
// supplier identity.
func TestPricing_Table_SourceColumn(t *testing.T) {
	var buf bytes.Buffer
	if err := Pricing(FormatTable, &buf, samplePricingRows()); err != nil {
		t.Fatalf("Pricing: %v", err)
	}
	out := buf.String()
	for _, want := range []string{"SOURCE", "community", "secure"} {
		if !strings.Contains(out, want) {
			t.Errorf("table missing %q\n%s", want, out)
		}
	}

	// The server-sent capacity_class wins over the bool-derived fallback.
	rows := []apiclient.PricingRow{
		{GPUType: "rtx_4090", GPUCount: 1, Region: "us-west", Tier: "spot", PricePerHour: 0.10, Available: 2, InstantBoot: true, Community: true, CapacityClass: "community"},
		{GPUType: "rtx_4090", GPUCount: 1, Region: "us-west", Tier: "spot", PricePerHour: 0.14, Available: 3, InstantBoot: true, Community: false, CapacityClass: "secure"},
	}
	buf.Reset()
	if err := Pricing(FormatTable, &buf, rows); err != nil {
		t.Fatalf("Pricing: %v", err)
	}
	out = buf.String()
	if !strings.Contains(out, "community") || !strings.Contains(out, "secure") {
		t.Errorf("capacity_class labels missing\n%s", out)
	}
}

// TestCatalogRenderCertified pins the pricing-table surface: the table gains an ENV column
// (certified / raw-vm / provider-template, derived from CertifiedImage +
// DeploymentType — a class label, never provider identity) and a TYPE column
// (vm / container, from DeploymentType). Clones the BOOT/SOURCE column shape.
func TestCatalogRenderCertified(t *testing.T) {
	rows := []apiclient.PricingRow{
		// certified-capable VM: normalized to the Certified Image.
		{GPUType: "h100_sxm", GPUCount: 1, Region: "us-east", Tier: "on_demand", PricePerHour: 2.50, Available: 4, InstantBoot: true, CertifiedImage: true, DeploymentType: "vm"},
		// raw-VM: a VM we bootstrap but that cannot take the certified image.
		{GPUType: "a100_pcie", GPUCount: 1, Region: "us-west", Tier: "on_demand", PricePerHour: 1.20, Available: 8, InstantBoot: false, CertifiedImage: false, DeploymentType: "vm"},
		// container-native: provider runs our image; template comes from provider.
		{GPUType: "l4", GPUCount: 1, Region: "eu-west", Tier: "on_demand", PricePerHour: 0.50, Available: 16, InstantBoot: true, CertifiedImage: false, DeploymentType: "container"},
	}
	var buf bytes.Buffer
	if err := Pricing(FormatTable, &buf, rows); err != nil {
		t.Fatalf("Pricing: %v", err)
	}
	out := buf.String()
	// ENV column header + the three class labels.
	for _, want := range []string{"ENV", "certified", "raw-vm", "provider-template"} {
		if !strings.Contains(out, want) {
			t.Errorf("table missing ENV-column token %q\n%s", want, out)
		}
	}
	// TYPE column header + both deployment classes.
	for _, want := range []string{"TYPE", "vm", "container"} {
		if !strings.Contains(out, want) {
			t.Errorf("table missing TYPE-column token %q\n%s", want, out)
		}
	}
}

// TestPricing_JSON_CommunityRoundTrip asserts the community bool survives the
// JSON output path automatically via the struct tag (no renderer change).
func TestPricing_JSON_CommunityRoundTrip(t *testing.T) {
	var buf bytes.Buffer
	if err := Pricing(FormatJSON, &buf, samplePricingRows()); err != nil {
		t.Fatalf("Pricing JSON: %v", err)
	}
	var got []apiclient.PricingRow
	if err := json.Unmarshal(buf.Bytes(), &got); err != nil {
		t.Fatalf("invalid JSON: %v", err)
	}
	if len(got) != 2 || !got[0].Community || got[1].Community {
		t.Errorf("community field did not round-trip: %+v", got)
	}
	if !strings.Contains(buf.String(), `"community": true`) {
		t.Errorf("JSON output missing community key\n%s", buf.String())
	}
}

// TestSpecCell pins the shared hardware-spec cell renderer: a positive count
// prints as-is; 0 or negative (an unreported spec) prints "-" rather than a
// misleading "0 vCPU".
func TestSpecCell(t *testing.T) {
	cases := map[int]string{0: "-", -1: "-", 26: "26", 512: "512"}
	for in, want := range cases {
		if got := specCell(in); got != want {
			t.Errorf("specCell(%d) = %q, want %q", in, got, want)
		}
	}
}

// TestGPUTypes_Table_SpecColumns asserts `gpu gpu-types` gained the VCPU,
// RAM (GB) and STORAGE (GB) columns and renders the reported values.
func TestGPUTypes_Table_SpecColumns(t *testing.T) {
	rows := []apiclient.GPUType{
		{GPUType: "h100_sxm", VRAMGB: 80, CPUCores: 26, RAMGB: 200, StorageGB: 512},
		{GPUType: "l4", VRAMGB: 24}, // specs unreported
	}
	var buf bytes.Buffer
	if err := GPUTypes(FormatTable, &buf, rows); err != nil {
		t.Fatalf("GPUTypes: %v", err)
	}
	out := buf.String()
	for _, want := range []string{"VCPU", "RAM (GB)", "STORAGE (GB)", "26", "200", "512"} {
		if !strings.Contains(out, want) {
			t.Errorf("gpu-types table missing %q\n%s", want, out)
		}
	}
}

// TestPricing_Table_SpecColumns asserts `gpu pricing` renders the spec columns.
// The third column is DISK (GB) — the filesystem a launch actually RECEIVES
// (instance_disk_gb), not the old host-storage figure, which for container
// offerings advertised disk the customer never got. A trailing "+" marks
// capacity that accepts a custom --disk; a pre-field server (instance_disk_gb
// absent → 0) falls back to the storage figure so the column never goes blank
// during a rollout window.
func TestPricing_Table_SpecColumns(t *testing.T) {
	rows := []apiclient.PricingRow{
		// Configurable container row: DISK shows the launch disk (100), plus-marked.
		{GPUType: "h100_sxm", GPUCount: 1, Region: "us-east", Tier: "on_demand", PricePerHour: 2.5, Available: 4,
			CPUCores: 26, RAMGB: 200, StorageGB: 512, InstanceDiskGB: 100, DiskConfigurable: true},
		// Fixed VM row: DISK shows the machine's own disk, no plus.
		{GPUType: "l40", GPUCount: 1, Region: "us-east", Tier: "on_demand", PricePerHour: 0.9, Available: 2,
			CPUCores: 16, RAMGB: 128, StorageGB: 625, InstanceDiskGB: 625, DiskConfigurable: false},
		// Pre-field server (no instance_disk_gb): falls back to the storage figure.
		{GPUType: "a40", GPUCount: 1, Region: "eu-west", Tier: "on_demand", PricePerHour: 0.5, Available: 1,
			CPUCores: 8, RAMGB: 64, StorageGB: 300},
	}
	var buf bytes.Buffer
	if err := Pricing(FormatTable, &buf, rows); err != nil {
		t.Fatalf("Pricing: %v", err)
	}
	out := buf.String()
	for _, want := range []string{"VCPU", "RAM (GB)", "DISK (GB)", "26", "200", "100+", "625", "300"} {
		if !strings.Contains(out, want) {
			t.Errorf("pricing table missing %q\n%s", want, out)
		}
	}
	if strings.Contains(out, "STORAGE (GB)") {
		t.Errorf("pricing table still says STORAGE (GB) — that label advertised host disk a container launch never received\n%s", out)
	}
	if strings.Contains(out, "625+") {
		t.Errorf("fixed-disk row must not carry the configurable marker\n%s", out)
	}
}

// TestCatalog_JSON_SpecRoundTrip asserts cpu_cores/ram_gb/storage_gb appear on
// the JSON wire for both catalog shapes and survive an unmarshal round-trip.
func TestCatalog_JSON_SpecRoundTrip(t *testing.T) {
	wantKeys := []string{`"cpu_cores": 26`, `"ram_gb": 200`, `"storage_gb": 512`}

	var gbuf bytes.Buffer
	gt := []apiclient.GPUType{{GPUType: "h100_sxm", VRAMGB: 80, CPUCores: 26, RAMGB: 200, StorageGB: 512}}
	if err := GPUTypes(FormatJSON, &gbuf, gt); err != nil {
		t.Fatalf("GPUTypes JSON: %v", err)
	}
	for _, want := range wantKeys {
		if !strings.Contains(gbuf.String(), want) {
			t.Errorf("gpu-types JSON missing %q\n%s", want, gbuf.String())
		}
	}
	var gotGT []apiclient.GPUType
	if err := json.Unmarshal(gbuf.Bytes(), &gotGT); err != nil {
		t.Fatalf("gpu-types invalid JSON: %v", err)
	}
	if len(gotGT) != 1 || gotGT[0].CPUCores != 26 || gotGT[0].RAMGB != 200 || gotGT[0].StorageGB != 512 {
		t.Errorf("gpu-types specs did not round-trip: %+v", gotGT)
	}

	var pbuf bytes.Buffer
	pr := []apiclient.PricingRow{{GPUType: "h100_sxm", GPUCount: 1, Region: "us-east", Tier: "on_demand", PricePerHour: 2.5, Available: 4, CPUCores: 26, RAMGB: 200, StorageGB: 512}}
	if err := Pricing(FormatJSON, &pbuf, pr); err != nil {
		t.Fatalf("Pricing JSON: %v", err)
	}
	for _, want := range wantKeys {
		if !strings.Contains(pbuf.String(), want) {
			t.Errorf("pricing JSON missing %q\n%s", want, pbuf.String())
		}
	}
	var gotPR []apiclient.PricingRow
	if err := json.Unmarshal(pbuf.Bytes(), &gotPR); err != nil {
		t.Fatalf("pricing invalid JSON: %v", err)
	}
	if len(gotPR) != 1 || gotPR[0].CPUCores != 26 || gotPR[0].RAMGB != 200 || gotPR[0].StorageGB != 512 {
		t.Errorf("pricing specs did not round-trip: %+v", gotPR)
	}
}
