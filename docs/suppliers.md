# Suppliers

`gpuctl` reaches GPU capacity through adapters. Two ship in this repository.

| Adapter | Enabled by | Use it for |
| --- | --- | --- |
| `generic` | Always registered. Suppliers come from `GENERIC_SUPPLIERS` and the `suppliers` table. | Any supplier that implements the Generic Supplier API below. One adapter serves every configured supplier. |
| `vultr` | `VULTR_API_KEY` | Vultr Cloud GPU. An example of an adapter written against a cloud's own API. |

`examples/mock-supplier` is a working implementation of the Generic Supplier
API and the quickest way to see the contract in action.

## Generic Supplier API

A supplier exposes four HTTP endpoints under its `base_url`. All bodies are
JSON. This section describes the contract as `internal/provider/generic`
implements it.

### Authentication

With `api_key` set on the supplier, every request carries
`Authorization: Bearer <api_key>`. With `SUPPLIER_OAUTH_CLIENT_ID_<SLUG>` and
`SUPPLIER_OAUTH_CLIENT_SECRET_<SLUG>` set, gpuctl instead runs the OAuth2
client-credentials flow against the token URL
(`SUPPLIER_OAUTH_TOKEN_URL_<SLUG>`, default `<base_url>/oauth/token`) and sends
the access token as the bearer. With neither, no `Authorization` header is
sent. Requests also carry `Content-Type: application/json`.

### `GET /v1/gpu/available`

The offerings the supplier can launch now. Polled every 30 seconds with a
10 second timeout.

```json
{
  "offerings": [
    {
      "gpu_type": "rtx_4090",
      "gpu_count": 1,
      "vram_per_gpu_gb": 24,
      "cpu_cores": 8,
      "ram_gb": 32,
      "storage_gb": 200,
      "price_per_hour": 0.35,
      "tier": "on_demand",
      "region": "us-east",
      "datacenter_location": "Example DC 1",
      "stock_status": "available",
      "available_count": 4
    }
  ]
}
```

| Field | Meaning |
| --- | --- |
| `gpu_type` | Canonical GPU id such as `h100_sxm`, `a100_80gb`, `l40s` or `rtx_4090`. The full list is in `internal/provider/types.go`. |
| `gpu_count` | GPUs per instance. |
| `vram_per_gpu_gb`, `cpu_cores`, `ram_gb`, `storage_gb` | Machine shape. Report 0 for anything you do not know rather than a guess. |
| `price_per_hour` | Price of the **whole instance** per hour in USD, covering every GPU in it. It is not multiplied by `gpu_count`. |
| `tier` | `on_demand` or `spot`. |
| `region` | Your region name. API callers filter on it and see it. |
| `datacenter_location` | Free text location. |
| `stock_status` | Free text such as `available` or `low`. |
| `available_count` | How many instances of this offering can be launched now. |

### `POST /v1/instances`

Launch an instance. 30 second timeout. gpuctl expects the call to return
quickly and polls the status endpoint afterwards.

Request:

```json
{
  "instance_id": "gpu-1a2b3c4d",
  "gpu_type": "rtx_4090",
  "gpu_count": 1,
  "tier": "on_demand",
  "region": "us-east",
  "ssh_public_keys": ["ssh-ed25519 AAAA... user@host"],
  "docker_image": "pytorch/pytorch:2.7.1-cuda12.8-cudnn9-devel",
  "startup_script": "#!/bin/bash\n...",
  "registry_auth": {"registry": "ghcr.io", "username": "bot", "password": "..."}
}
```

| Field | Meaning |
| --- | --- |
| `instance_id` | gpuctl's id for the instance. Use it to label the upstream resource. |
| `gpu_type`, `gpu_count`, `tier`, `region` | The offering to launch. `region` is omitted when empty. |
| `ssh_public_keys` | Keys to install for `root`. Omitted when empty. |
| `docker_image` | Image to run. Always present. When the caller chose no image, gpuctl sends a pinned certified image or `pytorch/pytorch:2.7.1-cuda12.8-cudnn9-devel`. Suppliers that hand out whole machines may ignore it. |
| `startup_script` | Script to run at boot, sent when gpuctl needs the instance to run its bootstrap (the FRP tunnel). Omitted when empty. |
| `registry_auth` | Login for a private `docker_image`. Optional, and only sent for a private image. Treat it as a secret. |

