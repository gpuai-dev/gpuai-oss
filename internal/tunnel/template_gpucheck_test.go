package tunnel

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// The startup GPU compute check (section 8b). A host whose nvidia-uvm path is
// broken answers nvidia-smi and fails every CUDA call; a framework built
// without kernels for the GPU's architecture does the same one layer up. Both
// were billed as ready before this check existed.

func gpuCheckBootstrap(mode string) BootstrapData {
	d := baseBootstrap()
	d.GPUCheckMode = mode
	d.ExpectedGPUCount = 2
	d.ExpectFramework = "pytorch"
	return d
}

func renderOK(t *testing.T, d BootstrapData) string {
	t.Helper()
	script, err := RenderBootstrap(d)
	if err != nil {
		t.Fatalf("RenderBootstrap: %v", err)
	}
	return script
}

func TestRenderBootstrap_GPUCheckOffRendersNothing(t *testing.T) {
	script := renderOK(t, baseBootstrap())
	for _, s := range []string{"gpuai-gpucheck", "gpu_check", "exit 94"} {
		if strings.Contains(script, s) {
			t.Errorf("check disabled but bootstrap contains %q", s)
		}
	}
}

func TestRenderBootstrap_GPUCheckModes(t *testing.T) {
	const readyCB = `echo "[GPU.ai] Sending ready callback..."`
	shadow := renderOK(t, gpuCheckBootstrap("shadow"))
	if !strings.Contains(shadow, "workload_python $_gpu_secs 2 'pytorch'") {
		t.Error("shadow bootstrap does not run the check with the expected count and framework")
	}
	if strings.Contains(shadow, "exit 94") {
		t.Error("shadow mode must never block the ready callback")
	}
	// Shadow adds no latency to readiness: one try, within its own budget, in
	// the background, only after the ready callback, reported report-only.
	run := fmt.Sprintf("gpu_check %d 1", GPUCheckShadowSeconds)
	at := strings.Index(shadow, run)
	if at < 0 || at < strings.Index(shadow, readyCB) || !strings.Contains(shadow[at:], ") &") {
		t.Errorf("shadow check must run %q in the background after the ready callback", run)
	}
	if !strings.Contains(shadow, `\"gpu_check_mode\": \"shadow\"`) || !strings.Contains(shadow, `\"report_only\": \"true\"`) {
		t.Error("shadow result is not delivered report-only")
	}
	if cb := shadow[strings.Index(shadow, readyCB):at]; strings.Contains(cb, "gpu_check") {
		t.Error("the shadow ready callback must not wait for or carry the check")
	}

	enforce := renderOK(t, gpuCheckBootstrap("enforce"))
	if !strings.Contains(enforce, "exit 94") {
		t.Error("enforce bootstrap has no refusal path")
	}
	// The check and the refusal must come before the ready callback is sent.
	gate := fmt.Sprintf("gpu_check %d 3", GPUCheckEnforceSeconds)
	if i := strings.Index(enforce, gate); i < 0 || i > strings.Index(enforce, readyCB) {
		t.Errorf("enforce must run %q before the ready callback", gate)
	}
	if strings.Index(enforce, "exit 94") > strings.Index(enforce, readyCB) {
		t.Error("enforce refusal is rendered after the ready callback")
	}
	if strings.Contains(enforce, "report_only") {
		t.Error("enforce reports on the ready callback itself")
	}
}

