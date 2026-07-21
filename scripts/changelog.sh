#!/usr/bin/env bash
# Generate release notes from commits since the previous release.
# Usage: ./scripts/changelog.sh [version] [start_tag] [extra_instructions]
# Set CHANGELOG_AGENT=codex (default), claude, or none.

set -euo pipefail

version="${1:-NEXT}"
start_tag="${2:-}"
extra_instructions="${3:-}"
agent="${CHANGELOG_AGENT:-codex}"
repo_root="$(git rev-parse --show-toplevel)"
changelog_paths=(
  .
  ':(exclude)docs/**'
  ':(exclude)AGENTS.md'
  ':(exclude).roborev.toml'
)

range_spec=""
range_label="the beginning of the repository"
if [[ -n "$start_tag" && "$start_tag" != "-" ]]; then
  git -C "$repo_root" rev-parse --verify "${start_tag}^{commit}" >/dev/null
  range_spec="${start_tag}..HEAD"
  range_label="$start_tag"
elif previous_tag="$(git -C "$repo_root" describe --tags --abbrev=0 2>/dev/null)"; then
  range_spec="${previous_tag}..HEAD"
  range_label="$previous_tag"
fi

git_log() {
  if [[ -n "$range_spec" ]]; then
    git -C "$repo_root" log --no-merges "$@" "$range_spec" -- "${changelog_paths[@]}"
  else
    git -C "$repo_root" log --no-merges "$@" -- "${changelog_paths[@]}"
  fi
}

git_diff_stat() {
  if [[ -n "$range_spec" ]]; then
    git -C "$repo_root" diff --stat "$range_spec" -- "${changelog_paths[@]}"
  else
    local empty_tree
    empty_tree="$(git -C "$repo_root" hash-object -t tree /dev/null)"
    git -C "$repo_root" diff --stat "$empty_tree" HEAD -- "${changelog_paths[@]}"
  fi
}

fallback_changelog() {
  local log_output
  log_output="$(git_log --pretty=format:'%s%x09%h')"
  if [[ -z "$log_output" ]]; then
    printf '## Changes\n\n- No commits since %s.\n' "$range_label"
    return
  fi

  local features="" improvements="" fixes=""
  while IFS=$'\t' read -r subject short_hash; do
    [[ -n "$subject" ]] || continue
    local entry="- ${subject} (${short_hash})"
    case "$subject" in
      feat:*|feat\(*\):*|feature:*|feature\(*\):*)
        features+="${entry}"$'\n'
        ;;
      fix:*|fix\(*\):*|bugfix:*|bugfix\(*\):*)
        fixes+="${entry}"$'\n'
        ;;
      docs:*|docs\(*\):*|doc:*|doc\(*\):*)
        ;;
      *)
        improvements+="${entry}"$'\n'
        ;;
    esac
  done <<<"$log_output"

  local printed=0
  if [[ -n "$features" ]]; then
    printf '## New Features\n\n%s' "$features"
    printed=1
  fi
  if [[ -n "$improvements" ]]; then
    [[ $printed -eq 0 ]] || printf '\n'
    printf '## Improvements\n\n%s' "$improvements"
    printed=1
  fi
  if [[ -n "$fixes" ]]; then
    [[ $printed -eq 0 ]] || printf '\n'
    printf '## Bug Fixes\n\n%s' "$fixes"
    printed=1
  fi
  if [[ $printed -eq 0 ]]; then
    printf '## Improvements\n\n- No user-facing changes in this commit range.\n'
  fi
}

if [[ "$agent" == "none" ]]; then
  fallback_changelog
  exit 0
fi

prompt_file="$(mktemp)"
log_file="$(mktemp)"
diff_file="$(mktemp)"
notes_file="$(mktemp)"
err_file="$(mktemp)"
trap 'rm -f "$prompt_file" "$log_file" "$diff_file" "$notes_file" "$err_file"' EXIT

