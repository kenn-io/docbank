#!/usr/bin/env bash
# Generate release notes, ask the operator to approve them, then tag and push.
# Usage: ./scripts/release.sh <version> [extra_instructions] [start_tag]

set -euo pipefail

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"

usage() {
  printf 'usage: %s <bare-version> [extra changelog instructions] [start tag]\n' "$0" >&2
  printf 'example: %s 0.11.0\n' "$0" >&2
}

version="${1:-}"
extra_instructions="${2:-}"
start_tag="${3:--}"

if [[ -z "$version" ]]; then
  usage
  exit 2
fi
if [[ "$version" == v* ]]; then
  printf 'version must be bare, such as 0.11.0, not %s\n' "$version" >&2
  exit 2
fi
if [[ ! "$version" =~ ^[0-9]+\.[0-9]+\.[0-9]+$ ]]; then
  printf 'version must use X.Y.Z semver shape\n' >&2
  exit 2
fi

tag="v${version}"
changelog_agent="${CHANGELOG_AGENT:-codex}"

if [[ "$(git -C "$repo_root" branch --show-current)" != "main" ]]; then
  printf 'releases must be cut from main\n' >&2
  exit 1
fi

git -C "$repo_root" update-index -q --refresh
if [[ -n "$(git -C "$repo_root" status --porcelain)" ]]; then
  printf 'worktree is dirty; commit or stash changes before releasing\n' >&2
  exit 1
fi

if ! command -v gh >/dev/null 2>&1; then
  printf 'gh CLI is required: https://cli.github.com/\n' >&2
  exit 1
fi
if ! gh auth status >/dev/null 2>&1; then
  printf 'gh CLI is not authenticated\n' >&2
  exit 1
fi

git -C "$repo_root" fetch --quiet origin \
  '+refs/heads/main:refs/remotes/origin/main' --tags

if [[ "$(git -C "$repo_root" rev-parse HEAD)" != "$(git -C "$repo_root" rev-parse origin/main)" ]]; then
  printf 'local main must exactly match origin/main\n' >&2
  exit 1
fi
if git -C "$repo_root" rev-parse --verify "refs/tags/$tag" >/dev/null 2>&1; then
  printf 'tag %s already exists\n' "$tag" >&2
  exit 1
fi

case "$changelog_agent" in
  codex)
    printf 'Generating %s notes with Codex from the supplied git history.\n' "$tag"
    ;;
  claude)
    printf 'Generating %s notes with Claude from the supplied git history.\n' "$tag"
    ;;
  none)
    printf 'Generating %s notes with the deterministic git-log fallback.\n' "$tag"
    ;;
  *)
    printf 'Generating %s notes with CHANGELOG_AGENT=%s; changelog.sh will validate it.\n' "$tag" "$changelog_agent"
    ;;
esac

notes_file="$(mktemp)"
tag_message="$(mktemp)"
trap 'rm -f "$notes_file" "$tag_message"' EXIT

"$repo_root/scripts/changelog.sh" "$version" "$start_tag" "$extra_instructions" >"$notes_file"
if [[ ! -s "$notes_file" ]]; then
  printf 'no release-note content was generated\n' >&2
  exit 1
fi

printf '\n==========================================\n'
printf 'PROPOSED RELEASE NOTES FOR %s\n' "$tag"
printf '==========================================\n'
cat "$notes_file"
printf '\n==========================================\n\n'

printf 'Accept these notes and create release %s? [y/N] ' "$tag"
answer=""
read -r answer || true
printf '\n'
if [[ "$answer" != "y" && "$answer" != "Y" && "$answer" != "yes" && "$answer" != "YES" ]]; then
  printf 'Release cancelled.\n'
  exit 0
fi

{
  printf 'Release %s\n\n' "$version"
  cat "$notes_file"
} >"$tag_message"

printf 'Creating annotated tag %s...\n' "$tag"
git -C "$repo_root" tag --cleanup=whitespace -a "$tag" -F "$tag_message"
printf 'Pushing %s to origin...\n' "$tag"
git -C "$repo_root" push origin "$tag"

printf '\nRelease %s tag pushed successfully.\n' "$tag"
printf 'GitHub Actions will publish archives, checksums, and the approved notes.\n'
printf 'https://github.com/kenn-io/docbank/releases/tag/%s\n' "$tag"
