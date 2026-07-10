#!/usr/bin/env bash
# Generate release notes, create an annotated version tag, and push it.

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "$SCRIPT_DIR/.." && pwd)"

VERSION="${1:-}"
EXTRA_INSTRUCTIONS="${2:-}"

if [[ -z "$VERSION" ]]; then
    echo "Usage: $0 <version> [extra_instructions]"
    echo "Example: $0 0.2.0"
    echo "Example: $0 0.2.0 \"Focus on editing and version history\""
    exit 1
fi

if [[ ! "$VERSION" =~ ^[0-9]+\.[0-9]+\.[0-9]+$ ]]; then
    echo "Error: version must use X.Y.Z format (for example, 0.2.0)"
    exit 1
fi

TAG="v$VERSION"

if [[ "$(git -C "$REPO_ROOT" branch --show-current)" != "main" ]]; then
    echo "Error: releases must be cut from main"
    exit 1
fi

if [[ -n "$(git -C "$REPO_ROOT" status --porcelain)" ]]; then
    echo "Error: the working tree is not clean; commit or stash changes first"
    exit 1
fi

if ! command -v gh >/dev/null 2>&1; then
    echo "Error: gh CLI is required (https://cli.github.com/)"
    exit 1
fi
if ! gh auth status >/dev/null 2>&1; then
    echo "Error: gh CLI is not authenticated"
    exit 1
fi

git -C "$REPO_ROOT" fetch --quiet origin \
    '+refs/heads/main:refs/remotes/origin/main' --tags

if [[ "$(git -C "$REPO_ROOT" rev-parse HEAD)" != "$(git -C "$REPO_ROOT" rev-parse origin/main)" ]]; then
    echo "Error: local main must exactly match origin/main"
    exit 1
fi

if git -C "$REPO_ROOT" rev-parse --verify "refs/tags/$TAG" >/dev/null 2>&1; then
    echo "Error: tag $TAG already exists"
    exit 1
fi

CHANGELOG_FILE="$(mktemp)"
trap 'rm -f "$CHANGELOG_FILE"' EXIT

"$SCRIPT_DIR/changelog.sh" "$VERSION" - "$EXTRA_INSTRUCTIONS" >"$CHANGELOG_FILE"

if [[ ! -s "$CHANGELOG_FILE" ]]; then
    echo "Error: no release-note content was generated"
    exit 1
fi

echo
echo "=========================================="
echo "PROPOSED RELEASE NOTES FOR $TAG"
echo "=========================================="
cat "$CHANGELOG_FILE"
echo
echo "=========================================="
echo

read -r -p "Accept these notes and create release $TAG? [y/N] " REPLY
if [[ ! "$REPLY" =~ ^[Yy]$ ]]; then
    echo "Release cancelled."
    exit 0
fi

echo "Creating annotated tag $TAG..."
git -C "$REPO_ROOT" tag -a "$TAG" \
    -m "Release $VERSION" \
    -m "$(cat "$CHANGELOG_FILE")"

echo "Pushing tag to origin..."
git -C "$REPO_ROOT" push origin "$TAG"

echo
echo "Release $TAG tag pushed successfully."
echo "GitHub Actions will publish the archives, checksums, and tag-message notes."
echo "https://github.com/kenn-io/docbank/releases/tag/$TAG"