func TestRenderBootstrap_GPUCheckRunsWhereTheWorkloadRuns(t *testing.T) {
	certified := gpuCheckBootstrap("enforce")
	certified.CertifiedImage = true
	certified.DeploymentType = "vm"
	certified.PinnedImageRef = "registry.example.com/certified@sha256:6422f5ac0000000000000000000000000000000000000000000000000000dead"
	certified.DriverFloor = "550"
	if s := renderOK(t, certified); !strings.Contains(s, "_wp_c=gpuai-certified\n") || !strings.Contains(s, "docker exec -i $_wp_c") {
		t.Error("certified VM: check must run inside the certified container, not on the host")
	}
	customer := gpuCheckBootstrap("enforce")
	customer.DeploymentType = "vm"
	customer.CustomerImageRef = "ghcr.io/acme/trainer:v1@sha256:" + strings.Repeat("a", 64)
	customer.ExpectFramework = ""
	if s := renderOK(t, customer); !strings.Contains(s, "_wp_c=gpuai-workload\n") || !strings.Contains(s, "docker exec -i $_wp_c") {
		t.Error("customer image on a VM: check must run inside the workload container")
	}
	native := renderOK(t, gpuCheckBootstrap("enforce"))
	if strings.Contains(native, "docker exec -i") {
		t.Error("container-native: check must run in place")
	}
}

func TestValidateBootstrapData_GPUCheckFieldsAreClosedSets(t *testing.T) {
	for name, mutate := range map[string]func(*BootstrapData){
		"mode":      func(d *BootstrapData) { d.GPUCheckMode = "always; rm -rf /" },
		"framework": func(d *BootstrapData) { d.ExpectFramework = "jax'" },
		"count":     func(d *BootstrapData) { d.ExpectedGPUCount = -1 },
	} {
		d := gpuCheckBootstrap("enforce")
		mutate(&d)
		if _, err := RenderBootstrap(d); err == nil {
			t.Errorf("%s: invalid value rendered", name)
		}
	}
}

// The embedded check script must at least compile; on a machine without a GPU
// it then reports the missing driver library as a host failure, never ok.
func TestGPUCheckScript_CompilesAndFailsClosedWithoutADriver(t *testing.T) {
	py, err := exec.LookPath("python3")
	if err != nil {
		t.Skip("python3 not available")
	}
	script := renderOK(t, gpuCheckBootstrap("enforce"))
	start := strings.Index(script, "<<'GPUCHECKEOF'\n")
	end := strings.Index(script, "\nGPUCHECKEOF\n")
	if start < 0 || end < start {
		t.Fatal("check script heredoc not found")
	}
	path := filepath.Join(t.TempDir(), "gpucheck.py")
	if err := os.WriteFile(path, []byte(script[start+len("<<'GPUCHECKEOF'\n"):end]), 0o600); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command(py, "-m", "py_compile", path).CombinedOutput(); err != nil {
		t.Fatalf("check script does not compile: %v\n%s", err, out)
	}
	if _, err := os.Stat("/dev/nvidiactl"); err == nil {
		t.Skip("a GPU driver is present; the no-driver path is not reachable here")
	}
	out, _ := exec.Command(py, path, "1", "").Output()
	if !strings.HasPrefix(strings.TrimSpace(string(out)), "GPUCHECK|fail|host: ") {
		t.Errorf("without a driver the check printed %q; want a host failure", out)
	}
}

// gpuCheckRun is what one execution of the rendered section 8b did.
type gpuCheckRun struct {
	result    string   // GPU_CHECK|DETAIL|SEEN in the main shell afterwards
	callbacks []string // every callback body, in order
	events    []string // "python" per check try and "callback" per callback, in order
	exitCode  int
	tries     int
	timeouts  []int    // the timeout each timeout call was given, in seconds
	docker    []string // every docker invocation's arguments
}

// runGPUCheckSection executes the rendered section 8b, the ready callback and
// the shadow block after it in bash, with fake python3 and timeout on PATH, so
// the retry, skip, refusal and ordering logic is tested without a GPU. The fake
// python3 prints fakeOut and exits with fakeRC.
func runGPUCheckSection(t *testing.T, mode, fakeOut string, fakeRC int) gpuCheckRun {
	t.Helper()
	return runGPUCheckSectionFor(t, gpuCheckBootstrap(mode), fakeOut, fakeRC)
}

