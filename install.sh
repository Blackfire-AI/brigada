#!/bin/sh
set -eu

fail() {
    printf 'brigada installer: %s\n' "$*" >&2
    exit 1
}

# Arguments let tests exercise every platform without depending on the host.
detect_platform() {
    case "$1" in
        Linux) os=linux ;;
        Darwin) os=darwin ;;
        *) fail "unsupported OS: $1 (supported: Linux, Darwin)" ;;
    esac
    case "$2" in
        x86_64|amd64) arch=amd64 ;;
        aarch64|arm64) arch=arm64 ;;
        *) fail "unsupported CPU: $2 (supported: amd64, arm64)" ;;
    esac
    printf '%s-%s\n' "$os" "$arch"
}

download() {
    if command -v curl >/dev/null 2>&1; then
        curl -fsSL -o "$2" "$1" || fail "could not download $1"
    elif command -v wget >/dev/null 2>&1; then
        wget -q -O "$2" "$1" || fail "could not download $1"
    else
        fail 'curl or wget is required'
    fi
}

verify_checksum() {
    expected=
    while read -r checksum filename rest; do
        if [ "${filename#\*}" = "$2" ]; then
            [ -z "$expected" ] || fail "duplicate checksum for $2"
            expected=$checksum
        fi
    done < "$3"
    [ -n "$expected" ] || fail "missing checksum for $2"
    if command -v sha256sum >/dev/null 2>&1; then
        actual=$(sha256sum < "$1")
    elif command -v shasum >/dev/null 2>&1; then
        actual=$(shasum -a 256 < "$1")
    else
        fail 'sha256sum or shasum is required'
    fi
    actual=${actual%% *}
    [ "$actual" = "$expected" ] || fail "checksum mismatch for $2; refusing to install"
}

main() {
    platform=$(detect_platform "$(uname -s)" "$(uname -m)")
    asset="brigada-${platform}.tar.gz"
    # Override the releases root for offline fixture servers.
    base_url=${BRIGADA_BASE_URL:-https://github.com/Blackfire-AI/brigada/releases}
    if [ -n "${BRIGADA_VERSION:-}" ]; then
        release_url="${base_url%/}/download/v${BRIGADA_VERSION#v}"
    else
        release_url="${base_url%/}/latest/download"
    fi
    install_dir=${BRIGADA_INSTALL_DIR:-"$HOME/.local/bin"}
    tmp_dir=$(mktemp -d)
    trap 'rm -rf "$tmp_dir"' EXIT
    trap 'exit 1' HUP INT TERM
    download "$release_url/$asset" "$tmp_dir/$asset"
    download "$release_url/SHA256SUMS" "$tmp_dir/SHA256SUMS"
    verify_checksum "$tmp_dir/$asset" "$asset" "$tmp_dir/SHA256SUMS"
    tar -xzf "$tmp_dir/$asset" -C "$tmp_dir" brigada
    chmod 755 "$tmp_dir/brigada"
    mkdir -p "$install_dir"
    mv "$tmp_dir/brigada" "$install_dir/brigada"
    printf 'Installed brigada to %s\n' "$install_dir/brigada"
    case ":$PATH:" in
        *":$install_dir:"*) ;;
        *) printf '%s is not on PATH; add it to PATH in your shell configuration.\n' "$install_dir" ;;
    esac
    "$install_dir/brigada" version
}

# Source functions in tests without downloading or installing anything.
if [ "${BRIGADA_INSTALL_SOURCE_ONLY:-0}" != 1 ]; then
    main
fi
