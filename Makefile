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

.PHONY: build install clean test test-v release-scripts-test fmt lint lint-ci tidy install-hooks docs-install docs-build docs-serve docs-link docs-deploy help

build:
	CGO_ENABLED=1 go build -tags "$(BUILD_TAGS)" -ldflags="$(LDFLAGS)" -o docbank ./cmd/docbank

install:
	@mkdir -p "$(HOME)/.local/bin"
	CGO_ENABLED=1 go build -tags "$(BUILD_TAGS)" -ldflags="$(LDFLAGS)" -o "$(HOME)/.local/bin/docbank" ./cmd/docbank

clean:
	rm -f docbank

test: release-scripts-test
	go test -tags "$(BUILD_TAGS)" ./...

test-v:
	go test -tags "$(BUILD_TAGS)" -v ./...

release-scripts-test:
	bash scripts/release_scripts_test.sh

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

# Deploys use the operator's installed Vercel CLI; install it with
# `npm install -g vercel` or from https://vercel.com/docs/cli.
docs-link:
	@if ! command -v vercel >/dev/null 2>&1; then \
		echo "vercel CLI not found. Install: https://vercel.com/docs/cli" >&2; \
		exit 1; \
	fi
	cd docs && vercel link

docs-deploy: docs-build
	@if ! command -v vercel >/dev/null 2>&1; then \
		echo "vercel CLI not found. Install: https://vercel.com/docs/cli" >&2; \
		exit 1; \
	fi
	@if [ ! -f docs/.vercel/project.json ]; then \
		echo "docs are not linked to a Vercel project yet." >&2; \
		echo "Run: vercel login && make docs-link" >&2; \
		exit 1; \
	fi
	cp -R docs/.vercel docs/site/.vercel
	vercel deploy docs/site --prod --yes

help:
	@echo "Targets: build install clean test test-v release-scripts-test fmt lint lint-ci tidy install-hooks docs-install docs-build docs-serve docs-link docs-deploy"
