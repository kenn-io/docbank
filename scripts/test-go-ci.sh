#!/usr/bin/env bash
set -euo pipefail

packages=$(go list -tags fts5 ./... | grep -vxF go.kenn.io/docbank/internal/store)

# Storage dominates CI. Split it without adding runners or changing tests.
# ponytail: two name ranges balance the current suite; remeasure if it grows unevenly.
go test -timeout 20m -tags fts5 "$@" -run '^(Test|Example|Fuzz)[A-L]' ./internal/store &
first=$!
go test -timeout 20m -tags fts5 "$@" -run '^(Test|Example|Fuzz)($|[^A-L])' ./internal/store &
second=$!

status=0
# Go import paths cannot contain whitespace; split this package list into arguments.
# shellcheck disable=SC2086
go test -timeout 20m -tags fts5 "$@" $packages || status=$?
wait "$first" || status=$?
wait "$second" || status=$?
exit "$status"
