#!/bin/sh
set -eu

script_dir=$(CDPATH='' cd -- "$(dirname -- "$0")" && pwd)
repo_root=$(CDPATH='' cd -- "$script_dir/.." && pwd)
if ! command -v vercel >/dev/null 2>&1; then
  printf 'vercel CLI not found; install Vercel CLI 58.4.4 or later\n' >&2
  exit 127
fi

cd "$repo_root"
if [ ! -f .vercel/project.json ] && { [ -z "${VERCEL_ORG_ID:-}" ] || [ -z "${VERCEL_PROJECT_ID:-}" ]; }; then
  printf 'documentation project is not linked; run make docs-link or provide Vercel project IDs\n' >&2
  exit 1
fi

upload_report=$(mktemp)
trap 'rm -f -- "$upload_report"' EXIT
trap 'exit 129' HUP
trap 'exit 130' INT
trap 'exit 143' TERM
vercel deploy --prod --skip-domain --yes --dry --json > "$upload_report"
node scripts/docs/assert-vercel-dry-run.mjs "$upload_report"

vercel deploy --prod --yes
