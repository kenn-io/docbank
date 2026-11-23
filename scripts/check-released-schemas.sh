#!/usr/bin/env bash
# Fails when a release tag's storage schema has no upgrade path or fixture.
# Every tagged schema version must appear in releasedStorageSchemaVersions.
# From version 3 onward, the first tag to ship each version must match an
# exact released-schema fixture byte for byte. A later tag at the same version
# may only add non-unique indexes: every table, view, trigger, and index in the
# version's fixture must remain unchanged in SQLite's catalog. Tags before
# v0.10.0 have no explicit schema version; every later tag must declare one.
# Requires full tag history (fetch-depth: 0) and the sqlite3 command.
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

if ! command -v sqlite3 >/dev/null; then
	echo "error: sqlite3 is required to compare released schema catalogs" >&2
	exit 1
fi
scratch=$(mktemp -d)
trap 'rm -rf -- "$scratch"' EXIT

# catalog prints one line per schema object: type, name, and its SQL with
# line breaks flattened, in a stable order.
catalog() {
	local db="$scratch/catalog.db"
	rm -f -- "$db"
	sqlite3 -bail "$db" <"$1" >/dev/null
	sqlite3 -separator $'\t' "$db" "SELECT type, name, replace(replace(coalesce(sql, ''), char(13), ' '), char(10), ' ') FROM sqlite_master WHERE name NOT LIKE 'sqlite_%' ORDER BY type, name"
}

failed=0
declare -A version_fixture=()
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
	git show "$tag:internal/store/schema.sql" >"$scratch/tag.sql"
	baseline=${version_fixture[$version]:-}
	if [[ -z $baseline ]]; then
		for fixture in "$fixtures_dir"/schema-v*.sql; do
			if cmp -s "$scratch/tag.sql" "$fixture"; then
				baseline=$fixture
				break
			fi
		done
		if [[ -z $baseline ]]; then
			echo "error: $tag first ships storage schema $version and its schema.sql has no exact fixture; add it as internal/store/testdata/schema-$tag.sql" >&2
			failed=1
			continue
		fi
		version_fixture[$version]=$baseline
		continue
	fi
	catalog "$baseline" | LC_ALL=C sort >"$scratch/baseline.catalog"
	catalog "$scratch/tag.sql" | LC_ALL=C sort >"$scratch/tag.catalog"
	changed=$(LC_ALL=C comm -23 "$scratch/baseline.catalog" "$scratch/tag.catalog" | cut -f1,2)
	added=$(LC_ALL=C comm -13 "$scratch/baseline.catalog" "$scratch/tag.catalog" |
		awk -F '\t' 'toupper($1) != "INDEX" || toupper($3) ~ /^CREATE UNIQUE INDEX/ { print $1 "\t" $2 }')
	if [[ -n $changed || -n $added ]]; then
		echo "error: $tag changes storage schema $version beyond added non-unique indexes; bump currentStorageSchemaVersion" >&2
		[[ -z $changed ]] || sed 's/^/  changed or removed: /' <<<"$changed" >&2
		[[ -z $added ]] || sed 's/^/  added: /' <<<"$added" >&2
		failed=1
	fi
done
exit "$failed"
