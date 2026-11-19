#!/usr/bin/env bash
# Run a test command with TMPDIR on Linux's RAM-backed /dev/shm. Tests sync
# many small files, and on a disk each sync waits for the device. The tests
# still call every sync; only the storage behind them is faster.
#
# TMPDIR is the same directory in every run, because go test reuses a cached
# result only when the environment a test read is unchanged. Each test removes
# its own temporary directory, so concurrent runs can share it. The directory
# must belong to the current user and be private to it.
#
# Hosts without /dev/shm, with less than 8 GiB free there, or where it cannot
# run programs keep the default TMPDIR. A full parallel run holds a few GiB of
# temporary files at once, and tests run helpers from temporary directories.
#
# Test processes of one run also share compiled PDFium code through
# DOCBANK_TEST_WASM_CACHE_DIR, a new directory that is removed after the run.
set -euo pipefail

free_kib=0
if [ -d /dev/shm ] && [ -w /dev/shm ]; then
	free_kib=$(df -Pk /dev/shm | awk 'NR == 2 { print $4 }')
fi
if [ "${free_kib:-0}" -ge $((8 * 1024 * 1024)) ]; then
	# Keep the name short: tests create Unix sockets below TMPDIR, and socket
	# paths are limited to about 100 bytes.
	dir="/dev/shm/dbt-$(id -u)"
	mkdir -p -m 700 -- "$dir" 2>/dev/null || true
	if [ -d "$dir" ] && [ ! -L "$dir" ] && [ -O "$dir" ] && [ "$(stat -c %a -- "$dir")" = 700 ]; then
		probe=$(mktemp "$dir/exec-probe.XXXXXX")
		printf '#!/bin/sh\n' >"$probe"
		chmod 700 "$probe"
		if "$probe"; then
			export TMPDIR="$dir"
		fi
		rm -f -- "$probe"
	fi
fi

cache=$(mktemp -d "${TMPDIR:-/tmp}/wasm.XXXXXX")
trap 'rm -rf -- "$cache"' EXIT
export DOCBANK_TEST_WASM_CACHE_DIR="$cache"
"$@"
