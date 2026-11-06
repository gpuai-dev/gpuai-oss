# Architecture

GPU.ai Open Source is one Go server, `gpuctl`, backed by PostgreSQL and
Redis. Clients (the `gpu` CLI, the SDKs and the MCP server) talk to it over
the `/v1` HTTP API. `gpuctl` talks to GPU suppliers through adapters.

```
  gpu CLI   SDKs   MCP server
      \      |      /
       v     v     v
   +-----------------------+       +------------+
   |  gpuctl               |<----->| PostgreSQL |  instances, operations,
   |   /v1 API             |       +------------+  keys, suppliers, audit
   |   provisioning engine |       +------------+
   |   background workers  |<----->|   Redis    |  offerings cache,
   +-----------------------+       +------------+  rate-limit counters
       |             |
       v             v
  generic adapter   vultr adapter
       |                 |
       v                 v
  suppliers that     Vultr API
  implement the
  Generic Supplier API
  (for example examples/mock-supplier)
```

## Components

| Package | Role |
| --- | --- |
| `cmd/gpuctl` | Entry point. Subcommands `serve` (default), `migrate`, `apikey create` and `version`. `serve` wires every component below and creates the default organization and admin user at startup. |
| `internal/config` | Reads environment variables. See [configuration.md](configuration.md). |
| `internal/db` | Postgres access and the embedded migration runner (`internal/db/migrations`). |
| `internal/api`, `internal/api/public` | HTTP server. `public` holds the `/v1` handlers and their middleware chain (request id, API-key auth, rate limit, scope check, idempotency). `internal/api` adds `/livez`, `/readyz` and the `/internal/...` callbacks that tunnel bootstraps use. |
| `openapi` | `v1.yaml`, embedded into the server and served at `/v1/openapi.json`. |
| `internal/provider` | The `Provider` interface every adapter implements, the adapter registry, shared types (GPU types, tiers, offerings) and per-adapter traits. |
| `internal/provider/generic` | Adapter for the Generic Supplier API. One adapter instance serves every row of the `suppliers` table. |
| `internal/provider/vultr` | Example adapter for a real cloud, enabled by `VULTR_API_KEY`. |
| `internal/supplier` | The `suppliers` table store and the outbound OAuth2 token source for generic suppliers. |
| `internal/availability` | The availability poller, the Redis offerings cache, retail pricing, and the circuit breakers that keep failing suppliers out of the catalog. |
| `internal/provision` | The provisioning engine. Chooses a placement, calls the adapter, tracks boot progress, rescues stalled boots and terminates. |
| `internal/health` | Health monitor for running instances. |
| `internal/spotrouter` | Watches spot instances and reroutes preempted ones. |
| `internal/reconcile` | Drift reconciler between the database and the suppliers. |
| `internal/tunnel` | Optional FRP tunnel and the bootstrap script rendered into launches when it is on. |
| `internal/manifest` | Embedded manifest of certified base images and per-GPU driver floors. |
| `internal/privacy` | Removes configured supplier names from errors returned to API callers. |
| `internal/registrycred`, `internal/registrycheck` | Sealed storage of private-registry passwords, and pre-launch checks that an image exists. |
| `internal/authz`, `internal/imageref`, `internal/instanceid` | Role checks, image reference parsing and instance id generation. |
| `cmd/gpu`, `internal/apiclient`, `internal/cliauth`, `internal/cliprint` | The CLI. It depends on no server package. |

## A launch, step by step

1. **Request.** A client sends `POST /v1/instances` with a GPU type, count and
   tier, plus optional region, SSH key ids, environment or image. The
   middleware assigns a request id, looks up the bearer key by its SHA-256
   hash, applies the per-key rate limit in Redis, checks the key's scopes
   and, when the client sent an `Idempotency-Key`, replays the earlier answer
   for a repeated key.
2. **Validation.** The handler validates the body (GPU type, disk size,
   environment, image reference, registry credential) and resolves the price
   cap the client accepted.
3. **Placement.** The engine reads the offerings that the availability
   poller cached in Redis, filters them (GPU type, count, tier, region,
   price cap, disabled adapters, open breakers, quarantined hosts) and orders
   the candidates by price.