Response (`2xx`):

```json
{
  "upstream_id": "inst-9f8e7d",
  "status": "creating",
  "cost_per_hour": 0.35,
  "estimated_ready_seconds": 60,
  "datacenter_location": "Example DC 1",
  "region": "us-east"
}
```

`upstream_id` is your id for the instance. gpuctl stores it as
`<slug>:<upstream_id>` and uses it in the two calls below. `cost_per_hour` is
what you will charge. The generic adapter does not send or enforce a price
ceiling, so charge the `price_per_hour` you advertised.

When you cannot place the request, answer with a `4xx` (`409` is
conventional) whose body contains one of `no_capacity`, `no capacity`,
`insufficient capacity`, `out of capacity`, `out of stock` or
`no availability`. gpuctl then tries the next candidate offering. Any other
error fails that attempt.

### `GET /v1/instances/{upstream_id}`

Current status. Polled while the instance boots and by the health monitor
while it runs. 10 second timeout.

```json
{
  "upstream_id": "inst-9f8e7d",
  "status": "running",
  "ip": "203.0.113.20",
  "ssh_port": 22022,
  "cost_per_hour": 0.35,
  "uptime_seconds": 42
}
```

| `status` | gpuctl reads it as |
| --- | --- |
| `running` | Booted. With `ip` set and no tunnel, gpuctl marks the instance `running` and hands out `ip` and the SSH port. |
| `terminated`, `stopped`, `error` | Gone or failed. |
| anything else (for example `creating`) | Still booting. |

`ssh_port` is optional. It is the public TCP port that reaches the instance's
sshd at `ip`. Leave it out and callers connect on port 22.

The instance must run sshd with the requested public keys. Without the tunnel,
gpuctl renders no bootstrap and does not install one. An instance stays
`starting` until `ip` and `ssh_port` answer with an SSH banner, and a launch that
never answers fails at its provisioning deadline.

### `DELETE /v1/instances/{upstream_id}`

Terminate. 30 second timeout. Return `2xx` once termination has started or
the instance is already gone. The body is ignored. Make it idempotent:
gpuctl retries a terminate that failed, and a `4xx` for an instance that no
longer exists is reported as a failed terminate.

### Errors, retries and timeouts

- `429` is retried after `Retry-After` seconds (5 seconds when the header is
  missing).
- `5xx` and network errors are retried with backoff (1 s, then 2 s), three
  attempts in total.
- Other `4xx` responses are not retried. The body is logged for the operator
  and replaced by a neutral message for API callers.
- A supplier whose availability poll keeps failing is removed from the
  catalog by the circuit breaker until it recovers.

### Limitations

- gpuctl routes a launch to a supplier by (`gpu_type`, `region`, `tier`). If two
  generic suppliers advertise the same combination, the supplier polled last
  receives the launch. Give suppliers distinct region names to avoid this.
- There is no list endpoint, so the reconciler cannot find upstream
  instances that gpuctl no longer tracks for generic suppliers.

### Configuring a supplier

```sh
GENERIC_SUPPLIERS=[{"slug":"acme-gpu","name":"Acme GPU","base_url":"https://gpu-api.example.com","api_key":"replace-me"}]
```

