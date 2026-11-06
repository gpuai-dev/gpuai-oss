package cliprint

import (
	"bytes"
	"strings"
	"testing"

	"github.com/gpuai-dev/gpuai-oss/internal/apiclient"
)

func sampleEnvironments() apiclient.Environments {
	return apiclient.Environments{
		Frameworks: []string{"pytorch", "jupyter"},
		Versions: map[string][]apiclient.EnvFrameworkVersion{
			"pytorch": {
				{ID: "2.11", Label: "PyTorch 2.11", Variant: "cuda128", Default: true},
				{ID: "2.13", Label: "PyTorch 2.13", Variant: "cuda124"},
			},
		},
		Includes: map[string]map[string][]apiclient.EnvIncludeItem{
			"pytorch": {"cuda128": {{Name: "PyTorch", Version: "2.11.0"}, {Name: "CUDA Runtime", Version: "12.8.1"}}},
			"jupyter": {"cuda128": {{Name: "JupyterLab", Version: "4.6.1"}}},
		},
		VMImages: map[string]apiclient.EnvVMImage{
			"ubuntu-24.04":      {Label: "Ubuntu 24.04 LTS", Family: "ubuntu", Version: "24.04"},
			"ubuntu-24.04-cuda": {Label: "Ubuntu 24.04 LTS (CUDA)", Family: "ubuntu", Version: "24.04", DriverPreinstalled: true},
		},
	}
}

func TestEnvironments_Table(t *testing.T) {
	var b bytes.Buffer
	if err := Environments(FormatTable, &b, sampleEnvironments()); err != nil {
		t.Fatalf("Environments: %v", err)
	}
	out := b.String()
	for _, want := range []string{
		"certified:pytorch@2.11", "CUDA 12.8", "(default)",
		"certified:pytorch@2.13", "CUDA 12.4",
		"certified:jupyter", "JupyterLab 4.6.1",
		"raw_vm:<os-id>", "ubuntu-24.04-cuda", "preinstalled", "install at boot",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("table output missing %q\n---\n%s", want, out)
		}
	}
}

func TestEnvironments_JSON(t *testing.T) {
	var b bytes.Buffer
	if err := Environments(FormatJSON, &b, sampleEnvironments()); err != nil {
		t.Fatalf("Environments json: %v", err)
	}
	if !strings.Contains(b.String(), "\"frameworks\"") || !strings.Contains(b.String(), "\"vm_images\"") {
		t.Errorf("json output missing expected keys:\n%s", b.String())
	}
}
