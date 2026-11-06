# CLI (`gpu`)

`gpu` is the command-line client for a GPU.ai Open Source server (`gpuctl`). It
lists GPU types and live offerings, launches and terminates instances, manages
SSH keys and registry credentials, and opens SSH sessions to running instances.

## Build

The CLI is a single Go binary with no runtime dependencies.

```sh
go build -o bin/gpu ./cmd/gpu
./bin/gpu version
```

Release builds can stamp version metadata with
`-ldflags "-X main.version=0.1.0 -X main.commit=$(git rev-parse --short HEAD) -X main.date=$(date -u +%F)"`.

## Configure

Every command except `login`, `logout`, `version` and `help` needs an API key.
Create one on the server, then store it with `gpu login`.

```sh
# on the server
gpuctl apikey create --name laptop

# on your machine; prompts for the key without echoing it
gpu login --api-base http://localhost:8080/v1

# or non-interactively
echo "$GPUAI_API_KEY" | gpu login --stdin --api-base https://gpu.example.com/v1
```

`gpu login` writes `~/.config/gpu/credentials.json` with mode 0600
(`$XDG_CONFIG_HOME/gpu/credentials.json` when `XDG_CONFIG_HOME` is set). It does
not contact the server. `gpu logout` deletes the file.

### Precedence

| Setting | Order, first match wins |
| --- | --- |
| API key | `GPUAI_API_KEY`, then `api_key` in credentials.json |
| API base | `--api-base`, then `GPUAI_API_BASE`, then `api_base` in credentials.json, then `http://localhost:8080/v1` |

The API base includes the `/v1` prefix. Commands that change state (create,
update, delete, key and credential changes) print the target host to stderr
before they run, so a command aimed at the wrong server is visible.

### Global flags

| Flag | Meaning |
| --- | --- |
| `--api-base <url>` | API base URL for this invocation. |
| `-o, --output table\|json` | Output format. Defaults to `table` on a terminal and `json` when piped. |
| `--debug` | Log HTTP requests to stderr. |

## Commands

### Catalog

```sh
gpu gpu-types                      # GPU models the configured suppliers offer
gpu pricing                        # live offerings with price and availability
gpu pricing --gpu-type h100_sxm --region us-east --instant-boot
gpu environments                   # values accepted by --environment
```

`gpu pricing` filters: `--gpu-type`, `--region`, `--tier`, `--instant-boot`,
`--community`, `--certified`, `--type vm|container` and `--include-unavailable`.
Prices are per instance per hour and cover every GPU in the instance. They are
passed through from the supplier.

### Instances

```sh
gpu instances create -t rtx_4090 --count 1 --ssh-key-id key_123
gpu instances list                 # non-terminated instances
gpu instances list --all           # include terminated ones
gpu instances get <id>
gpu instances update <id> --name trainer
gpu instances ssh <id>
gpu instances ssh <id> -- -L 8888:localhost:8888
gpu instances open <id>            # app URL, when the server runs the tunnel
gpu instances delete <id>          # alias: gpu instances terminate <id>
```

`create` submits the launch and polls the returned operation until it
succeeds or fails (five minutes by default, `--wait-timeout 0` to return the
operation id immediately). Useful flags:

| Flag | Meaning |
| --- | --- |
| `-t, --type` | GPU type id. Required. |
| `--count` | GPUs per instance (default 1). |
| `--region`, `--tier` | Placement filters. `--tier` defaults to `on_demand`. |
| `--ssh-key-id` | SSH key to install. Repeatable. |
| `--offering-id` | Pin the launch to one offering from `gpu pricing -o json`. |
| `--max-price` | Highest whole-instance $/hr to accept. Unpinned launches default to the cheapest matching listed price. `--max-price 0` launches uncapped. |
| `--viewed-price` | Fail instead of launching if the price moved more than a cent above this. |
| `--environment` | `certified`, `certified:<framework>[@<version>]`, `raw-vm[:<os>]` or `provider-template`. See `gpu environments`. |
| `--image` | Launch your own linux/amd64 container image. Combine with `--registry-credential` for a private image, and `--env`, `--entrypoint`, `--cmd`, `--port` and `--app-port` as needed. |
| `--disk` | Filesystem size in GB. |
| `--auto-terminate-hours` | Terminate after N hours of running time. Enforced by the server. |

`delete` also polls its operation (60 seconds by default).

`ssh` waits for the instance to reach `running` (ten minutes by default), then
runs your local `ssh` with the host and port the server reports. `--print`
shows the command instead of running it.

`open` prints the instance's app URL (or with `--terminal`, its browser
console) and the basic-auth login, then opens a browser. Both URLs come from
the server's optional tunnel. On a server without it, use `gpu instances ssh`.

### SSH keys

```sh
gpu keys create --name laptop --public-key-file ~/.ssh/id_ed25519.pub
gpu keys list
gpu keys get <key-id>
gpu keys delete <key-id>
```

### Registry credentials

Stored logins for pulling private images. The password is read from the
`GPUAI_REGISTRY_PASSWORD` environment variable and is never accepted as a flag.

```sh
GPUAI_REGISTRY_PASSWORD=<token> gpu registry-credentials add \
  --name ghcr-acme --registry ghcr.io --username acme-bot
gpu registry-credentials list
gpu registry-credentials remove <id>
gpu instances create -t rtx_4090 --image ghcr.io/acme/trainer:v1 --registry-credential <id>
```

### Operations

Instance create and terminate are asynchronous and return an operation id.

```sh
gpu operations get <operation-id>                     # current state, exits 0
gpu operations get <operation-id> --wait-timeout 10m  # wait; non-zero exit on failure
```

## Endpoints used

| Command | Request |
| --- | --- |
| `gpu-types` | `GET /v1/gpu-types` |
| `pricing` | `GET /v1/pricing` (`gpu_type`, `region`, `tier`, `include_unavailable`, `certified_image`, `deployment_type`, `limit`, `cursor`) |
| `environments` | `GET /v1/environments` |
| `instances list` | `GET /v1/instances` |
| `instances get`, `ssh`, `open` | `GET /v1/instances/{id}` (`get` and `open` add `include=credentials`) |
| `instances create` | `GET /v1/pricing` for the default price cap, `POST /v1/instances`, then `GET /v1/operations/{id}` and `GET /v1/instances/{id}` |
| `instances update` | `PATCH /v1/instances/{id}` |
| `instances delete` | `DELETE /v1/instances/{id}`, then `GET /v1/operations/{id}` |
| `keys` | `GET\|POST /v1/ssh-keys`, `GET\|DELETE /v1/ssh-keys/{id}` |
| `registry-credentials` | `GET\|POST /v1/registry-credentials`, `DELETE /v1/registry-credentials/{id}` |
| `operations get` | `GET /v1/operations/{id}` |

## Errors

| Response | What the CLI prints |
| --- | --- |
| 401 | Not authenticated: run `gpu login` or set `GPUAI_API_KEY`. |
| 404 with an empty body | The server does not have that endpoint (a disabled feature, or a CLI newer than the server). |
| Connection failure | Could not reach the API server: check that gpuctl is running, or set `--api-base` / `GPUAI_API_BASE`. |

## Tests

```sh
go test ./cmd/gpu/... ./internal/apiclient/... ./internal/cliauth/... ./internal/cliprint/...
```

The tests run the CLI in-process against `httptest` servers. They refuse to run
when `GPUAI_API_BASE` points at a non-loopback host, and they ignore any stored
credentials. Set `GPUAI_ALLOW_LIVE_API_IN_TESTS=1` to lift both protections for
a deliberate run against a live server.
