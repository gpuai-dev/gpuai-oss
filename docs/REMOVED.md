# What was removed or replaced

GPU.ai Open Source is built from the code GPU.ai runs in production. This
file lists what was taken out of that code base, or replaced, to make a
self-hostable release. It is kept at the level of components and categories.

Nothing listed here ships in this repository, in its history, or in its
tests and fixtures.

## Secrets, data and internal identifiers

| Removed | Replaced with |
| --- | --- |
| Every secret, credential, token and supplier API key, including CI and deployment secrets. | Nothing. Configuration comes from environment variables you set. Test fixtures use clearly fake values (`gpuai_test_...`, `tok_frp_test_...`, `mock-supplier-dev-key`). |
| Customer data of any kind: database rows, exports, logs, instance records, customer names and support-ticket details in comments. | A schema-only migration (`internal/db/migrations/0001_init.sql`). Test data is synthetic. |
| Internal hostnames of GPU.ai's hosted deployment (API, tunnel, registry, app and demo hosts), object-storage bucket names, server and NAT IP addresses. | `example.com` names and documentation IP ranges such as `203.0.113.0/24`. |
| Cloud account identifiers, resource names (ARNs), and infrastructure-as-code (Terraform, container service and log-group configuration). | `deploy/docker-compose.yml` and `deploy/Dockerfile.gpuctl`. See [self-hosting.md](self-hosting.md). |
| Production database dumps and data migrations. | One consolidated schema migration. |

## Hosted-platform services

| Removed | Notes |
| --- | --- |
| Billing: Stripe payments, invoices, deposits, crypto payments, credits and auto-credits, balances, spending limits, burn and spend caps, usage metering, payment reconciliation. | Prices pass through from suppliers. `GPUCTL_MARKUP_PCT` (default 0) is kept from the pricing path. There are no balance checks. |
| Clerk user accounts, sign-up, sessions, organizations and the device-login flow. | API keys stored as SHA-256 hashes in Postgres, created with `gpuctl apikey create` or `GPUCTL_BOOTSTRAP_API_KEY`. One default organization. |
| Account tiers, abuse and fraud screening, self-dealing checks, and identity and verification services. | None. A self-hosted deployment trusts its own key holders. |
| AWS integrations: S3 (bootstrap asset mirror and storage), Secrets Manager, RDS IAM authentication, CloudWatch. | Postgres and Redis only. Bootstraps download tools from their public release URLs. |
| Sentry error reporting (server and dashboard). | Structured logs on stdout. |
| Slack alerting and upstream-alert routing, email through Resend, and all customer notifications. | Logs. |
| Webhooks and the instance-event publisher. | None. |
| Analytics, marketing attribution and competitor tracking. | None. |
| The web dashboard (frontend), its documentation site generator and every dashboard-only API route. | The `/v1` API, CLI, SDKs and MCP server. |
| Every admin and staff route and tool. | `gpuctl` subcommands (`migrate`, `apikey create`). |

## Product features outside provisioning

| Removed | Notes |
| --- | --- |
| Inference: chat completions, embeddings, image and video generation, model catalog, model VRAM sizing and media jobs. | Out of scope. |
| Fine-tuning jobs and training files. | Out of scope. |
| Templates and template deploys. | The template catalog ships empty, so a request with `template_id` is rejected. `template_id` was dropped from the OpenAPI spec. |
| Community features, the community-cloud marketplace and its adapter, supplier-owned node agent (`gpud`) and supplier balances. | Out of scope. |
| Autolaunch, watch rules, market price history and reservations (the reserved tier). | Reserved-tier launches return an error. |
| Reliability scoring, fleet probes and chain probes. | The engine keeps its placement memory, host quarantine and circuit breakers. |
| The inbound OAuth server suppliers used to authenticate to GPU.ai. | Outbound OAuth client credentials to suppliers are kept (`SUPPLIER_OAUTH_*`). |
| `PATCH /v1/ssh-keys/{id}`. | Not part of the open-source API surface. |

## Supplier adapters

| Removed | Replaced with |
| --- | --- |
| 10 other supplier adapters for specific GPU clouds, and every supplier-specific mapping, region label, trait entry, image table and test fixture. | The `generic` adapter for the Generic Supplier API, the `vultr` adapter as an example of a native adapter, and `examples/mock-supplier`. See [suppliers.md](suppliers.md). |
| Brand-keyed adapter trait tables. | `provider.RegisterTraits`, so any adapter can declare its own traits. |
| VM OS image tables that only mapped removed adapters. | None. `raw_vm` OS choices are empty. Vultr boots its default GPU image. |

## Changed behaviour

These were adapted, not removed, so the code runs without the hosted platform.

| Area | Hosted platform | GPU.ai Open Source |
| --- | --- | --- |
| Reaching instances | Always through the FRP/WireGuard tunnel. An instance became `running` when its bootstrap called back. | Direct connect by default. An instance is `running` when the supplier reports it running with an IP, and callers connect to the supplier's IP and port. The tunnel is optional (`FRP_TOKEN`). |
| Generic Supplier API | Status response without a port. | Optional `ssh_port` field on `GET /v1/instances/{id}`. Additive. |
| Stuck-provision reaper and auto-terminate | Ran on the billing ticker. | Run in a lifecycle sweeper, leader-elected through a Postgres advisory lock. |
| Health dead-window credit | Credited the customer's balance. | No-op hooks. |
| Supplier names in errors | A fixed list of brand names was scrubbed. | The configured supplier slugs and names are scrubbed (`internal/privacy`). |
| Default port | 9090. | 8080, matching the OpenAPI `servers` URL. |
| Configuration | Mixed environment and secret-store loading. | Environment variables only, documented in [configuration.md](configuration.md). |
| CLI login | Browser device flow and a legacy config file. | `gpu login` stores an API key in `~/.config/gpu/credentials.json`. No network call. |
| SDK and MCP packaging | Published packages, release and version-injection scripts, live smoke tests. | Built from the checkout, marked private, never published. |
| MCP tools | 44 tools, including billing, usage, deploys, models, fine-tuning, chat, webhooks, watch rules, autolaunch and market history, plus a session spend budget. | The 15 tools that map to the open-source API. |
| OpenAPI spec | The full hosted API. | The 12 paths in `openapi/v1.yaml`, with descriptions that referred to billing, templates, dashboards or hosted URLs rewritten. |
