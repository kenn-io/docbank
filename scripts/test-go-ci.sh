#!/usr/bin/env bash
set -euo pipefail

go test -v -timeout 20m -tags fts5 "$@" ./...
