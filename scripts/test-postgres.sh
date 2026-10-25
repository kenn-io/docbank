#!/usr/bin/env bash
set -euo pipefail

# Own a fresh, disposable cluster. Never discover or reuse an operator database.
repo_root=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd)
image=${DOCBANK_TEST_POSTGRES_IMAGE:-postgres:16}
container=$(docker run --detach --rm \
  --publish 127.0.0.1::5432 \
  --env POSTGRES_PASSWORD=docbank-test-only \
  "$image")
trap 'docker rm --force "$container" >/dev/null' EXIT
ready=false
for ((attempt=0; attempt<60; attempt++)); do
  if docker exec "$container" pg_isready -U postgres >/dev/null 2>&1; then
    ready=true
    break
  fi
  sleep 1
done
if [[ $ready != true ]]; then
  echo 'Disposable PostgreSQL did not become ready' >&2
  exit 1
fi
port=$(docker inspect --format '{{(index (index .NetworkSettings.Ports "5432/tcp") 0).HostPort}}' "$container")
export DOCBANK_TEST_POSTGRES_DSN="postgres://postgres:docbank-test-only@127.0.0.1:$port/postgres?sslmode=disable"
cd "$repo_root"
go test -tags fts5,postgres -race -timeout 3m ./internal/store/pgschema "$@"
