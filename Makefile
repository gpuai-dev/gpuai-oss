# GPU.ai Open Source developer tasks. Run `make help` for the list.

VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS := -s -w -X main.version=$(VERSION)
PYTHON  ?= python3

.PHONY: help build test test-go test-sdk test-mcp lint e2e sdk-generate clean

help:
	@echo "make build         build bin/gpuctl, bin/gpu and bin/mock-supplier"
	@echo "make test          Go, SDK and MCP tests"
	@echo "make test-go       Go tests (set DATABASE_TEST_URL and REDIS_TEST_URL for the database tests)"
	@echo "make test-sdk      Python and TypeScript SDK tests"
	@echo "make test-mcp      MCP server tests, typecheck and build"
	@echo "make lint          gofmt, go vet and the forbidden-strings check"
	@echo "make e2e           docker compose quickstart: provision and terminate via the mock supplier"
	@echo "make sdk-generate  regenerate sdk/python and sdk/typescript from openapi/v1.yaml"

build:
	go build -trimpath -ldflags "$(LDFLAGS)" -o bin/gpuctl ./cmd/gpuctl
	go build -trimpath -ldflags "$(LDFLAGS)" -o bin/gpu ./cmd/gpu
	go build -trimpath -o bin/mock-supplier ./examples/mock-supplier

test: test-go test-sdk test-mcp

test-go:
	go test ./...

test-sdk:
	cd sdk/python && $(PYTHON) -m pip install -q -r requirements.txt pytest && $(PYTHON) -m pip install -q . && $(PYTHON) -m pytest -q
	cd sdk/typescript && npm ci --no-audit --no-fund && npm test

test-mcp:
	cd mcp && npm ci --no-audit --no-fund && npm test && npm run typecheck && npm run build

lint:
	@out=$$(gofmt -l cmd internal examples openapi); if [ -n "$$out" ]; then echo "gofmt needed:"; echo "$$out"; exit 1; fi
	go vet ./...
	scripts/check-forbidden.sh

e2e:
	scripts/e2e.sh

sdk-generate:
	sdk/generate.sh

clean:
	rm -rf bin