func runGPUCheckSectionFor(t *testing.T, data BootstrapData, fakeOut string, fakeRC int) gpuCheckRun {
	t.Helper()
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash not available")
	}
	script := renderOK(t, data)
	start := strings.Index(script, "# workload_python SECONDS")
	end := strings.Index(script, "# RAWREADY late re-POST")
	if start < 0 || end < start {
		t.Fatal("section 8b and the ready callback after it not found")
	}
	section := script[strings.LastIndex(script[:start], "\n")+1 : end]

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
	write("python3", "#!/bin/bash\ncat >/dev/null\necho python >> \"$EVENTS\"\nprintf '%s\\n' \"$FAKE_OUT\"\nexit \"$FAKE_RC\"\n")
	write("timeout", "#!/bin/bash\necho \"$1\" >> \"$TIMEOUTS\"\nshift\nexec \"$@\"\n")
	// docker exec (certified-VM and customer-image placements): the check
	// itself behaves like python3 above; any other exec (the kill) is logged.
	write("docker", "#!/bin/bash\necho \"$*\" >> \"$DOCKER_LOG\"\ncase \"$2\" in -i) cat >/dev/null; echo python >> \"$EVENTS\"; printf '%s\\n' \"$FAKE_OUT\"; exit \"$FAKE_RC\";; esac\n")
	files := map[string]string{}
	for _, f := range []string{"CB_LOG", "EVENTS", "TIMEOUTS", "DOCKER_LOG"} {
		files[f] = filepath.Join(dir, strings.ToLower(f))
	}
	harness := "set -uo pipefail\n" +
		"GPU_INFO='NVIDIA H100'; DRIVER_VERSION=580; CUDA_VERSION=13.0; DOCKER_UP=false\n" +
		"TUNNEL_UP=true; APP_READY=true; CERTIFIED_IMAGE_DIGEST=; CONTAINER_ID=\n" +
		"send_callback() { printf '%s\\n' \"$1\" >> \"$CB_LOG\"; echo callback >> \"$EVENTS\"; }\n" +
		"sleep() { :; }\n" +
		section +
		"\nwait\necho \"RESULT=${GPU_CHECK}|${GPU_CHECK_DETAIL}|${GPU_CHECK_SEEN}\"\n"
	cmd := exec.Command("bash", "-c", harness)
	cmd.Env = append(os.Environ(), "PATH="+bin+":"+os.Getenv("PATH"),
		"FAKE_OUT="+fakeOut, "FAKE_RC="+strconv.Itoa(fakeRC))
	for k, v := range files {
		cmd.Env = append(cmd.Env, k+"="+v)
	}
	out, err := cmd.CombinedOutput()
	var run gpuCheckRun
	if ee, ok := err.(*exec.ExitError); ok {
		run.exitCode = ee.ExitCode()
	} else if err != nil {
		t.Fatalf("harness: %v\n%s", err, out)
	}
	for _, line := range strings.Split(string(out), "\n") {
		if strings.HasPrefix(line, "RESULT=") {
			run.result = strings.TrimPrefix(line, "RESULT=")
		}
	}
	readLines := func(name string) []string {
		b, err := os.ReadFile(files[name])
		if err != nil || len(strings.TrimSpace(string(b))) == 0 {
			return nil
		}
		return strings.Split(strings.TrimSpace(string(b)), "\n")
	}
	run.callbacks = readLines("CB_LOG")
	run.docker = readLines("DOCKER_LOG")
	run.events = readLines("EVENTS")
	for _, e := range run.events {
		if e == "python" {
			run.tries++
		}
	}
	for _, s := range readLines("TIMEOUTS") {
		if n, err := strconv.Atoi(s); err == nil {
			run.timeouts = append(run.timeouts, n)
		}
	}
	return run
}

