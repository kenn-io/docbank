#!/bin/sh
# Install the latest docbank release on Linux or macOS.
# Usage: curl -fsSL https://raw.githubusercontent.com/kenn-io/docbank/main/scripts/install.sh | sh

set -eu

repo="kenn-io/docbank"
binary_name="docbank"

info() { printf '%s\n' "$1"; }
fail() { printf 'ERROR: %s\n' "$1" >&2; exit 1; }

detect_os() {
    case "$(uname -s)" in
        Darwin) printf 'darwin\n' ;;
        Linux) printf 'linux\n' ;;
        *) fail "unsupported operating system: $(uname -s)" ;;
    esac
}

detect_arch() {
    case "$(uname -m)" in
        x86_64|amd64) printf 'amd64\n' ;;
        aarch64|arm64) printf 'arm64\n' ;;
        *) fail "unsupported architecture: $(uname -m)" ;;
    esac
}

download() {
    url=$1
    output=$2
    if command -v curl >/dev/null 2>&1; then
        curl -fsSL "$url" -o "$output"
    elif command -v wget >/dev/null 2>&1; then
        wget -q "$url" -O "$output"
    else
        fail "curl or wget is required"
    fi
}

latest_version() {
    if [ -n "${DOCBANK_VERSION:-}" ]; then
        printf '%s\n' "$DOCBANK_VERSION"
        return
    fi

    latest_url="https://github.com/${repo}/releases/latest"
    if command -v curl >/dev/null 2>&1; then
        final_url=$(curl -fsSLI -o /dev/null -w '%{url_effective}' "$latest_url") || return 1
    elif command -v wget >/dev/null 2>&1; then
        final_url=$(wget --spider -S "$latest_url" 2>&1 \
            | awk 'tolower($1) == "location:" { print $2 }' \
            | tail -1 \
            | tr -d '\r\n') || return 1
    else
        fail "curl or wget is required"
    fi
    case "$final_url" in
        */releases/tag/*) printf '%s\n' "${final_url##*/releases/tag/}" ;;
        *) return 1 ;;
    esac
}

checksum_for() {
    checksums=$1
    filename=$2
    awk -v wanted="$filename" '
        NF >= 2 {
            name = $2
            sub(/^\*/, "", name)
            sub(/^\.\//, "", name)
            if (name == wanted) {
                count++
                hash = $1
            }
        }
        END {
            if (count != 1) exit 1
            print hash
        }
    ' "$checksums"
}

verify_checksum() {
    archive=$1
    checksums=$2
    filename=$3

    expected=$(checksum_for "$checksums" "$filename") || \
        fail "SHA256SUMS must contain exactly one entry for ${filename}"
    case "$expected" in
        *[!0-9a-fA-F]*|'') fail "invalid SHA-256 value for ${filename}" ;;
    esac
    [ "${#expected}" -eq 64 ] || fail "invalid SHA-256 value for ${filename}"

    if command -v sha256sum >/dev/null 2>&1; then
        actual=$(sha256sum "$archive" | awk '{ print $1 }')
    elif command -v shasum >/dev/null 2>&1; then
        actual=$(shasum -a 256 "$archive" | awk '{ print $1 }')
    else
        fail "sha256sum or shasum is required; refusing an unverified install"
    fi
    expected=$(printf '%s' "$expected" | tr 'A-F' 'a-f')
    actual=$(printf '%s' "$actual" | tr 'A-F' 'a-f')
    [ "$actual" = "$expected" ] || \
        fail "checksum mismatch for ${filename} (expected ${expected}, got ${actual})"
    info "Checksum verified."
}

install_docbank() {
    os=$(detect_os)
    arch=$(detect_arch)
    version=$(latest_version) || fail "could not resolve the latest GitHub release"
    printf '%s\n' "$version" | awk '/^v[0-9]+\.[0-9]+\.[0-9]+$/ { valid = 1 } END { exit !valid }' || \
        fail "release tag is not vX.Y.Z: ${version}"

    version_number=${version#v}
    filename="docbank_${version_number}_${os}_${arch}.tar.gz"
    base_url=${DOCBANK_RELEASE_BASE_URL:-"https://github.com/${repo}/releases/download/${version}"}
    base_url=${base_url%/}
    install_dir=${DOCBANK_INSTALL_DIR:-"$HOME/.local/bin"}
    mkdir -p "$install_dir"
    [ -d "$install_dir" ] || fail "install destination is not a directory: ${install_dir}"
    [ -w "$install_dir" ] || fail "install destination is not writable: ${install_dir}"

    tmpdir=$(mktemp -d "${TMPDIR:-/tmp}/docbank-install.XXXXXX") || \
        fail "could not create a temporary directory"
    trap 'rm -rf "$tmpdir"' EXIT HUP INT TERM

    archive="$tmpdir/$filename"
    checksums="$tmpdir/SHA256SUMS"
    info "Installing docbank ${version} for ${os}/${arch}..."
    download "${base_url}/${filename}" "$archive" || fail "could not download ${filename}"
    download "${base_url}/SHA256SUMS" "$checksums" || \
        fail "could not download SHA256SUMS; refusing an unverified install"
    verify_checksum "$archive" "$checksums" "$filename"

    entries=$(tar -tzf "$archive") || fail "could not read ${filename}"
    [ "$entries" = "$binary_name" ] || \
        fail "release archive must contain only ${binary_name} at its root"
    tar -xzf "$archive" -C "$tmpdir"
    [ -f "$tmpdir/$binary_name" ] || fail "release archive does not contain ${binary_name}"

    staged="$install_dir/.docbank-install-$$"
    trap 'rm -rf "$tmpdir"; rm -f "$staged"' EXIT HUP INT TERM
    cp "$tmpdir/$binary_name" "$staged"
    chmod 0755 "$staged"
    mv -f "$staged" "$install_dir/$binary_name"

    if [ "$os" = "darwin" ] && command -v codesign >/dev/null 2>&1; then
        codesign -s - "$install_dir/$binary_name" >/dev/null 2>&1 || true
    fi

    info "Installed ${install_dir}/${binary_name}"
    case ":$PATH:" in
        *":$install_dir:"*) ;;
        *) info "Add ${install_dir} to PATH: export PATH=\"${install_dir}:\$PATH\"" ;;
    esac
    info "Get started: docbank add ~/Documents --dest /archive"
}

install_docbank
