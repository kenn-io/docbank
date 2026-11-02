# .custom-gcl.yml owns the golangci-lint version so Renovate updates one place.
GOLANGCI_LINT_VERSION ?= $(shell sed -n 's/^version: //p' .custom-gcl.yml)
CUSTOM_GCL_DIR ?= .
CUSTOM_GCL := $(CUSTOM_GCL_DIR)/custom-gcl

# Compile without absolute source paths so every checkout and worktree shares
# one Go build cache instead of each compiling its own copy of every package.
# Read `go env` first so flags saved with `go env -w GOFLAGS=...` survive the
# export. GOFLAGS=-trimpath=false opts out. Debuggers need a source mapping:
# docs/development.md#debugging-trimmed-builds.
ifeq ($(origin GOFLAGS),undefined)
GOFLAGS := $(shell go env GOFLAGS)
endif
ifeq ($(filter -trimpath -trimpath=%,$(GOFLAGS)),)
override GOFLAGS += -trimpath
endif
export GOFLAGS

.PHONY: lint lint-golangci lint-config lint-config-check custom-gcl nilaway lint-workflow-retries

# lint is the repository lint gate: shared config drift, golangci-lint with the
# kit analyzers, and nilaway.
lint: lint-config-check lint-golangci nilaway lint-workflow-retries

# custom-gcl builds golangci-lint with the plugins in .custom-gcl.yml
# (the kit analyzers from ./lint/gclplugin and nilaway). golangci-lint keeps
# the build cached and skips it when nothing changed.
custom-gcl:
	.github/actions/kit-retry/kit-retry 3 5 go run github.com/golangci/golangci-lint/v2/cmd/golangci-lint@$(GOLANGCI_LINT_VERSION) custom \
		--destination $(CUSTOM_GCL_DIR) --name custom-gcl --version $(GOLANGCI_LINT_VERSION)

lint-golangci: custom-gcl
	$(CUSTOM_GCL) run ./...

nilaway: custom-gcl
	$(CUSTOM_GCL) run --config .golangci.nilaway.yml ./...

# lint-config regenerates .golangci.yml from the canonical config in
# lint/config plus the repository overlay in .golangci.overlay.yml.
lint-config:
	go run ./cmd/kennlint config

lint-config-check:
	go run ./cmd/kennlint config -check

.PHONY: test-embedserver
# Set KIT_EMBED_TEST_DIR and KIT_EMBED_TEST_LIBRARY to include native parity.
test-embedserver:
	go test -race ./embedserver ./cmd/kennembed ./internal/embedassets ./embedlocal

lint-workflow-retries:
	go run ./cmd/kennlint workflow-retries

# Workflow actions run at the commits in .github/workflows/actions.lock,
# including actions that other actions call. The organization requires
# SHA-pinned actions, and the lockfile is what satisfies that for nested refs.
# The version comes from constraints.ghActionsLock in renovate.json5.
# Renovate uses the same version when it regenerates the lockfile after
# bumping an action.
GH_ACTIONS_LOCK_VERSION ?= $(shell sed -nE "s/^[[:space:]]*\"?ghActionsLock\"?:[[:space:]]*[\"']([^\"']*)[\"'].*/\1/p" renovate.json5)

.PHONY: gh-actions-lock-install actions-lock actions-lock-check-local actions-lock-check

gh-actions-lock-install:
	@test -n "$(GH_ACTIONS_LOCK_VERSION)" || { echo "constraints.ghActionsLock is missing from renovate.json5" >&2; exit 1; }
	@if [ "$$(gh actions-lock --version 2>/dev/null)" != "actions-lock version $(GH_ACTIONS_LOCK_VERSION)" ]; then \
		gh extension remove actions-lock >/dev/null 2>&1 || true; \
		gh extension install github/gh-actions-lock --pin $(GH_ACTIONS_LOCK_VERSION); \
	fi

# Lock every workflow action after adding or bumping a `uses:` line
actions-lock: gh-actions-lock-install
	gh actions-lock --no-interactive

# Offline check that every workflow action has a lockfile entry
actions-lock-check-local: gh-actions-lock-install
	gh actions-lock --verify-local

# Network check that the lockfile is complete and every locked commit is still valid
actions-lock-check: gh-actions-lock-install
	gh actions-lock --verify

.PHONY: test-embedlocal
test-embedlocal:
	go test -race ./embedlocal
