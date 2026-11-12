#!/usr/bin/env bash
set -euo pipefail

root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
command="$(sed -n 's/^        run: \(git diff --no-renames.*\)$/\1/p' "$root/.github/workflows/ci-pr.yml")"
test -n "$command"
test_root="$(mktemp -d)"
trap 'find "$test_root" -depth -delete' EXIT
for variable in $(env | sed -n 's/^\(GIT_[A-Za-z0-9_]*\)=.*/\1/p'); do
  unset "$variable"
done
export GIT_CONFIG_GLOBAL="$test_root/gitconfig" GIT_CONFIG_NOSYSTEM=1
cd "$test_root"
git init -q
git config user.name "Docbank Test"
git config user.email "docbank-test@example.invalid"
records=internal/store/testdata/schema-contracts
mkdir -p "$records"
touch "$records/base.schema"
git add .
git commit -qm base
export BASE_SHA="$(git rev-parse HEAD)"

printf 'changed\n' > "$records/base.schema"
git add .
git commit -qm modified
if bash -c "$command" >/dev/null; then
  echo 'modified base record was accepted' >&2
  exit 1
fi

git checkout "$BASE_SHA" -- "$records/base.schema"
git add .
git commit -qm restored
touch "$records/new.schema"
git add .
git commit -qm added
if ! bash -c "$command" >/dev/null; then
  echo 'new record was rejected' >&2
  exit 1
fi

find "$records/base.schema" -delete
git add -A
git commit -qm deleted
if bash -c "$command" >/dev/null; then
  echo 'deleted base record was accepted' >&2
  exit 1
fi
