#!/bin/sh
set -eu

repository_root=$(CDPATH='' cd -- "$(dirname -- "$0")/.." && pwd)
validator="$repository_root/scripts/validate-docs-release.sh"
scratch=$(mktemp -d -t docbank-release-validation.XXXXXX)
trap 'find "$scratch" -depth -delete' EXIT HUP INT TERM
tests=0


new_fixture() {
  fixture="$scratch/repo-$tests"
  remote="$scratch/remote-$tests.git"
  git init --quiet --bare "$remote"
  git init --quiet -b main "$fixture"
  git -C "$fixture" config user.name "Release Test"
  git -C "$fixture" config user.email "release-test@example.invalid"
  git -C "$fixture" remote add origin "$remote"
  mkdir -p "$fixture/cmd/docbank" "$fixture/docs"
  printf 'package main\n' > "$fixture/cmd/docbank/main.go"
  printf '# Documentation\n' > "$fixture/docs/index.md"
  git -C "$fixture" add .
  git -C "$fixture" commit --quiet -m "initial release"
  git -C "$fixture" tag v1.0.0
  git -C "$fixture" push --quiet -u origin main --tags

  printf '\nRelease notes.\n' >> "$fixture/docs/index.md"
  git -C "$fixture" commit --quiet -am "docs: add release notes"
  git -C "$fixture" push --quiet origin main
  source_sha=$(git -C "$fixture" rev-parse HEAD)
}


expect_pass() {
  expected_tag=$1
  output=$(cd "$fixture" && "$validator" "$source_sha" "$expected_tag" 2>&1) || {
    printf 'expected success, got:\n%s\n' "$output" >&2
    exit 1
  }
  printf '%s\n' "$output" | grep -F "validated documentation source $source_sha at release $expected_tag" >/dev/null
  tests=$((tests + 1))
}


expect_fail() {
  expected_tag=$1
  expected_message=$2
  if output=$(cd "$fixture" && "$validator" "$source_sha" "$expected_tag" 2>&1); then
    printf 'expected failure, got success:\n%s\n' "$output" >&2
    exit 1
  fi
  printf '%s\n' "$output" | grep -F "$expected_message" >/dev/null || {
    printf 'expected failure containing %s, got:\n%s\n' "$expected_message" "$output" >&2
    exit 1
  }
  tests=$((tests + 1))
}


new_fixture
expect_pass v1.0.0

new_fixture
git -C "$fixture" switch --quiet --orphan unrelated
git -C "$fixture" rm --quiet -rf --ignore-unmatch .
printf 'unrelated\n' > "$fixture/unrelated.txt"
git -C "$fixture" add unrelated.txt
git -C "$fixture" commit --quiet -m "unrelated release"
git -C "$fixture" tag v9.0.0
git -C "$fixture" switch --quiet main
expect_fail v9.0.0 "is not an ancestor of documentation source"

new_fixture
printf '\nUnpublished.\n' >> "$fixture/docs/index.md"
git -C "$fixture" commit --quiet -am "docs: unpublished edit"
source_sha=$(git -C "$fixture" rev-parse HEAD)
expect_fail v1.0.0 "is not on origin/main"

new_fixture
printf 'package main\n\nvar version = 2\n' > "$fixture/cmd/docbank/main.go"
git -C "$fixture" commit --quiet -am "feat: change product"
git -C "$fixture" push --quiet origin main
source_sha=$(git -C "$fixture" rev-parse HEAD)
expect_fail v1.0.0 "release-gated documentation source contains product change: cmd/docbank/main.go"

new_fixture
git -C "$fixture" tag v1.1.0 "$source_sha"
git -C "$fixture" push --quiet origin v1.1.0
expect_fail v1.0.0 "latest release on origin/main changed from v1.0.0 to v1.1.0"

printf '%s release validation tests passed\n' "$tests"