git_log --date=short --pretty=format:'%ad%x09%h%x09%s' >"$log_file"
git_diff_stat >"$diff_file"

if [[ ! -s "$log_file" ]]; then
  printf 'No non-documentation commits since %s\n' "$range_label" >&2
  exit 0
fi

{
  printf 'Write concise Markdown release notes for docbank %s.\n\n' "$version"
  printf 'IMPORTANT: Do not use tools, run shell commands, search, or read files.\n'
  printf 'All required information is provided below. Analyze only the commit log and diff summary.\n\n'
  printf 'docbank is local-first document storage for people, agents, and embedded Go applications.\n'
  printf 'Its daemon owns SQLite metadata and a content-addressed blob vault; clients use an authenticated loopback HTTP API.\n'
  printf 'Use user-facing language and group changes under these Markdown headings when applicable:\n'
  printf '%s\n' '- ## New Features' '- ## Improvements' '- ## Bug Fixes'
  printf 'Use only headings that have entries. Keep every entry to one line and use present tense.\n'
  printf 'Skip internal refactoring, tests, review follow-ups, and release plumbing.\n'
  printf 'Skip documentation-only changes unless they materially change installation or operation.\n'
  printf 'Do not mention bugs introduced and fixed within this same release cycle.\n'
  printf 'Output only the release notes, with no preamble.\n'
  if [[ -n "$extra_instructions" ]]; then
    printf '\nAdditional instructions:\n%s\n' "$extra_instructions"
  fi
  printf '\nCommits since %s:\n' "$range_label"
  cat "$log_file"
  printf '\n\nDiff summary:\n'
  cat "$diff_file"
} >"$prompt_file"

run_agent() {
  case "$agent" in
    codex)
      if ! command -v codex >/dev/null 2>&1; then
        printf 'codex not found; install it or set CHANGELOG_AGENT=none for deterministic notes\n' >&2
        return 127
      fi
      local codex_rust_log
      codex_rust_log="${CHANGELOG_CODEX_RUST_LOG:-${RUST_LOG:-error,codex_core::rollout::list=off}}"
      RUST_LOG="$codex_rust_log" codex exec \
        --json \
        --skip-git-repo-check \
        --sandbox read-only \
        -c reasoning_effort=high \
        -o "$notes_file" \
        - >/dev/null <"$prompt_file" 2>"$err_file"
      ;;
    claude)
      if ! command -v claude >/dev/null 2>&1; then
        printf 'claude not found; install it or set CHANGELOG_AGENT=none for deterministic notes\n' >&2
        return 127
      fi
      claude --print <"$prompt_file" >"$notes_file" 2>"$err_file"
      ;;
    *)
      printf 'unknown CHANGELOG_AGENT %q; expected codex, claude, or none\n' "$agent" >&2
      return 2
      ;;
  esac
}

agent_status=0
set +e
run_agent
agent_status=$?
set -e

if [[ $agent_status -ne 0 || ! -s "$notes_file" ]]; then
  printf '%s failed to generate release notes\n' "$agent" >&2
  if [[ "${CHANGELOG_DEBUG:-0}" == "1" && -s "$err_file" ]]; then
    cat "$err_file" >&2
  elif [[ -s "$err_file" ]]; then
    filtered_err="$(grep -E -v 'rollout path for thread|failed to record rollout items: failed to queue rollout items: channel closed|^mcp startup: no servers$|^WARNING: proceeding, even though we could not update PATH:' "$err_file" || true)"
    if [[ -n "$filtered_err" ]]; then
      printf '%s\n' "$filtered_err" >&2
    else
      printf 'Set CHANGELOG_DEBUG=1 to print full agent logs.\n' >&2
    fi
  fi
  exit 1
fi

if [[ "${CHANGELOG_DEBUG:-0}" == "1" && -s "$err_file" ]]; then
  cat "$err_file" >&2
fi
cat "$notes_file"
