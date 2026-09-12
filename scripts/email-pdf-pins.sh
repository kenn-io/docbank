#!/usr/bin/env bash
# Canonical document/emailpdf.TreeSHA256 encoding for an operator-owned tree.
set -euo pipefail
export LC_ALL=C
if [[ $# != 1 || ! -d "$1" || -L "$1" ]]; then
  echo 'usage: bash scripts/email-pdf-pins.sh <ordinary-runtime-or-font-directory>' >&2
  exit 2
fi
cd -- "$1"
if [[ -n "$(find . ! -type d ! -type f -print -quit)" ]]; then
  echo 'runtime trees cannot contain links or special files' >&2
  exit 1
fi
if [[ -z "$(find . -type f -print -quit)" ]]; then
  echo 'runtime tree is empty' >&2
  exit 1
fi
find . -type f -print0 | sort -z | while IFS= read -r -d '' path; do
  digest=$(sha256sum < "$path")
  printf '%s\0%s\n' "${path#./}" "${digest%% *}"
done | sha256sum | cut -d ' ' -f 1
