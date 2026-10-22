#!/bin/sh
set -eu

script_dir=$(CDPATH='' cd -- "$(dirname -- "$0")" && pwd)
repo_root=$(CDPATH='' cd -- "$script_dir/.." && pwd)
tools_root="$repo_root/.vercel-tools"
uv_version=0.13.4

if [ "$(uname -s)" != "Linux" ]; then
  printf 'Vercel documentation install requires Linux\n' >&2
  exit 1
fi

case "$(uname -m)" in
  x86_64 | amd64)
    target=x86_64-unknown-linux-gnu
    expected_sha=2b428396f12572d4429cf210ca234dd599a33b3af0958a4d5ed569d63e3f344f
    ;;
  aarch64 | arm64)
    target=aarch64-unknown-linux-gnu
    expected_sha=1ae72b4718729e2ccc4f1ae2c6d1e531ca8467800e0546e9130b3886023a60ab
    ;;
  *)
    printf 'unsupported Vercel build architecture: %s\n' "$(uname -m)" >&2
    exit 1
    ;;
esac

scratch=$(mktemp -d)
trap 'rm -rf -- "$scratch"' EXIT HUP INT TERM
archive="$scratch/uv.tar.gz"
url="https://github.com/astral-sh/uv/releases/download/$uv_version/uv-$target.tar.gz"

curl --fail --location --silent --show-error "$url" --output "$archive"
printf '%s  %s\n' "$expected_sha" "$archive" | sha256sum -c -
tar -xzf "$archive" -C "$scratch"

mkdir -p "$tools_root/bin"
install -m 0755 "$scratch/uv-$target/uv" "$tools_root/bin/uv"
install -m 0755 "$scratch/uv-$target/uvx" "$tools_root/bin/uvx"
PATH="$tools_root/bin:$PATH" uv sync --project "$repo_root/docs" --frozen
