#!/bin/bash
# Builds cctv-server for macOS and Linux into bin/, and a ready-to-run zip into dist/.
# Needs Go 1.26+.  Usage: scripts/build.sh [version]
set -euo pipefail
cd "$(dirname "$0")/.."
VERSION=${1:-dev}
mkdir -p bin dist
for t in darwin/arm64 darwin/amd64 linux/amd64 linux/arm64; do
  os=${t%/*}; arch=${t#*/}
  echo "building $os/$arch"
  (cd server && CGO_ENABLED=0 GOOS=$os GOARCH=$arch go build -trimpath -ldflags="-s -w" -o "../bin/cctv-server-$os-$arch" .)
done
NAME=hikvision-web-viewer-$VERSION
rm -rf "dist/$NAME" "dist/$NAME.zip"
mkdir -p "dist/$NAME/data"
cp -R bin www go2rtc docker-compose.yml "Start CCTV Viewer.command" README.md LICENSE "dist/$NAME/"
touch "dist/$NAME/data/.gitkeep"
(cd dist && zip -qr9 "$NAME.zip" "$NAME")
echo "dist/$NAME.zip"
