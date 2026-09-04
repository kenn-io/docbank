# Makefile for docbank

.DEFAULT_GOAL := help

# Tag names are attacker-controlled in CI and VERSION is interpolated into
# the shell command line of build/install: strip anything outside a strict
# allowlist rather than trusting git metadata.
VERSION := $(shell (git describe --tags --always --dirty 2>/dev/null || echo dev) | tr -cd 'A-Za-z0-9._+-')
ifeq ($(VERSION),)
VERSION := dev
endif
COMMIT := $(shell git rev-parse --short HEAD 2>/dev/null || echo "unknown")

LDFLAGS := -X go.kenn.io/docbank/internal/version.Version=$(VERSION) \
           -X go.kenn.io/docbank/internal/version.Commit=$(COMMIT)

# fts5: enable the SQLite FTS5 full-text search extension
BUILD_TAGS := fts5

DEFAULT_GOLANGCI_LINT_CACHE := $(shell git rev-parse --path-format=absolute --git-path golangci-lint-cache)
GOLANGCI_LINT_CACHE ?= $(DEFAULT_GOLANGCI_LINT_CACHE)
export GOLANGCI_LINT_CACHE

.PHONY: build install clean test test-v release-scripts-test frontend frontend-test frontend-dev docs-screenshots fmt lint lint-ci tidy install-hooks docs-install docs-subpath-test docs-assets-test docs-assets-sync docs-build docs-serve docs-link docs-deploy help

build: frontend
	CGO_ENABLED=1 go build -tags "$(BUILD_TAGS)" -ldflags="$(LDFLAGS)" -o docbank ./cmd/docbank

install: frontend
	@mkdir -p "$(HOME)/.local/bin"
	CGO_ENABLED=1 go build -tags "$(BUILD_TAGS)" -ldflags="$(LDFLAGS)" -o "$(HOME)/.local/bin/docbank" ./cmd/docbank

clean:
	rm -f docbank
	find internal/web/dist -mindepth 1 ! -name .keep -exec rm -rf {} +
	rm -rf frontend/dist

test: release-scripts-test
	go test -tags "$(BUILD_TAGS)" ./...

test-v:
	go test -tags "$(BUILD_TAGS)" -v ./...

release-scripts-test:
	bash scripts/release_scripts_test.sh

frontend:
	cd frontend && npm ci && npm run build
	find internal/web/dist -mindepth 1 ! -name .keep -exec rm -rf {} +
	cp -R frontend/dist/. internal/web/dist/

frontend-test:
	cd frontend && npm ci
	cd frontend && npm run check
	cd frontend && npm run check:kit-ui
	cd frontend && npm run screenshots:check
	cd frontend && npm test
	cd frontend && npm run build

frontend-dev:
	cd frontend && npm run dev

docs-screenshots:
	cd frontend && npm run screenshots

fmt:
	go fmt ./...

lint:
	@if ! command -v golangci-lint >/dev/null 2>&1; then \
		echo "golangci-lint not found. Install: https://golangci-lint.run/usage/install/" >&2; \
		exit 1; \
	fi
	golangci-lint run --fix ./...

lint-ci:
	@if ! command -v golangci-lint >/dev/null 2>&1; then \
		echo "golangci-lint not found. Install: https://golangci-lint.run/usage/install/" >&2; \
		exit 1; \
	fi
	golangci-lint run ./...

tidy:
	go mod tidy

install-hooks:
	@if ! command -v prek >/dev/null 2>&1; then \
		echo "prek not found. Install with: brew install prek" >&2; \
		exit 1; \
	fi
	prek install

docs-install:
	cd docs && uv sync --frozen

docs-subpath-test:
	cd docs && uv run --project . --frozen --no-dev python scripts/check_zensical_subpath.py

bridge-contract:
	go test -tags fts5 ./document/bridge -run '^TestBridgeContractNormativeDocuments'

docs-assets-test:
	bash scripts/docs-assets-sync.test.sh

docs-assets-sync:
	./scripts/sync-docs-assets.sh

docs-build: bridge-contract docs-subpath-test
	node scripts/docs/build.mjs

docs-serve:
	node scripts/docs/serve.mjs

# Deploys use the operator's installed Vercel CLI; install it with
# `npm install -g vercel` or from https://vercel.com/docs/cli.
docs-link:
	@if ! command -v vercel >/dev/null 2>&1; then \
		echo "vercel CLI not found. Install: https://vercel.com/docs/cli" >&2; \
		exit 1; \
	fi
	vercel link
	@test -f .vercel/project.json || { \
		echo "Vercel did not create .vercel/project.json at the repository root." >&2; \
		exit 1; \
	}

docs-deploy:
	@if [ -z "$(DOCS_SOURCE)" ]; then \
		echo "DOCS_SOURCE is required and must be a full source commit SHA." >&2; \
		echo "Run: make docs-deploy DOCS_SOURCE=$$(git rev-parse HEAD)" >&2; \
		exit 1; \
	fi
	DOCS_SOURCE="$(DOCS_SOURCE)" ./scripts/deploy-docs.sh

help:
	@echo "Targets: build install clean test test-v release-scripts-test frontend frontend-test frontend-dev docs-screenshots fmt lint lint-ci tidy install-hooks docs-install docs-subpath-test docs-assets-test docs-assets-sync docs-build docs-serve docs-link docs-deploy"
