package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// dockerClient is a minimal Docker Engine API client over the unix socket.
// Unversioned paths are used so the daemon answers with its own API version.
type dockerClient struct {
	http *http.Client
}

func newDockerClient(socket string) *dockerClient {
	return &dockerClient{http: &http.Client{
		Timeout: 5 * time.Minute, // image pulls stream for a while
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				var d net.Dialer
				return d.DialContext(ctx, "unix", socket)
			},
		},
	}}
}

type dockerError struct {
	Status  int
	Message string
}

func (e *dockerError) Error() string { return fmt.Sprintf("docker API %d: %s", e.Status, e.Message) }

func (c *dockerClient) do(ctx context.Context, method, path string, body, out any) error {
	var rdr io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return err
		}
		rdr = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, "http://docker"+path, rdr)
	if err != nil {
		return err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		var m struct {
			Message string `json:"message"`
		}
		raw, _ := io.ReadAll(resp.Body)
		_ = json.Unmarshal(raw, &m)
		if m.Message == "" {
			m.Message = strings.TrimSpace(string(raw))
		}
		return &dockerError{Status: resp.StatusCode, Message: m.Message}
	}
	if out == nil {
		_, _ = io.Copy(io.Discard, resp.Body)
		return nil
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

// pull pulls ref, consuming the progress stream and surfacing a stream error.
func (c *dockerClient) pull(ctx context.Context, ref string) error {
	image, tag := ref, "latest"
	if i := strings.LastIndex(ref, ":"); i > strings.LastIndex(ref, "/") {
		image, tag = ref[:i], ref[i+1:]
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		"http://docker/images/create?fromImage="+url.QueryEscape(image)+"&tag="+url.QueryEscape(tag), nil)
	if err != nil {
		return err
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		raw, _ := io.ReadAll(resp.Body)
		return &dockerError{Status: resp.StatusCode, Message: strings.TrimSpace(string(raw))}
	}
	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 64*1024), 1<<20)
	for sc.Scan() {
		var ev struct {
			Error string `json:"error"`
		}
		if json.Unmarshal(sc.Bytes(), &ev) == nil && ev.Error != "" {
			return errors.New(ev.Error)
		}
	}
	return sc.Err()
}

type containerInspect struct {
	ID    string `json:"Id"`
	State struct {
		Running  bool   `json:"Running"`
		Status   string `json:"Status"`
		ExitCode int    `json:"ExitCode"`
	} `json:"State"`
	NetworkSettings struct {
		Ports    map[string][]struct{ HostIP, HostPort string } `json:"Ports"`
		Networks map[string]struct {
			IPAddress string `json:"IPAddress"`
		} `json:"Networks"`
	} `json:"NetworkSettings"`
}

// dockerRuntime runs each instance as a container from a small sshd image.
// Container port 22 is published on a random host port; customers reach it at
// publicIP:<port>. The customer's SSH keys and the optional startup script are
// passed in through the environment (see instance/entrypoint.sh).
type dockerRuntime struct {
	client      *dockerClient
	image       string
	network     string
	publicIP    string
	publishIP   string
	logger      *slog.Logger
	probeDialer *net.Dialer
}

const containerLabel = "io.gpuai-oss.mock-supplier"

