#!/bin/sh
set -eu

source_sha=${1:-}
expected_tag=${2:-}

fail() {
  printf 'documentation release validation failed: %s\n' "$1" >&2
  exit 1
}

if [ "${#source_sha}" -ne 40 ]; then
  fail "source must be a full 40-character commit SHA"
fi
case "$source_sha" in
  *[!0-9a-fA-F]*) fail "source must be a full 40-character commit SHA" ;;
esac
if ! printf '%s\n' "$expected_tag" | grep -Eq '^v[0-9]+\.[0-9]+\.[0-9]+$'; then
  fail "expected release tag must use vX.Y.Z form"
fi

git fetch --quiet origin refs/heads/main:refs/remotes/origin/main --tags
git cat-file -e "$source_sha^{commit}" 2>/dev/null || fail "source commit does not exist: $source_sha"
git rev-parse --verify --quiet "$expected_tag^{commit}" >/dev/null || fail "release tag does not exist: $expected_tag"

if ! git merge-base --is-ancestor "$source_sha" origin/main; then
  fail "documentation source $source_sha is not on origin/main"
fi
if ! git merge-base --is-ancestor "$expected_tag" "$source_sha"; then
  fail "release tag $expected_tag is not an ancestor of documentation source $source_sha"
fi

latest_tag=$(git describe --tags --abbrev=0 --match 'v[0-9]*.[0-9]*.[0-9]*' origin/main 2>/dev/null) || {
  fail "origin/main has no release tag"
}
if [ "$latest_tag" != "$expected_tag" ]; then
  fail "latest release on origin/main changed from $expected_tag to $latest_tag"
fi

git diff --name-only "$expected_tag..$source_sha" | while IFS= read -r changed_path; do
  case "$changed_path" in
    .github/workflows/deploy-docs.yml | \
    .vercelignore | \
    AGENTS.md | \
    Makefile | \
    README.md | \
    scripts/deploy-docs.sh | \
    scripts/docs-assets.ref | \
    scripts/docs-assets.txt | \
    scripts/sync-docs-assets.sh | \
    scripts/validate-docs-release.sh | \
    scripts/vercel-build-docs.sh | \
    scripts/vercel-install-docs.sh | \
    vercel.json | \
    LICENSES/* | \
    docs/* | \
    scripts/docs/* | \
    website/*)
      ;;
    *)
      fail "release-gated documentation source contains product change: $changed_path"
      ;;
  esac
done

printf 'validated documentation source %s at release %s\n' "$source_sha" "$expected_tag"
