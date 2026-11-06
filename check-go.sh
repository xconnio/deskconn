#!/bin/sh
# Makes sure a Go that can build this repo is available, offering to install the version
# go.mod asks for if it isn't. Run by the Makefile before anything that needs Go.
set -e

GO_DIR="/usr/local/go"
REQUIRED="$(awk '$1 == "go" { print $2; exit }' go.mod)"

# "go version" run here reports the toolchain go.mod selects: Go 1.21+ downloads a newer
# one by itself, so only a missing Go, an older one, or GOTOOLCHAIN=local falls through.
current_version() {
    go version 2>/dev/null | awk '{ print $3 }' | sed 's/^go//'
}

# Exits non-zero unless version $1 is at least $REQUIRED.
meets_required() {
    [ -n "$1" ] && awk -v have="$1" -v want="$REQUIRED" 'BEGIN {
        split(have, h, "."); split(want, w, ".")
        for (i = 1; i <= 3; i++) {
            if (h[i] + 0 > w[i] + 0) exit 0
            if (h[i] + 0 < w[i] + 0) exit 1
        }
    }'
}

CURRENT="$(current_version)"
meets_required "$CURRENT" && exit 0

if [ -n "$CURRENT" ]; then
    echo "Go $REQUIRED is required, but found Go $CURRENT."
else
    echo "Go $REQUIRED is required, but Go was not found."
fi

case "$(uname -s)" in
    Darwin) OS="darwin" ;;
    Linux) OS="linux" ;;
    *)
        echo "Install it from https://go.dev/dl/"
        exit 1
        ;;
esac

case "$(uname -m)" in
    x86_64) GO_ARCH="amd64" ;;
    aarch64 | arm64) GO_ARCH="arm64" ;;
    *)
        echo "Install it from https://go.dev/dl/"
        exit 1
        ;;
esac

if [ ! -t 0 ]; then
    echo "Install it from https://go.dev/dl/"
    exit 1
fi

printf 'Install Go %s to %s? This needs sudo and replaces any Go already there. [y/N] ' "$REQUIRED" "$GO_DIR"
read -r answer
case "$answer" in
    y | Y | yes | YES) ;;
    *)
        echo "Install it from https://go.dev/dl/"
        exit 1
        ;;
esac

TMP_DIR="$(mktemp -d)"
trap 'rm -rf "$TMP_DIR"' EXIT

if [ "$OS" = "darwin" ]; then
    ARCHIVE="go$REQUIRED.darwin-$GO_ARCH.pkg"
else
    ARCHIVE="go$REQUIRED.linux-$GO_ARCH.tar.gz"
fi

echo "Downloading $ARCHIVE..."
curl -fL "https://go.dev/dl/$ARCHIVE" -o "$TMP_DIR/$ARCHIVE"

if [ "$OS" = "darwin" ]; then
    sudo installer -pkg "$TMP_DIR/$ARCHIVE" -target /
else
    sudo rm -rf "$GO_DIR"
    sudo tar -C "$(dirname "$GO_DIR")" -xzf "$TMP_DIR/$ARCHIVE"
fi

PATH="$PATH:$GO_DIR/bin"
CURRENT="$(current_version)"
if ! meets_required "$CURRENT"; then
    echo "Installed Go $REQUIRED to $GO_DIR, but the go on your PATH reports version '$CURRENT': $(command -v go || echo 'not found')"
    exit 1
fi

echo "Installed Go $REQUIRED. Open a new terminal, or add $GO_DIR/bin to your PATH, to use it outside make."
