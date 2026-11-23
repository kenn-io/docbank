#!/usr/bin/env bash
# Check release tags with the storage schema catalog test. Requires full history.
set -euo pipefail

tags=$(git tag -l 'v[0-9]*' --sort=v:refname)
if [[ -z $tags ]]; then
	echo "error: no release tags found; check out with full history (fetch-depth: 0)" >&2
	exit 1
fi
scratch=$(mktemp -d)
trap 'find "$scratch" -depth -delete' EXIT
for tag in $tags; do
	mkdir -p "$scratch/$tag"
	for file in store.go schema.sql; do
		entry=$(git ls-tree "$tag" -- "internal/store/$file")
		if [[ -n $entry ]]; then
			git show "$tag:internal/store/$file" >"$scratch/$tag/$file"
		fi
	done
	printf '%s\n' "$tag" >>"$scratch/order"
done
DOCBANK_RELEASE_SCHEMAS="$scratch" go test -tags fts5 ./internal/store -run '^TestReleaseTagSchemas$' -count=1
