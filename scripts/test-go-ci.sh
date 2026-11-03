#!/usr/bin/env bash
set -euo pipefail

# Name the results after the platform and SQLite build so every CI job
# writes a distinct gotestsum report.
cgo=purego
if [ "$(go env CGO_ENABLED)" = 1 ]; then
	cgo=cgo
fi
suite="$(go env GOOS)-$(go env GOARCH)-$cgo"
mkdir -p gotestsum
go tool gotestsum --format pkgname-and-test-fails --format-icons text \
	--jsonfile "gotestsum/$suite.json" \
	--rerun-fails=2 --rerun-fails-report "gotestsum/$suite-reruns.txt" \
	--packages ./... -- -timeout 20m -tags fts5 "$@"
