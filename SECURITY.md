# Security policy

## Reporting a vulnerability

Email **support@gpu.ai** with a description of the issue, the affected
version or commit, and steps to reproduce. Please do not open a public issue
or pull request for a vulnerability.

We will acknowledge your report, keep you informed while we investigate, and
credit you in the fix unless you ask us not to. Please give us reasonable
time to release a fix before you disclose the issue.

## Scope

In scope:

- `gpuctl` and its `/v1` API, including authentication, scopes and the
  handling of API keys, SSH keys and registry credentials.
- The provisioning engine and the supplier adapters in this repository.
- The `gpu` CLI, the SDKs and the MCP server.
- The deployment files under `deploy/`.

Out of scope:

- Issues in a supplier's own API or infrastructure. Report those to the
  supplier.
- `examples/mock-supplier`, which is a local test fixture with access to
  the Docker socket and is not meant to be exposed to a network.
- Deployments that ignore [docs/self-hosting.md](docs/self-hosting.md), for
  example gpuctl exposed without TLS or with default database passwords.

## Supported versions

Security fixes are made on the `main` branch.