func TestGPUCheckSection_PassPublishes(t *testing.T) {
	r := runGPUCheckSection(t, "enforce", "GPUCHECK|ok||2", 0)
	if r.exitCode != 0 || r.result != "ok||2" || r.tries != 1 || len(r.callbacks) != 1 ||
		!strings.Contains(r.callbacks[0], `"tunnel_up": "true"`) || !strings.Contains(r.callbacks[0], `"gpu_check": "ok"`) {
		t.Errorf("pass: %+v", r)
	}
	if len(r.timeouts) != 1 || r.timeouts[0] > GPUCheckEnforceSeconds || r.timeouts[0] < GPUCheckEnforceSeconds-2 {
		t.Errorf("enforce check given timeouts %v; want one of about %ds", r.timeouts, GPUCheckEnforceSeconds)
	}
}

func TestGPUCheckSection_HostFailureRetriesThenRefuses(t *testing.T) {
	r := runGPUCheckSection(t, "enforce", "GPUCHECK|fail|host: cuInit returned 999, nvidia-uvm open: Input/output error|1", 0)
	if r.exitCode != 94 {
		t.Fatalf("exit = %d; want 94 (refuse to publish)", r.exitCode)
	}
	if r.tries != 3 {
		t.Errorf("host failure tried %d times; want 3", r.tries)
	}
	// The tries share one budget: none may be given more than what is left.
	for _, s := range r.timeouts {
		if s > GPUCheckEnforceSeconds {
			t.Errorf("a try was given %ds, over the %ds budget", s, GPUCheckEnforceSeconds)
		}
	}
	if len(r.callbacks) != 1 || !strings.Contains(r.callbacks[0], `"tunnel_up": "false"`) ||
		!strings.Contains(r.callbacks[0], `"gpu_check": "fail"`) || !strings.Contains(r.callbacks[0], "cuInit returned 999") {
		t.Errorf("refusal callback = %v", r.callbacks)
	}
}

func TestGPUCheckSection_EnvironmentFailureDoesNotRetry(t *testing.T) {
	r := runGPUCheckSection(t, "enforce", "GPUCHECK|fail|environment: torch 2.4.1 cuda 12.4 on sm_120: no kernel image is available|1", 0)
	if r.exitCode != 94 || r.tries != 1 || len(r.callbacks) != 1 {
		t.Errorf("environment failure: %+v; want exit 94, 1 try, 1 callback", r)
	}
}

// A framework the environment did not promise is the image's business: a
// customer image with CPU-only PyTorch used to be refused on every host in
// enforce mode. It is reported as a warning and the instance publishes. A host
// failure in the same environment still refuses.
func TestGPUCheckSection_UnpromisedFrameworkFailureNeverBlocks(t *testing.T) {
	noPromise := gpuCheckBootstrap("enforce")
	noPromise.ExpectFramework = ""
	r := runGPUCheckSectionFor(t, noPromise, "GPUCHECK|fail|environment: torch 2.4.1+cpu cannot use the GPU|1", 0)
	if r.exitCode != 0 || r.tries != 1 || r.result != "warn|environment: torch 2.4.1+cpu cannot use the GPU|1" ||
		len(r.callbacks) != 1 || !strings.Contains(r.callbacks[0], `"gpu_check": "warn"`) {
		t.Errorf("unpromised framework: %+v; want warn on the ready callback, no refusal", r)
	}
	r = runGPUCheckSectionFor(t, noPromise, "GPUCHECK|fail|host: cuInit returned 999|1", 0)
	if r.exitCode != 94 || len(r.callbacks) != 1 {
		t.Errorf("host failure without a promised framework: %+v; want a refusal", r)
	}
}