func (d *dockerRuntime) Start(ctx context.Context, inst *instance, req provisionRequest) (string, error) {
	env := []string{
		"SSH_PUBLIC_KEYS_B64=" + base64.StdEncoding.EncodeToString([]byte(strings.Join(req.SSHPublicKeys, "\n"))),
		"GPUAI_INSTANCE_ID=" + req.InstanceID,
	}
	if req.StartupScript != "" {
		env = append(env, "STARTUP_SCRIPT_B64="+base64.StdEncoding.EncodeToString([]byte(req.StartupScript)))
	}
	spec := map[string]any{
		"Image":        d.image,
		"Env":          env,
		"ExposedPorts": map[string]any{"22/tcp": map[string]any{}},
		"Labels": map[string]string{
			containerLabel:                 "1",
			containerLabel + ".upstream":   inst.UpstreamID,
			containerLabel + ".instance":   req.InstanceID,
			containerLabel + ".gpu_type":   inst.GPUType,
			containerLabel + ".docker_ref": req.DockerImage, // recorded, not run: there are no GPUs here
		},
		"HostConfig": map[string]any{
			"PortBindings": map[string]any{
				"22/tcp": []map[string]string{{"HostIp": d.publishIP, "HostPort": ""}},
			},
			"Memory":   int64(256 << 20),
			"NanoCpus": int64(500_000_000),
		},
	}
	if d.network != "" {
		spec["HostConfig"].(map[string]any)["NetworkMode"] = d.network
	}
	name := "gpuai-oss-mock-" + inst.UpstreamID

	var created struct {
		ID string `json:"Id"`
	}
	err := d.client.do(ctx, http.MethodPost, "/containers/create?name="+url.QueryEscape(name), spec, &created)
	var derr *dockerError
	if errors.As(err, &derr) && derr.Status == http.StatusNotFound {
		d.logger.Info("pulling instance image", "image", d.image)
		if perr := d.client.pull(ctx, d.image); perr != nil {
			return "", fmt.Errorf("pull %s: %w", d.image, perr)
		}
		err = d.client.do(ctx, http.MethodPost, "/containers/create?name="+url.QueryEscape(name), spec, &created)
	}
	if err != nil {
		return "", fmt.Errorf("create container: %w", err)
	}
	if err := d.client.do(ctx, http.MethodPost, "/containers/"+created.ID+"/start", nil, nil); err != nil {
		_ = d.client.do(context.Background(), http.MethodDelete, "/containers/"+created.ID+"?force=true", nil, nil)
		return "", fmt.Errorf("start container: %w", err)
	}
	return created.ID, nil
}

// Probe reports ready once the container runs, its SSH port is published and,
// when the mock shares a network with it, sshd answers with its banner.
func (d *dockerRuntime) Probe(ctx context.Context, inst *instance) (string, int, bool, error) {
	var ci containerInspect
	if err := d.client.do(ctx, http.MethodGet, "/containers/"+inst.ContainerID+"/json", nil, &ci); err != nil {
		var derr *dockerError
		if errors.As(err, &derr) && derr.Status == http.StatusNotFound {
			return "", 0, false, errors.New("instance container disappeared")
		}
		return "", 0, false, nil // transient: retry on the next poll
	}
	if !ci.State.Running {
		if ci.State.Status == "exited" || ci.State.Status == "dead" {
			return "", 0, false, fmt.Errorf("instance container %s (exit %d)", ci.State.Status, ci.State.ExitCode)
		}
		return "", 0, false, nil
	}
	port := 0
	for _, b := range ci.NetworkSettings.Ports["22/tcp"] {
		if p, err := strconv.Atoi(b.HostPort); err == nil && p > 0 {
			port = p
			break
		}
	}
	if port == 0 {
		return "", 0, false, nil
	}
	if d.network != "" {
		if n, ok := ci.NetworkSettings.Networks[d.network]; ok && n.IPAddress != "" {
			if !d.sshBanner(ctx, net.JoinHostPort(n.IPAddress, "22")) {
				return "", 0, false, nil
			}
		}
	}
	return d.publicIP, port, true, nil
}

// sshBanner reports whether addr answers with an SSH protocol banner.
func (d *dockerRuntime) sshBanner(ctx context.Context, addr string) bool {
	conn, err := d.probeDialer.DialContext(ctx, "tcp", addr)
	if err != nil {
		return false
	}
	defer conn.Close()
	_ = conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	line, err := bufio.NewReader(conn).ReadString('\n')
	return err == nil && strings.HasPrefix(line, "SSH-")
}

func (d *dockerRuntime) Stop(ctx context.Context, inst *instance) error {
	if inst.ContainerID == "" {
		return nil
	}
	err := d.client.do(ctx, http.MethodDelete, "/containers/"+inst.ContainerID+"?force=true&v=true", nil, nil)
	var derr *dockerError
	if errors.As(err, &derr) && derr.Status == http.StatusNotFound {
		return nil // already gone
	}
	return err
}
