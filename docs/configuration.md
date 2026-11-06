# Configuration

`gpuctl` is configured entirely through environment variables. Only
`DATABASE_URL` and `REDIS_URL` are required. Every other variable has a
default that runs a single-node, single-tenant deployment with no markup and
no tunnel.

`deploy/docker-compose.yml` reads `deploy/.env` when it exists, both for its
own settings and as the gpuctl environment. Copy `.env.example` to
`deploy/.env` to start. Every line in it is
commented out and shows the default.

Malformed values behave in one of two ways. A value whose silent fallback could
misprice or misroute a launch (pricing, disk sizes, breaker counts, supplier
JSON, the registry key) stops `gpuctl` at startup with an error naming the
variable. A malformed tuning value (durations, booleans) logs a warning and
uses the default.

Durations use Go syntax, for example `90s`, `8m` or `1h30m`. Booleans accept
`true`/`false`, `1`/`0`, `t`/`f`.

## Required

| Variable | Description |
| --- | --- |
| `DATABASE_URL` | PostgreSQL connection string, for example `postgres://<user>:<password>@<host>:5432/<database>?sslmode=disable`. |
| `REDIS_URL` | Redis connection string, for example `redis://redis:6379/0`. Redis holds the live offerings cache and the per-key rate-limit counters. |

`gpuctl migrate` and `gpuctl apikey create` load the same configuration, so
they also need both variables.

## Server

| Variable | Default | Description |
| --- | --- | --- |
| `GPUCTL_PORT` | `8080` | HTTP listen port. |
| `GPUCTL_ENV` | `development` | Deployment name. `production` and `demo` are stamped onto upstream resource names as `prod-` and `demo-`, and the reconciler only acts on names carrying its own tag. Any other value leaves names untagged. Two deployments sharing one supplier account must use different tags, and at most one may be untagged, or they will terminate each other's instances as orphans. |
| `GPUCTL_PUBLIC_URL` | empty | Externally reachable base URL of this server. Only used with the FRP tunnel, where instance bootstraps call back to it and the tunnel host defaults to its hostname. |
| `GPUCTL_AUTO_MIGRATE` | `true` | Apply pending database migrations at startup. Set `false` and run `gpuctl migrate` as a separate step if you prefer. |
| `GPUCTL_DIRECT_SSH_CHECK` | `true` | Without the tunnel, report an instance `running` only once its SSH endpoint answers with an SSH banner. Set `false` if this server has no network route to instances. Loopback addresses are never checked. |
| `GPUCTL_BOOTSTRAP_API_KEY` | empty | A full-access API key seeded at startup for the default organization. It must start with `gpuai_live_` followed by at least 24 characters. Only its SHA-256 hash is stored. The alternative is `gpuctl apikey create --name NAME`. |
| `GPUCTL_LOG_LEVEL` | `info` | `debug`, `info`, `warn` or `error`. The `--log-level` flag of `gpuctl serve` overrides it. |
| `GPUCTL_LOG_FORMAT` | `json` | `json` or `text`. The `--log-format` flag overrides it. docker-compose sets `text`. |
| `OFFERINGS_CACHE_KEY` | built-in key | Redis key for the offerings snapshot. Set a distinct value when two deployments share one Redis. |
| `REGISTRY_CREDENTIALS_KEY` | empty | Base64 encoding of a 32-byte key that seals stored private-registry passwords. Generate one with `openssl rand -base64 32`. Empty makes `/v1/registry-credentials` answer 503. A present but malformed value stops startup. Changing it makes stored credentials unreadable. |
| `AUTHZ_ENFORCE` | `false` | Role checks on instance writes. `false` logs would-deny decisions, `true` returns 403. |

## Suppliers