The fields are documented in
[configuration.md](configuration.md#generic_suppliers). The slug and name are
operator-facing only. API callers never see which supplier runs their
instance.

## The mock supplier

`examples/mock-supplier` implements the four endpoints in one Go file plus a
small Docker client. With `MOCK_RUNTIME=docker` (the compose default) each
launch starts a container from `examples/mock-supplier/instance`, an Alpine
image running sshd with the requested public keys, and publishes its port 22
on the host. It reports `running` with `ip` and `ssh_port` once sshd answers,
and removes the container on `DELETE`. `GET /mock/instances` lists everything
it created.

It returns the no-capacity error when no offering matches or the stock is
used up, so it is also a reference for the error contract. Its environment
variables are listed in [configuration.md](configuration.md#mock-supplier).

Run it on its own with `go run ./examples/mock-supplier`, or through the
compose profile `mock`.

## Writing a new adapter

Use the generic adapter when you control the supplier side or can put a
small service in front of it. Write a native adapter when you need to call a
cloud's own API directly, as the Vultr adapter does.

1. **Create a package** under `internal/provider/<name>` with a type that
   implements `provider.Provider` (`internal/provider/provider.go`).

   | Method | Contract |
   | --- | --- |
   | `Name()` | Stable adapter name, for example `vultr`. Used in logs, `PROVIDERS_DISABLED` and per-adapter variables. Never shown to API callers. |
   | `ListAvailable(ctx)` | Current offerings as `[]provider.GPUOffering`. `PricePerHour` is the whole-instance price. Called every 30 seconds. |
   | `Provision(ctx, req)` | Start an instance and return at once with an upstream id. Wrap `provider.ErrNoCapacity` when the offering is gone, so the engine tries the next one. |
   | `GetStatus(ctx, upstreamID, region)` | Status, IP and port mappings. Report a `PortMapping` with `PrivatePort: 22` so direct-connect mode knows the SSH port. |
   | `Terminate(ctx, upstreamID, region)` | Destroy the instance. Return nil when it is already gone. |
   | `ListProvisioned(ctx)` | Every live instance the adapter created, for the reconciler. Return `provider.ErrNotImplemented` if you cannot list safely. |

   Optional interfaces in the same package let an adapter declare more, for
   example `ProvisionCapability` and the spot interfaces in `spot.go`.

2. **Declare its traits** with `provider.RegisterTraits(name, provider.Traits{...})`
   (`internal/provider/bootprofile.go`). The zero value describes a
   container-native adapter that boots fast. Set `VM` and `SlowBoot` for
   adapters that hand out whole machines, and `ProvisioningEstimate` for
   the typical time to ready.

3. **Add configuration** to `internal/config/config.go`, usually an API key
   that enables the adapter, and document it in
   [configuration.md](configuration.md) and `.env.example`.

4. **Register it** in `cmd/gpuctl/deps.go`, next to the Vultr adapter, behind
   its enabling variable.

5. **Test it.** Unit-test the mapping from the supplier's responses with
   recorded fixtures. Use fake keys, `example.com` hosts and documentation IP
   ranges (`203.0.113.0/24`). Gate any test that calls the real API on an
   environment variable so `go test ./...` stays offline.

Keep supplier errors out of customer-facing messages. Log the detail and
return a neutral error, as `internal/provider/generic/adapter.go` does.

## The Vultr adapter

Results vary by plan and region. In real launches on 2026-11-06, the 1x A16 reached SSH with a working GPU in `ewr`, `nrt`, `fra`, and `sgp`, and the 1x A100 did in `ewr`, usually in about two minutes. The A16 in `sjc` and `blr` booted but never answered on the network, and the A40 in `lax` never left `pending`. This is a snapshot from a small sample, so it can change. A launch that stalls fails at its provisioning deadline and is cleaned up.

`internal/provider/vultr` maps Vultr Cloud GPU plans to offerings, launches
VMs from Vultr's GPU-ready Ubuntu image and terminates them through the Vultr
API. Enable it with `VULTR_API_KEY`. Vultr VMs take several minutes to boot,
so they are listed with `instant_boot: false`.

Vultr enforces an instance-count cap and a monthly-cost cap per account, and
neither is visible through its API. The adapter mirrors them
(`VULTR_MAX_INSTANCES`, `VULTR_MAX_MONTHLY_COST_USD`) and hides offerings that
would exceed them, so callers see less capacity instead of a failed launch.
Set both to your account's limits.

A live probe is excluded from the normal test run by a build tag. It creates
one real instance (a 1x A16, under $0.50) and terminates it:

```sh
VULTR_API_KEY=... go test -tags liveprobe -run TestLiveProbe_MapStatus -v -timeout 15m ./internal/provider/vultr/
```
