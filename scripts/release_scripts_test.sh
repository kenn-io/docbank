#!/usr/bin/env bash
set -euo pipefail

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
tmp_root="$(mktemp -d)"
trap 'rm -rf "$tmp_root"' EXIT

fail() {
  printf 'FAIL: %s\n' "$*" >&2
  exit 1
}

assert_contains() {
  local haystack="$1" needle="$2" context="$3"
  if [[ "$haystack" != *"$needle"* ]]; then
    fail "$context: expected to find [$needle] in [$haystack]"
  fi
}

assert_not_contains() {
  local haystack="$1" needle="$2" context="$3"
  if [[ "$haystack" == *"$needle"* ]]; then
    fail "$context: did not expect to find [$needle] in [$haystack]"
  fi
}

init_repo() {
  local dir="$1"
  mkdir -p "$dir"
  git -C "$dir" init -q -b main
  git -C "$dir" config user.name "Example User"
  git -C "$dir" config user.email "example@example.test"
  printf 'example archive\n' >"$dir/README.md"
  git -C "$dir" add README.md
  git -C "$dir" commit -q -m "feat: add document archive"
}

install_scripts() {
  local dir="$1"
  mkdir -p "$dir/scripts"
  cp "$repo_root/scripts/changelog.sh" "$repo_root/scripts/release.sh" "$dir/scripts/"
  chmod +x "$dir/scripts/changelog.sh" "$dir/scripts/release.sh"
  git -C "$dir" add scripts
  git -C "$dir" commit -q -m "chore: add release scripts"
}

run_in_repo() {
  local dir="$1"
  shift
  (
    cd "$dir"
    "$@"
  )
}

fake_gh() {
  local dir="$1"
  mkdir -p "$dir"
  cat >"$dir/gh" <<'EOF'
#!/usr/bin/env bash
exit 0
EOF
  chmod +x "$dir/gh"
}

test_release_rejects_missing_version() {
  local output status
  set +e
  output="$("$repo_root/scripts/release.sh" 2>&1)"
  status=$?
  set -e
  [[ $status -eq 2 ]] || fail "release.sh without version should exit 2"
  assert_contains "$output" "usage:" "missing version"
}

test_release_rejects_prefixed_and_invalid_versions() {
  local output status
  set +e
  output="$("$repo_root/scripts/release.sh" v0.11.0 2>&1)"
  status=$?
  set -e
  [[ $status -eq 2 ]] || fail "release.sh should reject v-prefixed versions"
  assert_contains "$output" "must be bare" "v-prefixed version"

  set +e
  output="$("$repo_root/scripts/release.sh" 0.11 2>&1)"
  status=$?
  set -e
  [[ $status -eq 2 ]] || fail "release.sh should reject incomplete semver"
  assert_contains "$output" "X.Y.Z" "invalid version"
}

test_release_refuses_dirty_main() {
  local repo="$tmp_root/dirty"
  init_repo "$repo"
  install_scripts "$repo"
  printf 'dirty\n' >"$repo/dirty.txt"

  local output status
  set +e
  output="$(run_in_repo "$repo" env CHANGELOG_AGENT=none "$repo/scripts/release.sh" 0.11.0 2>&1)"
  status=$?
  set -e
  [[ $status -ne 0 ]] || fail "release.sh should reject dirty worktrees"
  assert_contains "$output" "worktree is dirty" "dirty worktree"
  assert_not_contains "$output" "PROPOSED RELEASE NOTES" "dirty worktree"
}

test_changelog_fallback_includes_first_commit() {
  local repo="$tmp_root/fallback"
  init_repo "$repo"

  local output
  output="$(run_in_repo "$repo" env CHANGELOG_AGENT=none "$repo_root/scripts/changelog.sh" NEXT -)"
  assert_contains "$output" "## New Features" "fallback heading"
  assert_contains "$output" "feat: add document archive" "first commit"
}

