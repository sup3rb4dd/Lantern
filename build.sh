#!/usr/bin/env bash
# Builds Lantern.exe and assembles a ready-to-copy drive folder in dist/Lantern.
# Needs Go 1.22+ (https://go.dev/dl). Works on Linux, macOS, or Git Bash on Windows.
set -euo pipefail
cd "$(dirname "$0")"
OUT=dist/Lantern
rm -rf dist && mkdir -p "$OUT/lantern/web" "$OUT/lantern/source"
(cd launcher && GOOS=windows GOARCH=amd64 CGO_ENABLED=0 \
  go build -trimpath -ldflags "-s -w" -o "../$OUT/Lantern.exe" .)
cp -r web/. "$OUT/lantern/web/"
cp launcher/*.go launcher/go.mod RELAY.md "$OUT/lantern/source/"
cp package/README.txt "$OUT/README.txt"
VERSION=$(grep -oE 'version = "[^"]+"' launcher/main.go | cut -d'"' -f2)
(cd dist && zip -qr "Lantern-v$VERSION.zip" Lantern)
echo "Built dist/Lantern and dist/Lantern-v$VERSION.zip"