| Variable | Default | Description |
| --- | --- | --- |
| `GENERIC_SUPPLIERS` | empty (compose sets the mock) | JSON array of suppliers that implement the Generic Supplier API. See below and [suppliers.md](suppliers.md). |
| `SUPPLIER_OAUTH_CLIENT_ID_<SLUG>` | empty | OAuth2 client-credentials ID for one generic supplier. `<SLUG>` is the slug in upper case with dashes as underscores (`acme-gpu` becomes `ACME_GPU`). Takes precedence over that supplier's `api_key`. |
| `SUPPLIER_OAUTH_CLIENT_SECRET_<SLUG>` | empty | The matching client secret. |
| `SUPPLIER_OAUTH_TOKEN_URL_<SLUG>` | `<base_url>/oauth/token` | Token endpoint for that supplier. |
| `VULTR_API_KEY` | empty | Enables the Vultr adapter. Empty leaves it unregistered. |
| `VULTR_MAX_INSTANCES` | `15` | Mirrors the instance cap Vultr enforces on the account. `0` disables the guard. |
| `VULTR_MAX_MONTHLY_COST_USD` | `4000` | Mirrors the monthly cost cap Vultr enforces on the account. `0` disables the guard. |
| `PROVIDERS_DISABLED` | empty | Comma-separated adapter names (`generic`, `vultr`). A disabled adapter leaves the catalog and placement, but stays registered so existing instances can still be read and terminated. |

### `GENERIC_SUPPLIERS`

```json
[
  {
    "slug": "acme-gpu",
    "name": "Acme GPU",
    "base_url": "https://gpu-api.example.com",
    "api_key": "replace-me",
    "regions": ["us-east"],
    "disabled": false
  }
]
```

| Field | Required | Meaning |
| --- | --- | --- |
| `slug` | yes | Stable identifier, 2 to 63 characters of `[a-z0-9-]`, starting with a letter or digit. It prefixes stored upstream ids, so do not change it while instances are running. |
| `base_url` | yes | `http` or `https` root of the supplier API. The adapter calls `<base_url>/v1/...`. A trailing slash is removed. |
| `name` | no | Display name for operators. Defaults to the slug. Never shown to API callers. |
| `api_key` | no | Sent as `Authorization: Bearer <api_key>` on every call. |
| `regions` | no | Informational list of the supplier's regions. |
| `disabled` | no | `true` keeps the supplier row but marks it suspended, so it is not polled or used. |

Entries are upserted into the `suppliers` table at startup. Rows inserted into
that table directly are used too.

## Pricing

Prices pass through from suppliers unchanged by default. Per-adapter variables
take the adapter name in upper case as `<NAME>`, for example `VULTR` or
`GENERIC`.

| Variable | Default | Description |
| --- | --- | --- |
| `GPUCTL_MARKUP_PCT` | `0` | Markup over supplier prices, in percent. |
| `PRICING_MARKUP_PCT_<NAME>` | unset | Per-adapter markup override. |
| `PRICING_MIN_RETAIL_MARKUP_PCT` | `0` | Price floor. No offering is listed or launched below cost x (1 + pct/100), rounded up to the cent. |
| `PRICING_COST_DRIFT_TOLERANCE_PCT` | `5` | How far, in percent, a supplier's accepted cost may exceed its quote before the launch is refused and re-quoted. |
| `PRICING_COST_DRIFT_TOLERANCE_ABS` | `0.02` | The same tolerance in $/hr. |
| `PRICING_RESCUE_COST_ALLOWANCE_PCT` | `150` | How much more, in percent, a boot-stall rescue replacement may cost than the original placement. |
| `PRICING_RESCUE_COST_ALLOWANCE_ABS` | `0.25` | The same allowance in $/hr. |
| `PROVIDER_COST_DISCOUNT_PCT_<NAME>` | unset | Negotiated discount below an adapter's list price, in `[0,100)`. |

## Instance disk

| Variable | Default | Description |
| --- | --- | --- |
| `LAUNCH_DEFAULT_DISK_GB` | `100` | Disk for launches that request none. |
| `LAUNCH_INCLUDED_DISK_GB` | `100` | Disk included in the hourly rate. |
| `LAUNCH_MAX_DISK_GB` | `250` | Largest `disk_gb` a launch may request. |
| `DISK_PRICE_PER_GB_HOUR` | `0` | Hourly cost of each GB beyond the included size. |
| `PROVIDER_DISK_PRICE_PER_GB_HOUR_<NAME>` | unset | Per-adapter override of the disk rate. |

