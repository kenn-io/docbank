#!/usr/bin/env bash
# Run the complete screenshot capture inside frontend/screenshots/Dockerfile and
# write the published docs-assets set to OUTPUT_DIR/assets. The host needs only
# Bash and Docker.
set -euo pipefail

output="${1:?usage: run-in-docker.sh OUTPUT_DIR}"
script_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
repo_root="$(cd "$script_dir/../.." && pwd)"
image=docbank-docs-screenshots

mkdir -p "$output"
output="$(cd "$output" && pwd -P)"
docker build -t "$image" "$script_dir"
# The daemon looks up the effective user, so give the caller's IDs an account.
# Keep it in the checkout, which the Docker daemon can always see.
accounts="$(mktemp -d "$repo_root/.screenshot-accounts.XXXXXX")"
trap 'rm -rf "$accounts"' EXIT
printf 'root:x:0:0:root:/root:/bin/sh\ndocbank:x:%s:%s:docbank:/tmp:/bin/bash\n' "$(id -u)" "$(id -g)" >"$accounts/passwd"
printf 'root:x:0:\ndocbank:x:%s:\n' "$(id -g)" >"$accounts/group"
# Run as the caller so every generated file stays removable without root.
docker run --rm --init --user "$(id -u):$(id -g)" -e HOME=/tmp -e DOCBANK_TELEMETRY_ENABLED=0 \
    -v "$accounts/passwd:/etc/passwd:ro" -v "$accounts/group:/etc/group:ro" \
    -v "$repo_root:/src" -v "$output:/out" -w /src "$image" \
    bash -c 'cd frontend && npm ci && cd .. && make docs-screenshots && mkdir -p /out/assets && cp -R .superpowers/screenshots/. /out/assets/'
