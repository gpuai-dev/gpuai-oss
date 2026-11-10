<p align="center">
  <a href="https://gpu.ai">
    <picture>
      <source media="(prefers-color-scheme: dark)" srcset="docs/assets/gpuai-logo-dark.png">
      <img src="docs/assets/gpuai-logo-light.png" alt="GPU.ai" width="300">
    </picture>
  </a>
</p>

# GPU.ai Open Source

GPU.ai Open Source is a self-hostable server for provisioning GPU instances
across suppliers. You run one API, connect it to the GPU suppliers you have
accounts with, and launch, list, reach and terminate instances through that
API, a CLI, SDKs or an MCP server. It is the provisioning engine that GPU.ai
runs, released with the hosted platform's billing, accounts and other
services removed.

## What is included

| Component | Path | Description |
| --- | --- | --- |
| `gpuctl` | `cmd/gpuctl` | The server. Serves the `/v1` API and runs the background workers that poll supplier availability, drive provisioning, check instance health and reconcile drift. Needs PostgreSQL and Redis. |
| Supplier framework | `internal/provider` | The adapter interface plus two adapters. `generic` talks to any supplier that implements the Generic Supplier API. `vultr` is an example adapter for a real cloud. |
| Mock supplier | `examples/mock-supplier` | A reference implementation of the Generic Supplier API that runs each instance as a small SSH container on your Docker host. The quickstart uses it. |
| `gpu` CLI | `cmd/gpu` | Command-line client. |
| SDKs | `sdk/python`, `sdk/typescript` | Clients generated from `openapi/v1.yaml`. |
| MCP server | `mcp` | Exposes the API as tools to MCP clients such as Claude Code. |
| API spec | `openapi/v1.yaml` | OpenAPI 3.1 description of the API. The server embeds it and serves it at `/v1/openapi.json`. |
| Deployment | `deploy` | Dockerfile and docker-compose file for Postgres, Redis, gpuctl and the mock supplier. |

The API covers GPU types, the live offerings catalog, environments, instances,
asynchronous operations, SSH keys and private-registry credentials. The
endpoints, paths and JSON shapes match GPU.ai's hosted API.

## Quickstart

This brings up a complete deployment on one machine and launches an instance
on the mock supplier. You need git, Docker with the Compose plugin (2.24 or
newer), Go 1.25 or newer, and an SSH client.

**1. Start the stack.**

```sh
git clone https://github.com/gpuai-dev/gpuai-oss.git
cd gpuai-oss
docker compose -f deploy/docker-compose.yml --profile mock up -d --build
curl http://localhost:8080/v1/health
```

The health check prints `{"status":"ok"}`. The first build takes a few
minutes. The mock supplier needs the Docker socket to start instance
containers, so only run this profile on a machine you control.

**2. Create an API key.** The key is printed once and only its hash is stored.

```sh
KEY=$(docker compose -f deploy/docker-compose.yml exec -T gpuctl gpuctl apikey create --name admin)
```

**3. Build the CLI and log in.**

```sh
make build
echo "$KEY" | ./bin/gpu login --stdin
```

`make build` writes `bin/gpuctl`, `bin/gpu` and `bin/mock-supplier`. Without
make, run `go build -o bin/gpu ./cmd/gpu`.

**4. Look at the catalog.** The mock supplier advertises three offerings.

```sh
./bin/gpu pricing -o table
```

If the list is empty, wait a few seconds. The server polls suppliers every 30
seconds and right after it starts.

**5. Register an SSH key.** This makes a dedicated key in `bin/`, which git
ignores.

```sh
ssh-keygen -t ed25519 -N '' -f bin/quickstart_key
./bin/gpu keys create --name quickstart --public-key-file bin/quickstart_key.pub
```

**6. Launch an instance.**

```sh
./bin/gpu instances create -t rtx_4090 -o table
./bin/gpu instances list -o table
```

`create` waits until the launch operation succeeds and prints the instance,
including its ID (for example `gpu-1a2b3c4d`). The instance is `running` a few
seconds later.

**7. Connect to it.** Replace `<id>` with the instance ID. Arguments after `--`
go to `ssh`.

```sh
./bin/gpu instances ssh <id> -- -i bin/quickstart_key uname -a
```

Leave out `uname -a` for an interactive shell.

**8. Terminate it.**

```sh
./bin/gpu instances terminate <id>
./bin/gpu instances list --all -o table
```

The instance shows as `terminated`, and the mock supplier has removed its
container. Stop the stack with
`docker compose -f deploy/docker-compose.yml --profile mock down -v`.

`scripts/e2e.sh` (or `make e2e`) runs the same flow against the API with curl.
CI runs it on every push.

## Install the CLI

```sh
go install ./cmd/gpu      # from a checkout, into $(go env GOPATH)/bin
# or
make build                # into ./bin
```

See [docs/cli.md](docs/cli.md) for every command.

## Use your own suppliers

Point `GENERIC_SUPPLIERS` at suppliers that implement the Generic Supplier
API, or set `VULTR_API_KEY` to enable the Vultr adapter, whose results vary by
plan and region (see [docs/suppliers.md](docs/suppliers.md)). Put the values in a
`deploy/.env` file (start with `cp .env.example deploy/.env`) and restart
with `docker compose -f deploy/docker-compose.yml up -d`. Leave out
`--profile mock` once you no longer need the mock supplier.

```sh
GENERIC_SUPPLIERS=[{"slug":"acme-gpu","name":"Acme GPU","base_url":"https://gpu-api.example.com","api_key":"replace-me"}]
```

Supplier names are kept out of API responses. Operators see them in the
server logs.

## Documentation

| Document | Contents |
| --- | --- |
| [docs/architecture.md](docs/architecture.md) | Components, the path of a launch, background loops and the data model. |
| [docs/configuration.md](docs/configuration.md) | Every environment variable. |
| [docs/suppliers.md](docs/suppliers.md) | The Generic Supplier API and how to write an adapter. |
| [docs/self-hosting.md](docs/self-hosting.md) | TLS, backups, scaling and upgrades for a real deployment. |
| [docs/cli.md](docs/cli.md) | The `gpu` CLI. |
| [docs/sdk.md](docs/sdk.md) | The Python and TypeScript SDKs. |
| [docs/mcp.md](docs/mcp.md) | The MCP server. |
| [docs/REMOVED.md](docs/REMOVED.md) | What was removed or replaced from GPU.ai's hosted platform. |

## Development

```sh
make test     # Go, SDK and MCP tests
make lint     # gofmt, go vet and scripts/check-forbidden.sh
make e2e      # the quickstart, end to end
```

See [CONTRIBUTING.md](CONTRIBUTING.md). Report security issues as described in
[SECURITY.md](SECURITY.md).

## License

Apache License 2.0. See [LICENSE](LICENSE).
