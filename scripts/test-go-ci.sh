#!/usr/bin/env bash
set -euo pipefail

go test -timeout 20m -tags fts5 "$@" ./...