test_changelog_fallback_groups_changes() {
  local repo="$tmp_root/groups"
  init_repo "$repo"
  printf 'faster\n' >"$repo/perf.txt"
  git -C "$repo" add perf.txt
  git -C "$repo" commit -q -m "perf: speed up archive traversal"
  printf 'fixed\n' >"$repo/fix.txt"
  git -C "$repo" add fix.txt
  git -C "$repo" commit -q -m "fix: retain document names"

  local output
  output="$(run_in_repo "$repo" env CHANGELOG_AGENT=none "$repo_root/scripts/changelog.sh" NEXT -)"
  assert_contains "$output" "## New Features" "feature section"
  assert_contains "$output" "## Improvements" "improvement section"
  assert_contains "$output" "## Bug Fixes" "bug-fix section"
}

test_changelog_agent_receives_context() {
  local repo="$tmp_root/agent"
  local fake_bin="$tmp_root/agent-bin"
  init_repo "$repo"
  mkdir -p "$fake_bin"
  cat >"$fake_bin/codex" <<'EOF'
#!/usr/bin/env bash
out=""
while [[ $# -gt 0 ]]; do
  if [[ "$1" == "-o" ]]; then
    shift
    out="$1"
  fi
  shift || true
done
prompt="$(cat)"
[[ "$prompt" == *"feat: add document archive"* ]] || exit 3
printf '## New Features\n\n- Summarizes supplied document changes.\n' >"$out"
EOF
  chmod +x "$fake_bin/codex"

  local output
  output="$(run_in_repo "$repo" env PATH="$fake_bin:$PATH" "$repo_root/scripts/changelog.sh" NEXT -)"
  assert_contains "$output" "Summarizes supplied document changes" "agent output"
}

test_changelog_rejects_unknown_agent() {
  local repo="$tmp_root/unknown-agent"
  init_repo "$repo"

  local output status
  set +e
  output="$(run_in_repo "$repo" env CHANGELOG_AGENT=example "$repo_root/scripts/changelog.sh" NEXT - 2>&1)"
  status=$?
  set -e
  [[ $status -ne 0 ]] || fail "changelog.sh should reject unknown agents"
  assert_contains "$output" "unknown CHANGELOG_AGENT" "unknown agent"
}

test_release_preview_and_push() {
  local repo="$tmp_root/release"
  local remote="$tmp_root/origin.git"
  local fake_bin="$tmp_root/gh-bin"
  init_repo "$repo"
  install_scripts "$repo"
  git init -q --bare "$remote"
  git -C "$repo" remote add origin "$remote"
  git -C "$repo" push -q -u origin main
  fake_gh "$fake_bin"

  local output
  output="$(printf 'yes\n' | run_in_repo "$repo" env PATH="$fake_bin:$PATH" CHANGELOG_AGENT=none "$repo/scripts/release.sh" 0.11.0)"
  assert_contains "$output" "PROPOSED RELEASE NOTES FOR v0.11.0" "release preview"
  assert_contains "$output" "Release v0.11.0 tag pushed successfully" "release outcome"
  git -C "$remote" rev-parse -q --verify refs/tags/v0.11.0 >/dev/null || fail "remote tag missing"
  assert_contains "$(git -C "$repo" tag -l v0.11.0 --format='%(contents)')" "Release 0.11.0" "annotated tag"
}

test_release_cancellation_creates_no_tag() {
  local repo="$tmp_root/cancel"
  local remote="$tmp_root/cancel-origin.git"
  local fake_bin="$tmp_root/cancel-gh-bin"
  init_repo "$repo"
  install_scripts "$repo"
  git init -q --bare "$remote"
  git -C "$repo" remote add origin "$remote"
  git -C "$repo" push -q -u origin main
  fake_gh "$fake_bin"

  local output
  output="$(printf 'n\n' | run_in_repo "$repo" env PATH="$fake_bin:$PATH" CHANGELOG_AGENT=none "$repo/scripts/release.sh" 0.11.0)"
  assert_contains "$output" "Release cancelled." "release cancellation"
  if git -C "$repo" rev-parse -q --verify refs/tags/v0.11.0 >/dev/null; then
    fail "cancelled release created a tag"
  fi
}

test_release_rejects_missing_version
test_release_rejects_prefixed_and_invalid_versions
test_release_refuses_dirty_main
test_changelog_fallback_includes_first_commit
test_changelog_fallback_groups_changes
test_changelog_agent_receives_context
test_changelog_rejects_unknown_agent
test_release_preview_and_push
test_release_cancellation_creates_no_tag

printf 'release script tests passed\n'
