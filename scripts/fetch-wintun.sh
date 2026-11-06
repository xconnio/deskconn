#!/bin/sh
# Downloads the Wintun driver that goreleaser bundles into the Windows archives, into
# build/wintun. Its checksum is pinned, so a changed download fails the release.
set -eu

version=0.14.1
sha256=07c256185d6ee3652e09fa55c0b673e2624b565e02c4b9091c79ca7d2f24ef51

mkdir -p build
zip="build/wintun-$version.zip"
curl -fsSL "https://www.wintun.net/builds/wintun-$version.zip" -o "$zip"
echo "$sha256  $zip" | sha256sum -c -

rm -rf build/wintun
unzip -q "$zip" -d build
rm -f "$zip"
