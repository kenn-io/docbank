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

.PHONY: build install clean test test-v fmt lint lint-ci tidy install-hooks docs-install docs-build docs-serve docs-vercel-cli docs-link docs-deploy help

build:
	CGO_ENABLED=1 go build -tags "$(BUILD_TAGS)" -ldflags="$(LDFLAGS)" -o docbank ./cmd/docbank

install:
	@mkdir -p "$(HOME)/.local/bin"
	CGO_ENABLED=1 go build -tags "$(BUILD_TAGS)" -ldflags="$(LDFLAGS)" -o "$(HOME)/.local/bin/docbank" ./cmd/docbank

clean:
	rm -f docbank

test:
	go test -tags "$(BUILD_TAGS)" ./...

test-v:
	go test -tags "$(BUILD_TAGS)" -v ./...

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

docs-build:
	cd docs && ./zensical-docs.sh build

docs-serve:
	cd docs && ./zensical-docs.sh serve

# Deploys run the Vercel CLI version pinned in docs/.vercel-cli/package-lock.json,
# never a version resolved at deploy time. The npm tree lives in a dot-directory
# so Go's ./... walk and the docs publishing boundary both skip it.
docs-vercel-cli:
	cd docs/.vercel-cli && npm ci --no-audit --no-fund

docs-link: docs-vercel-cli
	cd docs && ./.vercel-cli/node_modules/.bin/vercel link

docs-deploy: docs-build docs-vercel-cli
	@if [ ! -f docs/.vercel/project.json ]; then \
		echo "docs are not linked to a Vercel project yet." >&2; \
		echo "Run: docs/.vercel-cli/node_modules/.bin/vercel login && make docs-link" >&2; \
		exit 1; \
	fi
	cp -R docs/.vercel docs/site/.vercel
	docs/.vercel-cli/node_modules/.bin/vercel deploy docs/site --prod --yes

help:
	@echo "Targets: build install clean test test-v fmt lint lint-ci tidy install-hooks docs-install docs-build docs-serve docs-link docs-deploy"
