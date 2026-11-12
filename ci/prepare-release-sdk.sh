#!/usr/bin/env bash
set -euo pipefail

version=0.1.6
destination=${1:?Usage: prepare-release-sdk.sh DIRECTORY [linux_amd64|linux_arm64|windows_amd64]}
mkdir -p "$destination"
destination=$(cd "$destination" && pwd)
base=https://github.com/xconnio/xconn-core/releases/download/v$version

for platform in ${2:-linux_amd64 linux_arm64 windows_amd64}; do
  case "$platform" in
    linux_amd64) target=x86_64-unknown-linux-gnu ;;
    linux_arm64) target=aarch64-unknown-linux-gnu ;;
    windows_amd64) target=x86_64-pc-windows-gnu ;;
    *) echo "Unsupported platform: $platform" >&2; exit 1 ;;
  esac
  name=xconn-native-$version-$target-static
  temporary=$(mktemp -d)
  trap 'rm -rf "$temporary"' EXIT
  curl -fLsS "$base/$name.tar.gz" -o "$temporary/$name.tar.gz"
  curl -fLsS "$base/$name.tar.gz.sha256" -o "$temporary/$name.tar.gz.sha256"
  (cd "$temporary" && sha256sum --check "$name.tar.gz.sha256")
  tar -xzf "$temporary/$name.tar.gz" -C "$temporary"
  mkdir -p "$destination/$platform"
  cp -R "$temporary/$name/." "$destination/$platform/"
  rm -rf "$temporary"
done
