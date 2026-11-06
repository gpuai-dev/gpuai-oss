package cliprint

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/gpuai-dev/gpuai-oss/internal/apiclient"
)

func sampleInstance() apiclient.Instance {
	name := "my-trainer"
	return apiclient.Instance{
		ID: "gpu-abc", Name: &name, Status: "running",
		GPUType: "h100_sxm", GPUCount: 1, Tier: "on_demand",
		Region: "us-east", PricePerHour: 2.50,
		CreatedAt: time.Date(2026, 5, 14, 0, 0, 0, 0, time.UTC),
	}
}

func TestInstances_Json(t *testing.T) {
	buf := &bytes.Buffer{}
	err := Instances(FormatJSON, buf, []apiclient.Instance{sampleInstance()})
	if err != nil {
		t.Fatalf("Instances: %v", err)
	}
	var got []apiclient.Instance
	if err := json.Unmarshal(buf.Bytes(), &got); err != nil {
		t.Fatalf("unmarshal: %v\nbuf: %s", err, buf.String())
	}
	if len(got) != 1 || got[0].ID != "gpu-abc" {
		t.Errorf("roundtrip: %+v", got)
	}
}

func TestInstances_Table(t *testing.T) {
	buf := &bytes.Buffer{}
	err := Instances(FormatTable, buf, []apiclient.Instance{sampleInstance()})
	if err != nil {
		t.Fatalf("Instances: %v", err)
	}
	out := buf.String()
	for _, want := range []string{"ID", "NAME", "STATUS", "gpu-abc", "my-trainer", "running", "h100_sxm"} {
		if !strings.Contains(out, want) {
			t.Errorf("table missing %q in output: %s", want, out)
		}
	}
}

func TestInstances_EmptyTable(t *testing.T) {
	buf := &bytes.Buffer{}
	err := Instances(FormatTable, buf, nil)
	if err != nil {
		t.Fatalf("Instances(empty): %v", err)
	}
	if !strings.Contains(buf.String(), "ID") {
		t.Errorf("expected header in empty-table output: %s", buf.String())
	}
}

func TestInstances_UnknownFormat(t *testing.T) {
	buf := &bytes.Buffer{}
	err := Instances("xml", buf, nil)
	if err == nil || !strings.Contains(err.Error(), "unknown output format") {
		t.Errorf("expected unknown-format error, got: %v", err)
	}
}

func TestInstanceDetail_TableShowsSSHCommand(t *testing.T) {
	inst := sampleInstance()
	inst.Connection = &apiclient.ConnectionInfo{
		Hostname: "ssh.example.com", Port: 10042,
		SSHCommand: "ssh -p 10042 root@ssh.example.com",
	}
	buf := &bytes.Buffer{}
	if err := InstanceDetail(FormatTable, buf, inst); err != nil {
		t.Fatalf("InstanceDetail: %v", err)
	}
	out := buf.String()
	if !strings.Contains(out, "ssh.example.com") || !strings.Contains(out, "10042") {
		t.Errorf("connection info missing: %s", out)
	}
	if !strings.Contains(out, "ssh -p 10042 root@ssh.example.com") {
		t.Errorf("SSH command missing: %s", out)
	}
}

func TestSSHKeys_Table(t *testing.T) {
	keys := []apiclient.SSHKey{{
		ID: "key-abc", Name: "laptop", Fingerprint: "SHA256:xyz",
		CreatedAt: time.Date(2026, 5, 14, 0, 0, 0, 0, time.UTC),
	}}
	buf := &bytes.Buffer{}
	if err := SSHKeys(FormatTable, buf, keys); err != nil {
		t.Fatalf("%v", err)
	}
	out := buf.String()
	for _, want := range []string{"ID", "NAME", "FINGERPRINT", "key-abc", "laptop", "SHA256:xyz"} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q: %s", want, out)
		}
	}
}

func TestGPUTypes_Table(t *testing.T) {
	types := []apiclient.GPUType{
		{GPUType: "h100_sxm", VRAMGB: 80},
	}
	buf := &bytes.Buffer{}
	if err := GPUTypes(FormatTable, buf, types); err != nil {
		t.Fatalf("%v", err)
	}
	if !strings.Contains(buf.String(), "h100_sxm") || !strings.Contains(buf.String(), "80") {
		t.Errorf("missing fields: %s", buf.String())
	}
}

func TestPricing_Table(t *testing.T) {
	rows := []apiclient.PricingRow{
		{GPUType: "h100_sxm", GPUCount: 1, Region: "us-east", Tier: "on_demand", PricePerHour: 2.5, Available: 4, InstantBoot: true},
		{GPUType: "h100_sxm", GPUCount: 8, Region: "us-west", Tier: "spot", PricePerHour: 1.2, Available: 0, InstantBoot: false},
	}
	buf := &bytes.Buffer{}
	if err := Pricing(FormatTable, buf, rows); err != nil {
		t.Fatalf("%v", err)
	}
	out := buf.String()
	if !strings.Contains(out, "$2.50") || !strings.Contains(out, "$1.20") {
		t.Errorf("prices missing: %s", out)
	}
	// BOOT column: "instant" for fast capacity, the boot estimate for slow
	// capacity. SOURCE renders the capacity class; both sample rows omit
	// Community/CapacityClass so they fall back to "secure". (ENV/TYPE now
	// trail SOURCE, so SOURCE is no longer the final column.)
	if !strings.Contains(out, "instant") || !strings.Contains(out, "~3-5 min") {
		t.Errorf("boot column missing: %s", out)
	}
	if !strings.Contains(out, "SOURCE") || !strings.Contains(out, "secure") {
		t.Errorf("source column missing: %s", out)
	}
	// ENV/TYPE trailing columns: sample rows are non-certified with an empty
	// DeploymentType, so they render as provider-template / "-".
	if !strings.Contains(out, "ENV") || !strings.Contains(out, "TYPE") {
		t.Errorf("env/type columns missing: %s", out)
	}
	// Availability counts render between COST/HR and BOOT.
	for _, line := range strings.Split(out, "\n") {
		if strings.Contains(line, "on_demand") && !strings.Contains(line, "4") {
			t.Errorf("on_demand row missing count 4: %s", line)
		}
		if strings.Contains(line, "spot") && !strings.Contains(line, "0") {
			t.Errorf("spot row missing count 0: %s", line)
		}
	}
}
