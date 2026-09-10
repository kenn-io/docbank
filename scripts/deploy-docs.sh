#!/bin/sh
set -eu

script_dir=$(CDPATH='' cd -- "$(dirname -- "$0")" && pwd)
repo_root=$(CDPATH='' cd -- "$script_dir/.." && pwd)
source_sha=${DOCS_SOURCE:-}

if [ -z "$source_sha" ]; then
  printf 'DOCS_SOURCE is required; use a full source commit SHA\n' >&2
  exit 2
fi
if ! command -v vercel >/dev/null 2>&1; then
  printf 'vercel CLI not found; install Vercel CLI 58.4.4 or later\n' >&2
  exit 127
fi

cd "$repo_root"
if [ -n "$(git status --porcelain)" ]; then
  printf 'refusing documentation deploy from a dirty worktree\n' >&2
  exit 1
fi
head_sha=$(git rev-parse HEAD)
if [ "$head_sha" != "$source_sha" ]; then
  printf 'DOCS_SOURCE must equal HEAD: expected %s, got %s\n' "$head_sha" "$source_sha" >&2
  exit 1
fi
if [ ! -f .vercel/project.json ] && { [ -z "${VERCEL_ORG_ID:-}" ] || [ -z "${VERCEL_PROJECT_ID:-}" ]; }; then
  printf 'documentation project is not linked; run make docs-link or provide Vercel project IDs\n' >&2
  exit 1
fi

git fetch --quiet origin refs/heads/main:refs/remotes/origin/main --tags
expected_tag=$(git describe --tags --abbrev=0 --match 'v[0-9]*.[0-9]*.[0-9]*' origin/main)
./scripts/validate-docs-release.sh "$source_sha" "$expected_tag"

deployment_url=$(vercel deploy --prod --skip-domain --yes)
case "$deployment_url" in
  https://*.vercel.app) ;;
  *)
    printf 'Vercel did not return a deployment URL: %s\n' "$deployment_url" >&2
    exit 1
    ;;
esac

vercel inspect "$deployment_url" --wait --timeout 10m
./scripts/validate-docs-release.sh "$source_sha" "$expected_tag"
vercel promote "$deployment_url" --yes
