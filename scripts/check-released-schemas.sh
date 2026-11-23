#!/usr/bin/env bash
# Check release tags with the storage schema catalog test. Requires full history.
set -euo pipefail
DOCBANK_RELEASE_SCHEMAS=1 go test -tags fts5 ./internal/store -run '^TestReleaseTagSchemas$' -count=1
