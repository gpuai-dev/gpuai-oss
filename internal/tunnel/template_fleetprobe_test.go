package tunnel

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// Section 8c: fleet-probe launches download the on-host probe, run it and post
// the report. The probe is fetched rather than embedded: at about 20 KB it
// would push a size-capped provider startup script past its cap.

const testProbeURL = "https://api.example.com/internal/instances/gpu-abcd1234/probe"

func probeBootstrap() BootstrapData {
	d := gpuCheckBootstrap("enforce")
	d.ProbeURL = testProbeURL
	return d
}

func TestRenderBootstrap_FleetProbeOffRendersNothing(t *testing.T) {
	if script := renderOK(t, gpuCheckBootstrap("enforce")); strings.Contains(script, "gpuai-probe") {
		t.Error("customer launch renders the fleet probe section")
	}
}

func TestRenderBootstrap_FleetProbeDownloadsRunsAndPosts(t *testing.T) {
	script := renderOK(t, probeBootstrap())
	get := strings.Index(script, "curl -fsS '"+testProbeURL+"' -H 'Authorization: Bearer internal-token'")
	post := strings.Index(script, "curl -fsS -X POST '"+testProbeURL+"'")
	ready := strings.Index(script, `echo "[GPU.ai] Sending ready callback..."`)
	if get < 0 || post < 0 {
		t.Fatalf("probe section is missing the download (%d) or the report post (%d)", get, post)
	}
	if get >= post || post >= ready {
		t.Errorf("order: download %d, post %d, ready callback %d; want download, post, then ready", get, post, ready)
	}
}

func TestValidateBootstrapData_ProbeURL(t *testing.T) {
	for _, bad := range []string{"http://api.example.com/internal/instances/x/probe", testProbeURL + "';reboot;'", testProbeURL + "$(id)"} {
		d := probeBootstrap()
		d.ProbeURL = bad
		if _, err := RenderBootstrap(d); err == nil {
			t.Errorf("ProbeURL %q rendered", bad)
		}
	}
}

// runProbeSection executes the rendered section 8c in bash with a fake curl and
// python3 on PATH and returns what was posted as the report.
func runProbeSection(t *testing.T, download, probeOut string) string {
	t.Helper()
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash not available")
	}
	script := renderOK(t, probeBootstrap())
	helper := strings.Index(script, "# workload_python SECONDS")
	helperEnd := strings.Index(script, "# 8b. Startup GPU compute check")
	start := strings.Index(script, "# 8c. Fleet probe")
	end := strings.Index(script, `echo "[GPU.ai] Sending ready callback..."`)
	if helper < 0 || helperEnd < helper || start < helperEnd || end < start {
		t.Fatal("section 8c or the workload_python helper not found")
	}
	// The helper and section 8c, without the GPU check in between.
	section := script[helper:helperEnd] + script[strings.LastIndex(script[:start], "\n")+1:end]

	dir := t.TempDir()
	bin := filepath.Join(dir, "bin")
	if err := os.Mkdir(bin, 0o755); err != nil {
		t.Fatal(err)
	}
	write := func(name, body string) {
		if err := os.WriteFile(filepath.Join(bin, name), []byte(body), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	// GET: -o <file> gets the probe (or exit 22, curl's HTTP-error code).
	// POST: --data-binary @<file> is copied to $POSTED.
	write("curl", `#!/bin/bash
out=""; data=""; post=false
while [ $# -gt 0 ]; do
  case "$1" in
    -o) out="$2"; shift ;;
    --data-binary) data="${2#@}"; shift ;;
    -X) [ "$2" = POST ] && post=true; shift ;;
  esac
  shift
done
if $post; then cp "$data" "$POSTED"; exit 0; fi
[ "$FAKE_DOWNLOAD" = ok ] || exit 22
printf 'print(1)\n' > "$out"
`)
	write("python3", "#!/bin/bash\ncat >/dev/null\nprintf '%s' \"$FAKE_PROBE_OUT\"\n")
	write("timeout", "#!/bin/bash\nshift\nexec \"$@\"\n")
	posted := filepath.Join(dir, "posted")
	cmd := exec.Command("bash", "-c", "set -uo pipefail\nrm -f /tmp/gpuai-probe.py /tmp/gpuai-probe.json\n"+section)
	cmd.Env = append(os.Environ(), "PATH="+bin+":"+os.Getenv("PATH"), "POSTED="+posted,
		"FAKE_DOWNLOAD="+download, "FAKE_PROBE_OUT="+probeOut)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("section 8c exited: %v\n%s", err, out)
	}
	b, err := os.ReadFile(posted)
	if err != nil {
		t.Fatalf("no report was posted: %v", err)
	}
	return string(b)
}

func TestFleetProbeSection_PostsTheProbeReport(t *testing.T) {
	if got := runProbeSection(t, "ok", `{"probe_version": 3, "cuda": {"cuinit": 0}}`); !strings.Contains(got, `"cuinit": 0`) {
		t.Errorf("posted %q; want the probe's report", got)
	}
}

func TestFleetProbeSection_DownloadFailureIsReported(t *testing.T) {
	if got := runProbeSection(t, "fail", ""); !strings.Contains(got, "could not be downloaded") {
		t.Errorf("posted %q; want an explicit download failure", got)
	}
}

func TestFleetProbeSection_SilentProbeIsReported(t *testing.T) {
	if got := runProbeSection(t, "ok", ""); !strings.Contains(got, "without a report") {
		t.Errorf("posted %q; want the probe's missing report", got)
	}
}
