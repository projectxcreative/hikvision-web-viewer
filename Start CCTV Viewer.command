#!/bin/bash
# Mac launcher. Double-click to start the viewer; leave this window open while
# you watch, close it (or press Ctrl+C) to stop.
cd "$(dirname "$0")" || exit 1
VERSION=1.9.15
if [ "$(uname -m)" = "arm64" ]; then ARCH=arm64; else ARCH=amd64; fi
URL="http://127.0.0.1:1984/"

if curl -s -o /dev/null "$URL"cfg; then
  echo "Viewer already running - opening it."; open "$URL"; exit 0
fi

if [ ! -x bin/go2rtc-mac ]; then
  echo "First run: downloading go2rtc $VERSION ($ARCH)..."
  curl -fL --progress-bar -o go2rtc.zip \
    "https://github.com/AlexxIT/go2rtc/releases/download/v$VERSION/go2rtc_mac_$ARCH.zip" || { echo "Download failed"; read -r; exit 1; }
  unzip -oq go2rtc.zip go2rtc -d bin && mv bin/go2rtc bin/go2rtc-mac && rm go2rtc.zip && chmod +x bin/go2rtc-mac
fi
rm -f go2rtc 2>/dev/null   # binary from the previous version, no longer used

# files from a downloaded zip are quarantined by macOS; clear that for our own programs
xattr -dr com.apple.quarantine . 2>/dev/null
chmod +x "bin/cctv-server-darwin-$ARCH"

bin/go2rtc-mac -config go2rtc/go2rtc.yaml &
G2R=$!
LISTEN=127.0.0.1:1984 GO2RTC=http://127.0.0.1:1986 "bin/cctv-server-darwin-$ARCH" &
SRV=$!
trap 'kill $G2R $SRV 2>/dev/null' EXIT HUP INT TERM

(sleep 1.5; open "$URL") &
echo "CCTV viewer running at $URL  (close this window to stop)"
wait $SRV
