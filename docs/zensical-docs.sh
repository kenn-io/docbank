#!/usr/bin/env bash
set -euo pipefail

command_name="${1:-}"
if [[ "$command_name" != "build" && "$command_name" != "serve" ]]; then
  printf 'usage: %s {build|serve} [zensical args...]\n' "$0" >&2
  exit 2
fi
shift || true

script_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
docs_root="$script_dir"
site_dir="${DOCBANK_DOCS_SITE_DIR:-site}"

if ! command -v uv >/dev/null 2>&1; then
  printf 'uv not found; install it from https://docs.astral.sh/uv/\n' >&2
  exit 127
fi
uv_run=(uv run --project "$docs_root" --frozen --no-dev)

"${uv_run[@]}" python "$docs_root/scripts/check_markdown_sources.py"

tmp_docs=""
tmp_config_base=""
tmp_config=""

cleanup() {
  if [[ -n "$tmp_docs" ]]; then
    rm -rf "$tmp_docs"
  fi
  if [[ -n "$tmp_config" ]]; then
    rm -f "$tmp_config"
  fi
  if [[ -n "$tmp_config_base" ]]; then
    rm -f "$tmp_config_base"
  fi
}
trap cleanup EXIT INT TERM

tmp_docs_name="$(cd "$docs_root" && mktemp -d zensical-public-docs.XXXXXX)"
tmp_docs="$docs_root/$tmp_docs_name"
tmp_config_base_name="$(cd "$docs_root" && mktemp .zensical-build.XXXXXX)"
tmp_config_base="$docs_root/$tmp_config_base_name"
tmp_config="$tmp_config_base.toml"
tmp_config_name="$tmp_config_base_name.toml"
if [[ -e "$tmp_config" ]]; then
  printf 'temporary config path already exists: %s\n' "$tmp_config" >&2
  exit 1
fi
mv "$tmp_config_base" "$tmp_config"
tmp_config_base=""

# The temporary docs tree is the publishing boundary: internal material
# (superpowers specs/plans, tooling, local env) never reaches the site.
(
  cd "$docs_root"
  tar \
    --exclude './.cache' \
    --exclude './.venv' \
    --exclude './.env' \
    --exclude './.env.*' \
    --exclude './internal' \
    --exclude './scripts' \
    --exclude './site' \
    --exclude './zensical-public-docs.*' \
    --exclude './.zensical-build.*' \
    --exclude './superpowers' \
    --exclude './README.md' \
    --exclude './pyproject.toml' \
    --exclude './uv.lock' \
    --exclude './zensical-docs.sh' \
    --exclude './zensical.toml' \
    -cf - .
) | (cd "$tmp_docs" && tar -xf -)

find "$tmp_docs" -depth -name '.*' -exec rm -rf {} +

# tar preserves symlinks, and a symlink could smuggle excluded or
# out-of-tree content (.env, superpowers plans) past the publishing
# boundary when zensical follows it.
symlinks="$(find "$tmp_docs" -type l)"
if [[ -n "$symlinks" ]]; then
  printf 'refusing to publish: symlinks in docs tree:\n%s\n' "$symlinks" >&2
  exit 1
fi

awk -v docs_dir="$tmp_docs_name" -v site_dir="$site_dir" '
  $0 == "docs_dir = \"docs\"" {
    print "docs_dir = \"" docs_dir "\""
    next
  }
  $0 == "site_dir = \"site\"" {
    print "site_dir = \"" site_dir "\""
    next
  }
  { print }
' "$docs_root/zensical.toml" > "$tmp_config"

case "$command_name" in
  build)
    (cd "$docs_root" && "${uv_run[@]}" zensical build --strict --config-file "$tmp_config_name" "$@")
    "${uv_run[@]}" python "$docs_root/scripts/check_built_site.py" "$docs_root/$site_dir"
    ;;
  serve)
    (cd "$docs_root" && "${uv_run[@]}" zensical serve --config-file "$tmp_config_name" "$@")
    ;;
esac