4. **Provision.** The engine writes the instance row and an operation, then
   calls `Provision` on the first candidate's adapter. A no-capacity answer
   moves on to the next candidate. Each attempt is recorded in
   `provision_attempts`. The API returns `202` with the operation id in the
   body and the `Operation-Id` header.
5. **Boot tracking.** A goroutine polls the adapter's `GetStatus` until the
   instance runs, fails or exceeds its boot budget. A stalled boot can be
   rescued once by relaunching on another candidate, within the cost
   allowance.
6. **Running.** Without the tunnel (the default), the instance is marked
   `running` once the supplier reports `running` with an IP address and that
   address answers on SSH.
   The IP and SSH port are stored, and `GET /v1/instances/{id}` returns them
   as `connection.hostname`, `connection.port` and `connection.ssh_command`.
   With the tunnel on, the bootstrap script on the instance connects to the
   embedded frps and calls `/internal/instances/{id}/ready`, and the
   connection details point at the tunnel.
7. **Terminate.** `DELETE /v1/instances/{id}` creates a terminate operation.
   The engine calls the adapter's `Terminate`, verifies the upstream instance
   is gone and marks the row `terminated`.

Supplier identity never reaches API responses. The adapter name, upstream id
and supplier slug are stored, but the public handlers do not emit them, and
`internal/privacy` scrubs supplier names from error text.

## Background loops

All loops start in `gpuctl serve`.

| Loop | Interval | What it does |
| --- | --- | --- |
| Availability poller | 30 s | Calls `ListAvailable` on every registered adapter, applies pricing and filters, and writes the snapshot to Redis (90 s TTL). Breakers skip adapters that keep failing. |
| Health monitor | 60 s | Checks upstream status of running instances and, with the tunnel, SSH reachability through it. |
| Spot preemption watcher | 60 s | Detects reclaimed spot instances and reroutes them. |
| Reconciler | 5 min | Compares the database with what suppliers report: orphans, ghost rows and stuck terminations. Logs only unless `RECONCILER_ENFORCE=true`. The generic adapter cannot list instances, so orphan detection does not cover generic suppliers. |
| Lifecycle sweeper | 60 s | Reaps provisions stuck past their deadline and enforces `auto_terminate_hours`. One replica runs it at a time, elected through a Postgres advisory lock. |
| FRP server | continuous | Only with `FRP_TOKEN` set. |

## Data model

All tables are created by `internal/db/migrations/0001_init.sql`.

| Table | Contents |
| --- | --- |
| `organizations` | The single default organization. |
| `users` | The admin user created at startup. API keys act as this user. |
| `api_keys` | SHA-256 hash, prefix, last four characters, scopes and last-used time of each key. |
| `ssh_keys` | Public SSH keys of the organization and whether they attach to new instances by default. |
| `registry_credentials` | Private-registry logins. The password is sealed with `REGISTRY_CREDENTIALS_KEY`. |
| `suppliers` | Generic suppliers: slug, name, base URL, status and credentials. Seeded from `GENERIC_SUPPLIERS`. |
| `instances` | One row per instance: requested shape, placement, price, status, upstream id and address, SSH port, lifecycle timestamps. |
| `instance_events` | Status transitions and lifecycle events per instance. |
| `operations` | Asynchronous create and terminate operations and their outcome. |
| `public_api_idempotency_keys` | Stored answers for `Idempotency-Key` replays. |
| `provision_attempts` | Each placement attempt of a launch and its result. |
| `provision_failure_events` | Launches that failed, with the reason. |
| `instance_bootstraps` | Per-instance bootstrap state for the tunnel. |
| `instance_ready_reports` | Ready callbacks from tunnel bootstraps. |
| `gpu_telemetry` | GPU samples sent by tunnel bootstraps. |
| `audit_log` | Actions taken through the API, such as instance termination requests. |

## Single tenant

A deployment has one organization and one admin user, created at startup
with external id `default`. Every API key belongs to that organization. There
is no sign-up, billing, balance check or spend limit. Prices pass through
from the suppliers, plus `GPUCTL_MARKUP_PCT` if you set one.