## Provisioning engine

| Variable | Default | Description |
| --- | --- | --- |
| `PROVISION_DEFAULT_ENVIRONMENT` | `certified:pytorch` | Environment applied to launches that name none on container placements. `certified[:framework]`, or `off` to keep each supplier's own default image. |
| `PROVISION_DEFAULT_ENVIRONMENT_SCOPE` | `incompatible` | `all` applies the default environment to every GPU the image manifest covers. `incompatible` applies it only where the supplier default cannot run. |
| `PROVISION_TUNNEL_STALL_AFTER` | `8m` | Boot-stall watchdog window for a tunnel that never connects. `0` disables it. |
| `PROVISION_PULL_STALL_AFTER` | `8m` | Boot-stall window for a boot whose progress message stops changing. `0` disables it. |
| `PROVISION_BOOT_RESCUES` | `1` | Rescue relaunches allowed per launch. |
| `PROVISION_HOST_QUARANTINE` | on | Keeps hosts whose bootstrap never reported back out of placement for a while. `off` or `no` disables it. |
| `PROVISION_PLACEMENT_MEMORY` | on | Backs off offerings that keep failing to place. `off` or `no` disables it. |
| `PROVISION_GPU_CHECK` | `shadow` | Startup GPU compute check in tunnel bootstraps: `off`, `shadow` (report only) or `enforce`. Direct-connect launches never run it. |

## Availability and circuit breakers

The availability poller asks every adapter for offerings every 30 seconds.

| Variable | Default | Description |
| --- | --- | --- |
| `BREAKER_FAILURE_THRESHOLD` | `5` | Consecutive failed polls before an adapter's breaker opens. |
| `BREAKER_COOLDOWN_SECONDS` | `60` | How long an open breaker waits before probing again. |
| `BREAKER_PROBE_TIMEOUT_SECONDS` | `10` | Timeout of one availability poll. |
| `AVAILABILITY_RETENTION_SECONDS` | `300` | How long an adapter's last good snapshot is served after a failed poll. |
| `LAUNCH_BREAKER_WINDOW` | `0` (built-in) | Number of recent launches the launch breaker looks at per adapter and GPU type. |
| `LAUNCH_BREAKER_MIN_SAMPLES` | `0` (built-in) | Launches needed before the launch breaker can trip. |
| `LAUNCH_BREAKER_FAIL_RATE` | `0` (built-in) | Failure rate, between 0 and 1, that trips it. |
| `LAUNCH_BREAKER_COOLDOWN_SECONDS` | `600` | How long a tripped adapter and GPU type stay suppressed. |

## Reconciler

| Variable | Default | Description |
| --- | --- | --- |
| `RECONCILER_ENFORCE` | `false` | `false` logs what the drift reconciler would do. `true` acts, for example terminating upstream orphans. |
| `RECONCILER_INTERVAL_SECONDS` | `0` (5 minutes) | Time between reconciler passes. |
| `RECONCILER_RECENT_TERM_WINDOW_HOURS` | `0` (4 hours) | How far back recently terminated instances are rechecked. |

## Optional FRP tunnel

