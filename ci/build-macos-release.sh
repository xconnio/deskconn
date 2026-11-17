#!/usr/bin/env bash
set -euo pipefail
cd "$(dirname "$0")/.."

if [[ "$(uname -s)" != Darwin || "$(uname -m)" != arm64 ]]; then
  echo 'Build macOS releases on an Apple Silicon macOS runner' >&2
  exit 1
fi

: "${XCONN_RELEASE_SDK:?SDK directory required}"
destination=${1:?Usage: build-macos-release.sh DIRECTORY [TAG]}
version=${2:-v0.1.0-beta.$(git rev-parse --short=7 HEAD)}
if [[ ! "$version" =~ ^v[0-9][A-Za-z0-9.+-]*$ ]]; then
  echo 'Invalid release tag' >&2
  exit 1
fi
mkdir -p "$destination"
destination=$(cd "$destination" && pwd)
export PKG_CONFIG_PATH="$XCONN_RELEASE_SDK/darwin_arm64/lib/pkgconfig"
export CGO_ENABLED=1
export GOOS=darwin
export GOARCH=arm64
export CC=clang

build=$(mktemp -d)
trap 'rm -rf "$build"' EXIT
go build -tags xconn_static -ldflags "-s -w -X main.version=$version -linkmode external" \
  -o "$build/deskconn" ./cmd/deskconn
lipo "$build/deskconn" -verify_arch arm64
codesign --force --sign - "$build/deskconn"
codesign --verify --strict "$build/deskconn"
# XConn is linked in; only Apple system libraries may remain external.
if otool -L "$build/deskconn" | tail -n +2 | awk '{print $1}' | grep -Ev '^(/usr/lib/|/System/Library/)'; then
  echo 'macOS release must not depend on XConn or Homebrew shared libraries' >&2
  exit 1
fi
"$build/deskconn" --help >/dev/null
cp README.md LICENSE "$build/"
tar -czf "$destination/deskconn_${version#v}_darwin_arm64.tar.gz" -C "$build" deskconn README.md LICENSE