// Shadow adds no latency to readiness (the tunnel-stall watchdog clock runs
// until the ready callback lands): the callback goes first, untouched, then the
// check runs once within its short budget and its result follows report-only.
func TestGPUCheckSection_ShadowRunsAfterTheCallbackOnce(t *testing.T) {
	for name, tc := range map[string]struct {
		out    string
		rc     int
		detail string
	}{
		"host failure": {"GPUCHECK|fail|host: cuInit returned 999|1", 0, "host: cuInit returned 999"},
		"hung check":   {"", 124, fmt.Sprintf("host: compute check did not finish in %ds", GPUCheckShadowSeconds)},
	} {
		t.Run(name, func(t *testing.T) {
			r := runGPUCheckSection(t, "shadow", tc.out, tc.rc)
			if r.exitCode != 0 || r.tries != 1 {
				t.Fatalf("shadow: exit=%d tries=%d; want 0 and a single try", r.exitCode, r.tries)
			}
			if want := []string{"callback", "python", "callback"}; strings.Join(r.events, ",") != strings.Join(want, ",") {
				t.Fatalf("events = %v; want %v (ready callback before the check)", r.events, want)
			}
			if strings.Contains(r.callbacks[0], "gpu_check") || !strings.Contains(r.callbacks[0], `"tunnel_up": "true"`) {
				t.Errorf("ready callback = %q; want it unchanged by the check", r.callbacks[0])
			}
			late := r.callbacks[1]
			for _, want := range []string{`"report_only": "true"`, `"tunnel_up": "false"`, `"gpu_check": "fail"`, `"gpu_check_mode": "shadow"`} {
				if !strings.Contains(late, want) {
					t.Errorf("late report %q lacks %s", late, want)
				}
			}
			if !strings.Contains(late, tc.detail) {
				t.Errorf("late report %q lacks %q", late, tc.detail)
			}
			if len(r.timeouts) != 1 || r.timeouts[0] > GPUCheckShadowSeconds || r.timeouts[0] < GPUCheckShadowSeconds-2 {
				t.Errorf("shadow check given timeouts %v; want one of about %ds", r.timeouts, GPUCheckShadowSeconds)
			}
		})
	}
}

func TestGPUCheckSection_NoPythonSkips(t *testing.T) {
	r := runGPUCheckSection(t, "enforce", "", 127)
	if r.exitCode != 0 || len(r.callbacks) != 1 || !strings.HasPrefix(r.result, "skipped|") ||
		!strings.Contains(r.callbacks[0], `"gpu_check": "skipped"`) {
		t.Errorf("no python: %+v", r)
	}
}

// A hung check has already spent its budget; retrying would only push the
// ready callback toward the boot watchdogs. One try, then refuse.
func TestGPUCheckSection_HungCheckFailsWithoutRetry(t *testing.T) {
	r := runGPUCheckSection(t, "enforce", "", 124)
	if r.exitCode != 94 || r.tries != 1 || len(r.callbacks) != 1 || !strings.Contains(r.callbacks[0], "compute check did not finish") {
		t.Errorf("hung check: %+v", r)
	}
}

// Through docker exec a timeout only kills the docker client; the check would
// keep running inside the container, holding the GPUs. The bootstrap must kill
// it there.
func TestGPUCheckSection_TimeoutKillsTheCheckInsideTheContainer(t *testing.T) {
	d := gpuCheckBootstrap("enforce")
	d.CertifiedImage = true
	d.DeploymentType = "vm"
	d.PinnedImageRef = "registry.example.com/certified@sha256:6422f5ac0000000000000000000000000000000000000000000000000000dead"
	d.DriverFloor = "550"
	r := runGPUCheckSectionFor(t, d, "", 124)
	if r.exitCode != 94 || r.tries != 1 {
		t.Fatalf("hung check in the certified container: %+v", r)
	}
	if len(r.docker) != 2 || !strings.HasPrefix(r.docker[1], "exec gpuai-certified sh -c") || !strings.Contains(r.docker[1], "kill -9") {
		t.Errorf("docker calls = %q; want the check, then a kill of it inside gpuai-certified", r.docker)
	}
	if len(r.timeouts) != 2 || r.timeouts[1] != 10 {
		t.Errorf("timeouts = %v; want the check's budget, then a 10s bound on the kill", r.timeouts)
	}
	// A check that answered leaves nothing to kill.
	if r := runGPUCheckSectionFor(t, d, "GPUCHECK|ok||2", 0); len(r.docker) != 1 || r.exitCode != 0 {
		t.Errorf("passing check: docker calls = %q exit=%d; want the check alone", r.docker, r.exitCode)
	}
}