The tunnel is off by default. Instances are then reached directly at the
address and SSH port the supplier reports. Setting `FRP_TOKEN` starts an
embedded frps server and renders a bootstrap script into every launch that
connects back through it. See [self-hosting.md](self-hosting.md#optional-frp-tunnel).

| Variable | Default | Description |
| --- | --- | --- |
| `FRP_TOKEN` | empty | Shared secret between frps and the frpc client on each instance. Setting it enables the tunnel. |
| `FRP_BIND_PORT` | `7000` | Port frps listens on for frpc clients. |
| `FRP_ALLOW_PORTS` | `10000-10255` | Remote port range handed out to instances. |
| `FRP_PUBLIC_HOST` | hostname of `GPUCTL_PUBLIC_URL` | Public hostname of frps. |
| `FRP_TRANSPORT_<NAME>` | unset | Per-adapter frpc transport: `tcp`, `websocket`, `wss`, `kcp` or `quic`. |
| `APPS_HOST` | empty | Wildcard host for app URLs served through the tunnel (`<instance>.<APPS_HOST>`). |
| `WEB_TERMINAL_DISABLED` | `false` | `true` turns off the browser console in tunnel bootstraps. |
| `TUNNEL_FAIL_THRESHOLD` | `0` (built-in) | Failed reachability probes before an instance is flagged. |
| `TUNNEL_ESCALATE_AFTER` | `0` (built-in) | Duration after which a flagged instance is escalated. |

## docker-compose only

These are read by `deploy/docker-compose.yml`, not by `gpuctl`.

| Variable | Default | Description |
| --- | --- | --- |
| `POSTGRES_PASSWORD` | `gpuai-local-dev` | Password of the bundled Postgres. Change it for anything but local use. |
| `POSTGRES_PORT` | `5432` | Host port Postgres is published on (127.0.0.1 only). |
| `GPUCTL_HOST_PORT` | `8080` | Host port gpuctl is published on (127.0.0.1 only). |
| `MOCK_HOST_PORT` | `8081` | Host port of the mock supplier API (profile `mock`). |
| `MOCK_RUNTIME` | `docker` | Passed to the mock supplier. |
| `MOCK_PUBLIC_IP` | `127.0.0.1` | Passed to the mock supplier. |
| `MOCK_PUBLISH_IP` | `127.0.0.1` | Passed to the mock supplier. |

## Mock supplier

`examples/mock-supplier` reads its own variables.

| Variable | Default | Description |
| --- | --- | --- |
| `MOCK_LISTEN` | `:8080` | Listen address. |
| `MOCK_API_KEY` | empty | When set, every `/v1` call must send `Authorization: Bearer <MOCK_API_KEY>`. |
| `MOCK_RUNTIME` | `docker` when the Docker socket exists, else `simulated` | `docker` runs each instance as an sshd container. `simulated` only tracks state. |
| `MOCK_DOCKER_HOST` | `/var/run/docker.sock` | Docker Engine socket. |
| `MOCK_INSTANCE_IMAGE` | `gpuai-oss-mock-instance:latest` | Image run per instance, built from `examples/mock-supplier/instance`. |
| `MOCK_DOCKER_NETWORK` | empty | Network instance containers join, so the mock can wait for sshd before reporting `running`. |
| `MOCK_PUBLIC_IP` | `127.0.0.1` | Address reported to gpuctl as the instance IP. |
| `MOCK_PUBLISH_IP` | `127.0.0.1` | Host interface the SSH ports are published on. |
| `MOCK_OFFERINGS` | three built-in offerings | JSON array that replaces the advertised offerings. |
| `MOCK_READY_DELAY` | `3s` | Extra delay before a simulated instance reports `running`. |

## Clients

The CLI, SDKs and MCP server read these. See [cli.md](cli.md), [sdk.md](sdk.md)
and [mcp.md](mcp.md).

| Variable | Used by | Description |
| --- | --- | --- |
| `GPUAI_API_KEY` | CLI, SDKs, MCP | API key. Required by the MCP server. |
| `GPUAI_API_BASE` | CLI, SDKs, MCP | API base URL including `/v1`. Defaults to `http://localhost:8080/v1`. |
| `GPUAI_REGISTRY_PASSWORD` | CLI | Password for `gpu registry-credentials add`. |
| `XDG_CONFIG_HOME` | CLI | Where `gpu login` stores credentials (`$XDG_CONFIG_HOME/gpu/credentials.json`, else `~/.config/gpu/credentials.json`). |

## Tests only

| Variable | Description |
| --- | --- |
| `DATABASE_TEST_URL` | Postgres for the database tests, migrated with `gpuctl migrate`. Unset skips them. |
| `REDIS_TEST_URL` | Redis for the Redis tests. Unset skips them. |
| `GPUAI_ALLOW_LIVE_API_IN_TESTS` | `1` lets the CLI tests run against a non-loopback `GPUAI_API_BASE` and stored credentials. |
| `REGCHECK_LIVE` | `1` runs the registry check tests against real public registries. |
| `VULTR_API_KEY` | Also used by the Vultr live probe, which only builds with `-tags liveprobe` and launches a real instance. |
