#!/usr/bin/env bash
set -euo pipefail

script_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
repo_root="$(cd "$script_dir/.." && pwd)"
remote="${DOCBANK_DOCS_ASSETS_REMOTE:-https://github.com/kenn-io/docbank.git}"
manifest="${DOCBANK_DOCS_ASSETS_MANIFEST:-$script_dir/docs-assets.txt}"
ref_file="${DOCBANK_DOCS_ASSETS_REF:-$script_dir/docs-assets.ref}"
cache_root="${DOCBANK_DOCS_ASSETS_CACHE:-$repo_root/.cache/docs-assets}"
max_png_bytes=$((10 * 1024 * 1024))

if command -v sha256sum >/dev/null 2>&1; then
  checksum() { sha256sum "$@"; }
  checksum_check() { sha256sum -c "$@"; }
elif command -v shasum >/dev/null 2>&1; then
  checksum() { shasum -a 256 "$@"; }
  checksum_check() { shasum -a 256 -c "$@"; }
else
  printf 'SHA-256 checksum tool not found\n' >&2
  exit 127
fi

if [[ ! -f "$manifest" ]]; then
  printf 'docs asset manifest is missing: %s\n' "$manifest" >&2
  exit 1
fi
if [[ ! -f "$ref_file" ]]; then
  printf 'docs asset ref is missing: %s\n' "$ref_file" >&2
  exit 1
fi

asset_ref="$(tr -d '\r\n' < "$ref_file")"
if [[ ! "$asset_ref" =~ ^[0-9a-f]{40}$ ]]; then
  printf 'docs asset ref must be one full lowercase commit SHA\n' >&2
  exit 1
fi
if [[ ! -s "$manifest" ]] || grep -q '^$' "$manifest"; then
  printf 'docs asset manifest must contain no blank entries\n' >&2
  exit 1
fi
if ! LC_ALL=C sort -c "$manifest" 2>/dev/null; then
  printf 'docs asset manifest must be sorted\n' >&2
  exit 1
fi
if [[ -n "$(LC_ALL=C sort "$manifest" | uniq -d)" ]]; then
  printf 'docs asset manifest contains duplicate entries\n' >&2
  exit 1
fi
while IFS= read -r name; do
  if [[ ! "$name" =~ ^[A-Za-z0-9][A-Za-z0-9._-]*\.png$ ]]; then
    printf 'invalid docs asset manifest entry: %s\n' "$name" >&2
    exit 1
  fi
done < "$manifest"

destination="$cache_root/$asset_ref"

cache_is_valid() {
  [[ -d "$destination" && -f "$destination/.sha256" ]] || return 1
  local actual_files recorded_files
  actual_files="$(
    find "$destination" -maxdepth 1 -type f ! -name '.sha256' -exec basename {} \; |
      LC_ALL=C sort
  )"
  recorded_files="$(awk '{name=$2; sub(/^\*/, "", name); print name}' "$destination/.sha256")"
  [[ "$actual_files" == "$(<"$manifest")" ]] || return 1
  [[ "$recorded_files" == "$(<"$manifest")" ]] || return 1
  (cd "$destination" && checksum_check .sha256 >/dev/null 2>&1)
}

if cache_is_valid; then
  printf 'docs assets ready at %s\n' "$destination"
  exit 0
fi

mkdir -p "$cache_root"
scratch="$(mktemp -d)"
staging="$cache_root/.${asset_ref}.next.$$"
previous="$cache_root/.${asset_ref}.previous"
cleanup() {
  rm -rf -- "$scratch"
  if [[ -n "${staging:-}" ]]; then
    rm -rf -- "$staging"
  fi
}
trap cleanup EXIT INT TERM

export GIT_CONFIG_GLOBAL="$scratch/gitconfig"
export GIT_CONFIG_NOSYSTEM=1
unset GIT_DIR GIT_WORK_TREE GIT_INDEX_FILE GIT_OBJECT_DIRECTORY
unset GIT_ALTERNATE_OBJECT_DIRECTORIES GIT_COMMON_DIR GIT_NAMESPACE

git_repo="$scratch/repository.git"
git init --bare --quiet "$git_repo"
git -C "$git_repo" fetch --quiet --depth=1 "$remote" "$asset_ref"
fetched="$(git -C "$git_repo" rev-parse 'FETCH_HEAD^{commit}')"
if [[ "$fetched" != "$asset_ref" ]]; then
  printf 'fetched docs asset commit differs from pinned ref\n' >&2
  exit 1
fi
if [[ -n "$(git -C "$git_repo" show -s --format='%P' "$asset_ref")" ]]; then
  printf 'docs asset commit must be an orphan commit\n' >&2
  exit 1
fi

tree_files="$scratch/tree-files"
git -C "$git_repo" ls-tree -r --name-only "$asset_ref" | LC_ALL=C sort > "$tree_files"
if ! diff -u "$manifest" "$tree_files" >/dev/null; then
  printf 'docs asset commit tree differs from manifest\n' >&2
  diff -u "$manifest" "$tree_files" >&2 || true
  exit 1
fi
if git -C "$git_repo" ls-tree -r "$asset_ref" | awk '$1 != "100644" { found=1 } END { exit !found }'; then
  printf 'docs asset commit contains unsupported entry modes\n' >&2
  exit 1
fi

mkdir "$staging"
while IFS= read -r name; do
  git -C "$git_repo" show "$asset_ref:$name" > "$staging/$name"
  size="$(wc -c < "$staging/$name" | tr -d ' ')"
  if (( size > max_png_bytes )); then
    printf 'oversized docs asset: %s\n' "$name" >&2
    exit 1
  fi
  signature="$(od -An -tx1 -N8 "$staging/$name" | tr -d ' \n')"
  if [[ "$signature" != "89504e470d0a1a0a" ]]; then
    printf 'invalid PNG signature: %s\n' "$name" >&2
    exit 1
  fi
  (cd "$staging" && checksum "$name") >> "$staging/.sha256"
done < "$manifest"
(cd "$staging" && checksum_check .sha256 >/dev/null)

rm -rf -- "$previous"
had_destination=false
if [[ -e "$destination" ]]; then
  mv "$destination" "$previous"
  had_destination=true
fi
if ! mv "$staging" "$destination"; then
  if [[ "$had_destination" == true ]]; then
    mv "$previous" "$destination"
  fi
  exit 1
fi
staging=""
rm -rf -- "$previous"
printf 'docs assets ready at %s\n' "$destination"
