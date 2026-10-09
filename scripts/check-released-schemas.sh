#!/usr/bin/env bash
# Fails when a release tag's storage schema has no upgrade path or fixture.
# Every tagged schema version must appear in releasedStorageSchemaVersions,
# and every tag from schema 3 onward must match an exact released-schema
# fixture byte for byte. Tags before v0.10.0 have no explicit schema version;
# every later tag must declare one. Requires full tag history (fetch-depth: 0).
set -euo pipefail

first_versioned_tag=v0.10.0

repo_root=$(git rev-parse --show-toplevel)
upgrade_go="$repo_root/internal/store/upgrade.go"
fixtures_dir="$repo_root/internal/store/testdata"

released=$(sed -nE 's/^var releasedStorageSchemaVersions = \[\]int\{([0-9, ]+)\}.*/\1/p' "$upgrade_go" | tr -d ' ')
if [[ -z $released ]]; then
	echo "error: cannot read releasedStorageSchemaVersions from $upgrade_go" >&2
	exit 1
fi

tags=$(git tag -l 'v[0-9]*' --sort=v:refname)
if [[ -z $tags ]]; then
	echo "error: no release tags found; check out with full history (fetch-depth: 0)" >&2
	exit 1
fi

failed=0
for tag in $tags; do
	version=$(git show "$tag:internal/store/store.go" 2>/dev/null |
		sed -nE 's/^const currentStorageSchemaVersion = ([0-9]+).*/\1/p')
	if [[ -z $version ]]; then
		if [[ $tag != "$first_versioned_tag" ]] && printf '%s\n' "$tag" "$first_versioned_tag" | sort -V -C; then
			continue
		fi
		echo "error: cannot read currentStorageSchemaVersion from $tag:internal/store/store.go" >&2
		failed=1
		continue
	fi
	if [[ ",$released," != *",$version,"* ]]; then
		echo "error: $tag ships storage schema $version, which is missing from releasedStorageSchemaVersions" >&2
		failed=1
	fi
	if ((version < 3)); then
		continue
	fi
	matched=0
	for fixture in "$fixtures_dir"/schema-v*.sql; do
		if git show "$tag:internal/store/schema.sql" | cmp -s - "$fixture"; then
			matched=1
			break
		fi
	done
	if ((matched == 0)); then
		echo "error: $tag schema.sql has no exact fixture; add it as internal/store/testdata/schema-$tag.sql" >&2
		failed=1
	fi
done
exit "$failed"
