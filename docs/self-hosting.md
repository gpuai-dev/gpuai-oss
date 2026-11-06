# Self-hosting

The quickstart runs everything on one machine with local-only ports and
development passwords. This page covers what to change for a deployment other
people depend on.

## What you run

| Service | Notes |
| --- | --- |
| `gpuctl` | One container or binary (`deploy/Dockerfile.gpuctl` builds a distroless image). Listens on `GPUCTL_PORT` (8080) over plain HTTP. |
| PostgreSQL | Holds all durable state. Tested with PostgreSQL 16 (`postgres:16-alpine` in the compose file and CI). |
| Redis | Holds the offerings cache and rate-limit counters. Nothing in it needs to survive a restart. Tested with Redis 7. |

You do not need the mock supplier. Remove `--profile mock` and configure
`GENERIC_SUPPLIERS` or `VULTR_API_KEY` (see [suppliers.md](suppliers.md)).

Before going live:

- Set a strong `POSTGRES_PASSWORD`, or point `DATABASE_URL` at a managed
  Postgres with `sslmode=require`.
- Set a unique `GPUCTL_ENV` per deployment, for example `prod`.
- Generate `REGISTRY_CREDENTIALS_KEY` (`openssl rand -base64 32`) if callers
  will store private-registry logins, and keep a copy with your secrets.
- Create API keys with `gpuctl apikey create --name NAME`. Keys carry full
  access by default. `--scopes` takes a narrower scope document.
- Keep `.env` files and keys out of version control. `.gitignore` already
  excludes `.env`.

## TLS and the reverse proxy

`gpuctl` does not terminate TLS. Put a reverse proxy in front of it and
expose only the proxy. The compose file publishes gpuctl on `127.0.0.1` for
this reason.

A minimal Caddy configuration:

```
gpu.example.com {
    reverse_proxy 127.0.0.1:8080
}
```

The same with nginx:

```nginx
server {
    listen 443 ssl;
    server_name gpu.example.com;
    ssl_certificate     /etc/ssl/gpu.example.com.crt;
    ssl_certificate_key /etc/ssl/gpu.example.com.key;

    location / {
        proxy_pass http://127.0.0.1:8080;
        proxy_set_header Host $host;
        proxy_set_header X-Forwarded-For $proxy_add_x_forwarded_for;
        proxy_set_header X-Forwarded-Proto $scheme;
    }
}
```

Clients then use `https://gpu.example.com/v1` as the API base
(`gpu login --api-base https://gpu.example.com/v1`).

Paths you may want to treat differently at the proxy:

| Path | Purpose |
| --- | --- |
| `/v1/...` | The public API. |
| `/livez`, `/readyz` | Liveness, and readiness including Postgres and Redis. Use them for load-balancer health checks. They need not be public. |
| `/internal/...` | Callbacks from tunnel bootstraps. Only used with the FRP tunnel. Without it you can block these at the proxy. |

## Instance connectivity

By default instances are reached directly at the address and SSH port the
supplier reports, so callers need network access to the supplier's
instances. Nothing flows through gpuctl. An instance reports `running` once
its SSH endpoint answers, so gpuctl needs network access to instances too. If
it has none, set `GPUCTL_DIRECT_SSH_CHECK=false`.

### Optional FRP tunnel

Setting `FRP_TOKEN` enables an embedded frps server on `FRP_BIND_PORT` (7000)
and makes every launch run a bootstrap script that connects back to it. SSH
and app ports are then served from your gpuctl host on ports from
`FRP_ALLOW_PORTS`. You also need to:

- set `GPUCTL_PUBLIC_URL` to the HTTPS URL of your proxy, so bootstraps can
  call `/internal/instances/{id}/ready` and the other callbacks;
- open `FRP_BIND_PORT` and the `FRP_ALLOW_PORTS` range on the gpuctl host's
  firewall;
- set `FRP_PUBLIC_HOST` if frps is reached under a different name than
  `GPUCTL_PUBLIC_URL`, and `APPS_HOST` with a wildcard DNS record if you want
  app URLs.

The tunnel path compiles and has unit tests, but the end-to-end check in this
repository covers direct-connect mode only.

## Backups

Back up Postgres. It is the only state that matters: instances, operations,
API key hashes, SSH keys, sealed registry credentials and supplier rows.

```sh
docker compose -f deploy/docker-compose.yml exec -T postgres \
  pg_dump -U gpuai -d gpuai --format=custom > gpuai-$(date +%F).dump
```

Restore with `pg_restore --clean --if-exists -d gpuai`. Store
`REGISTRY_CREDENTIALS_KEY` with the backups' secrets. Without it the sealed
registry passwords in a restored database cannot be read.

Redis needs no backup. After a restart the availability poller refills the
offerings cache within 30 seconds.

## Scaling

One `gpuctl` replica is the tested configuration. The API is stateless apart from Postgres and Redis, so you can run
more replicas behind the proxy, with these limits:

- The lifecycle sweeper (stuck-provision reaper and auto-terminate) runs on
  one replica at a time through a Postgres advisory lock.
- Migrations take an advisory lock, so replicas starting together apply each
  migration once.
- The availability poller, health monitor, spot watcher and reconciler run on
  every replica. Each replica polls every supplier, so supplier API traffic
  grows with the replica count.
- Boot tracking for a launch runs in the replica that accepted it.
- With the FRP tunnel, run a single replica. frps runs in-process.

Rate limits are kept in Redis, so they apply across replicas.

## Upgrading

Migrations are embedded in the binary (`internal/db/migrations`) and applied
in order, each in its own transaction, and recorded in `schema_migrations`.
There are no automatic downgrades.

1. Back up Postgres.
2. Read the release notes for changed environment variables.
3. Either let the new version migrate at startup (`GPUCTL_AUTO_MIGRATE=true`,
   the default), or set it to `false` and run `gpuctl migrate` before
   starting the new version. `migrate` needs `DATABASE_URL` and `REDIS_URL`.
4. Replace the binary or image and restart. With compose:

   ```sh
   git pull
   docker compose -f deploy/docker-compose.yml up -d --build
   ```

A restart does not touch running instances. Their state is in Postgres, and
the health monitor checks them again as soon as it starts. Boot tracking for
launches that were still booting is not resumed. The stuck-provision reaper
cleans up any launch that never reaches `running` within its deadline, so
avoid restarting while launches are in flight.

## Logs and monitoring

`gpuctl` logs to stdout, as JSON by default (`GPUCTL_LOG_FORMAT=text` for
plain text). Supplier names and raw supplier errors appear in these logs, so
treat them as operator-only. `/readyz` reports `db` and `redis` status.
