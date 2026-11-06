# Contributing

Thanks for helping with GPU.ai Open Source. This file covers how to build,
test and send changes.

## Before you start

Open an issue before a large change, such as a new adapter or a new API
field, so the approach can be agreed first. Small fixes can go straight to a
pull request.

The `/v1` API keeps the same paths and JSON shapes as GPU.ai's hosted API.
Changes to `openapi/v1.yaml` must stay compatible with existing clients:
add fields, do not rename or remove them.

## Setup

You need Go 1.25 or newer, Docker with Compose, Node 22, Python 3.10 or newer,
and (only to regenerate the SDKs) Java 11 or newer.

```sh
make build    # bin/gpuctl, bin/gpu, bin/mock-supplier
make test     # Go, SDK and MCP tests
make lint     # gofmt, go vet, forbidden-strings check
make e2e      # quickstart end to end with docker compose
```

The Go database and Redis tests skip unless you point them at disposable
instances. Migrate the database first:

```sh
docker run -d --name gpuai-test-pg -e POSTGRES_USER=gpuai -e POSTGRES_PASSWORD=test \
  -e POSTGRES_DB=gpuai_test -p 127.0.0.1:55432:5432 postgres:16-alpine
docker run -d --name gpuai-test-redis -p 127.0.0.1:56379:6379 redis:7-alpine

export DATABASE_TEST_URL='postgres://gpuai:test@127.0.0.1:55432/gpuai_test?sslmode=disable'
export REDIS_TEST_URL='redis://127.0.0.1:56379/0'
DATABASE_URL=$DATABASE_TEST_URL REDIS_URL=$REDIS_TEST_URL go run ./cmd/gpuctl migrate
go test ./...
```

Never point these variables at a database you care about. The tests write
to it.

## Changing the API spec

`openapi/v1.yaml` is the single source. The server embeds it, and the SDKs
are generated from it. After editing it:

```sh
make sdk-generate   # regenerates sdk/python and sdk/typescript
make test
```

Commit the regenerated SDKs with the spec change.

## Writing an adapter

See [docs/suppliers.md](docs/suppliers.md#writing-a-new-adapter). Prefer
implementing the Generic Supplier API on the supplier side when you can, so
no Go code is needed.

## Rules for every change

- No secrets, credentials or real customer data, including in tests. Use
  fake values, `example.com` hosts and documentation IP ranges such as
  `203.0.113.0/24`.
- Supplier identity never reaches API responses or customer-facing error
  text. Log it for operators instead.
- `scripts/check-forbidden.sh` must pass. CI runs it together with gitleaks
  over the full history.
- Run `gofmt`. Keep tests offline: anything that calls a real supplier API
  must be opt-in through an environment variable or build tag.
- Add or update tests for behaviour you change, and update the docs under
  `docs/` when configuration or behaviour changes.

## Pull requests

- Keep each pull request to one change, with a description of what changed
  and how you tested it.
- CI (`.github/workflows/ci.yml`) must pass: Go build, vet and tests with
  Postgres and Redis, SDK tests, MCP tests and build, the secret scan and the
  end-to-end quickstart.
- By contributing you agree that your contribution is licensed under the
  Apache License 2.0, the license of this repository.

## Conduct

This project follows the [Code of Conduct](CODE_OF_CONDUCT.md).
