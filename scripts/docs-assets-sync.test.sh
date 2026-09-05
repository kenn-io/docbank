#!/usr/bin/env bash
set -euo pipefail

script_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
sync_script="$script_dir/sync-docs-assets.sh"
test_root="$(mktemp -d)"
trap 'rm -rf -- "$test_root"' EXIT INT TERM

export GIT_CONFIG_GLOBAL="$test_root/gitconfig"
export GIT_CONFIG_NOSYSTEM=1
unset GIT_DIR GIT_WORK_TREE GIT_INDEX_FILE GIT_OBJECT_DIRECTORY
unset GIT_ALTERNATE_OBJECT_DIRECTORIES GIT_COMMON_DIR GIT_NAMESPACE

manifest="$test_root/docs-assets.txt"
ref_file="$test_root/docs-assets.ref"
cache="$test_root/cache"
remote="$test_root/remote.git"
fixture="$test_root/fixture"
if command -v sha256sum >/dev/null 2>&1; then
  checksum() { sha256sum "$@"; }
else
  checksum() { shasum -a 256 "$@"; }
fi

fail() {
  printf 'docs asset sync test failed: %s\n' "$1" >&2
  exit 1
}

write_png() {
  local destination="$1"
  mkdir -p "$(dirname "$destination")"
  printf '\211PNG\r\n\032\n' > "$destination"
  printf 'synthetic png payload\n' >> "$destination"
}

create_remote() {
  local mode="$1"
  rm -rf -- "$remote" "$fixture"
  git init --bare --quiet "$remote"
  git init --quiet "$fixture"
  git -C "$fixture" config user.name "Docbank Test"
  git -C "$fixture" config user.email "docbank-test@example.invalid"
  case "$mode" in
    valid)
      write_png "$fixture/one.png"
      ;;
    extra)
      write_png "$fixture/one.png"
      write_png "$fixture/extra.png"
      ;;
    malformed)
      printf 'not a png\n' > "$fixture/one.png"
      ;;
    oversized)
      write_png "$fixture/one.png"
      dd if=/dev/zero bs=1048576 count=11 >> "$fixture/one.png" 2>/dev/null
      ;;
    nested)
      write_png "$fixture/one.png"
      write_png "$fixture/nested/two.png"
      ;;
    *)
      fail "unknown fixture mode $mode"
      ;;
  esac
  git -C "$fixture" add .
  git -C "$fixture" commit --quiet -m "docs assets fixture"
  fixture_commit="$(git -C "$fixture" rev-parse HEAD)"
  git -C "$fixture" push --quiet "$remote" "HEAD:refs/heads/docs-assets-candidate"
  printf '%s\n' "$fixture_commit" > "$ref_file"
}

run_sync() {
  DOCBANK_DOCS_ASSETS_REMOTE="$remote" \
  DOCBANK_DOCS_ASSETS_MANIFEST="$manifest" \
  DOCBANK_DOCS_ASSETS_REF="$ref_file" \
  DOCBANK_DOCS_ASSETS_CACHE="$cache" \
    "$sync_script"
}

expect_failure() {
  local label="$1"
  if run_sync >"$test_root/stdout" 2>"$test_root/stderr"; then
    fail "$label unexpectedly succeeded"
  fi
}

printf 'one.png\n' > "$manifest"
create_remote valid
run_sync
destination="$cache/$fixture_commit"
test -f "$destination/one.png" || fail "valid PNG was not cached"
test -f "$destination/.sha256" || fail "checksum record was not cached"
before_checksum="$(checksum "$destination/one.png")"

printf 'docs-assets-candidate\n' > "$ref_file"
expect_failure "mutable ref"
printf '%s\n' "$fixture_commit" > "$ref_file"

printf 'one.png\ntwo.png\n' > "$manifest"
expect_failure "missing manifest member"
test "$(checksum "$destination/one.png")" = "$before_checksum" ||
  fail "failed sync changed the previous cache"

printf 'nested/one.png\n' > "$manifest"
expect_failure "nested manifest name"

printf 'one.png\none.png\n' > "$manifest"
expect_failure "duplicate manifest name"

printf 'one.png\n\n' > "$manifest"
expect_failure "blank manifest entry"

printf 'one.jpg\n' > "$manifest"
expect_failure "unsupported manifest extension"

printf 'one.png\n' > "$manifest"
create_remote extra
expect_failure "unmanifested PNG"

create_remote malformed
expect_failure "invalid PNG signature"

create_remote oversized
expect_failure "oversized PNG"

create_remote nested
expect_failure "nested tree entry"

printf 'docs asset sync tests passed\n'
