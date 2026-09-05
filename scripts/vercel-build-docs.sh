#!/bin/sh
set -eu

script_dir=$(CDPATH='' cd -- "$(dirname -- "$0")" && pwd)
repo_root=$(CDPATH='' cd -- "$script_dir/.." && pwd)
export PATH="$repo_root/.vercel-tools/bin:$PATH"
export DOCBANK_REPO_ROOT="$repo_root"

if [ ! -x "$repo_root/.vercel-tools/bin/uv" ]; then
  printf 'pinned uv is missing; the Vercel install step did not complete\n' >&2
  exit 1
fi

cd "$repo_root"
node scripts/docs/build.mjs
