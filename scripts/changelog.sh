#!/usr/bin/env bash
# Generate release notes from commits since the previous release.
# Usage: ./scripts/changelog.sh [version] [start_tag] [extra_instructions]
# Set CHANGELOG_AGENT=claude to use Claude instead of Codex (default).
# If version is omitted, NEXT is used as a placeholder.
# If start_tag is - or omitted, the previous tag is auto-detected.

set -euo pipefail

VERSION="${1:-NEXT}"
START_TAG="${2:-}"
EXTRA_INSTRUCTIONS="${3:-}"
AGENT="${CHANGELOG_AGENT:-codex}"
REPO_ROOT="$(git rev-parse --show-toplevel)"
CHANGELOG_PATHS=(
    .
    ':(exclude)docs/**'
    ':(exclude)AGENTS.md'
    ':(exclude).roborev.toml'
)

if [[ -n "$START_TAG" && "$START_TAG" != "-" ]]; then
    git -C "$REPO_ROOT" rev-parse --verify "${START_TAG}^{commit}" >/dev/null
    RANGE="${START_TAG}..HEAD"
    RANGE_LABEL="$START_TAG"
    echo "Generating changelog from $START_TAG to HEAD..." >&2
else
    PREV_TAG="$(git -C "$REPO_ROOT" describe --tags --abbrev=0 2>/dev/null || true)"
    if [[ -z "$PREV_TAG" ]]; then
        FIRST_COMMIT="$(git -C "$REPO_ROOT" rev-list --max-parents=0 HEAD)"
        RANGE="${FIRST_COMMIT}..HEAD"
        RANGE_LABEL="$FIRST_COMMIT"
        echo "No previous release found. Generating changelog for all commits..." >&2
    else
        RANGE="${PREV_TAG}..HEAD"
        RANGE_LABEL="$PREV_TAG"
        echo "Generating changelog from $PREV_TAG to HEAD..." >&2
    fi
fi

# Documentation-only and reviewer-configuration commits are intentionally
# excluded from release-note material.
COMMITS="$(git -C "$REPO_ROOT" log "$RANGE" --pretty=format:'- %s (%h)' --no-merges -- "${CHANGELOG_PATHS[@]}")"
DIFF_STAT="$(git -C "$REPO_ROOT" diff --stat "$RANGE" -- "${CHANGELOG_PATHS[@]}")"

if [[ -z "$COMMITS" ]]; then
    echo "No non-documentation commits since $RANGE_LABEL" >&2
    exit 0
fi

echo "Using $AGENT to generate changelog..." >&2

OUTPUT_FILE="$(mktemp)"
PROMPT_FILE="$(mktemp)"
trap 'rm -f "$OUTPUT_FILE" "$PROMPT_FILE"' EXIT

cat >"$PROMPT_FILE" <<EOF
You are generating release notes for docbank version $VERSION.

docbank is a local-first personal document archive. A daemon owns its SQLite
metadata and content-addressed blob vault; the CLI and external applications use
its authenticated loopback HTTP API to ingest, organize, search, and verify files.

IMPORTANT: Do NOT use tools, run shell commands, search, or read files. Everything
needed is in the commit list and diff summary below.

Commits since the previous release:
$COMMITS

Diff summary:
$DIFF_STAT

Generate concise, user-focused Markdown release notes. Use only the sections that
have content, choosing from:
- New Features
- Improvements
- Bug Fixes

Focus on user-visible changes. Skip internal refactoring, tests, release plumbing,
review follow-ups, and documentation-only changes unless they materially change
installation or operation. Keep each description to one line and use present tense.
Do not mention bugs introduced and fixed within this same release cycle.
${EXTRA_INSTRUCTIONS:+

Pay particular attention to these features or improvements when they appear in the
commit list: $EXTRA_INSTRUCTIONS
Do not inspect anything outside the supplied commit list and diff summary.}
Output ONLY the Markdown release-note content, with no preamble.
EOF

case "$AGENT" in
    codex)
        codex exec --skip-git-repo-check --sandbox read-only \
            -c model_reasoning_effort=high -o "$OUTPUT_FILE" - >/dev/null <"$PROMPT_FILE"
        ;;
    claude)
        claude --print <"$PROMPT_FILE" >"$OUTPUT_FILE"
        ;;
    *)
        echo "Error: unknown CHANGELOG_AGENT '$AGENT' (expected 'codex' or 'claude')" >&2
        exit 1
        ;;
esac

if [[ ! -s "$OUTPUT_FILE" ]]; then
    echo "Error: $AGENT returned empty release notes" >&2
    exit 1
fi

cat "$OUTPUT_FILE"
