#!/bin/sh
# Build dd-pits.app (Apple Silicon + Intel in one) and package it as a disk
# image with an Applications shortcut to drag it onto. Run on a Mac.
#   packaging/macos/build-app.sh <version> <out-dir>
set -eu
VERSION=${1:-dev}
OUT=${2:-dist}
ROOT=$(cd "$(dirname "$0")/../.." && pwd)
WORK=$(mktemp -d)
COMMIT=$(git -C "$ROOT" rev-parse --short HEAD 2>/dev/null || echo unknown)
for arch in arm64 amd64; do
  (cd "$ROOT" && CGO_ENABLED=0 GOOS=darwin GOARCH=$arch go build -trimpath \
    -ldflags "-s -w -X main.version=$VERSION -X main.commit=$COMMIT" -o "$WORK/dd-pits-$arch" ./cmd/dd-pits)
done
APP="$WORK/dd-pits.app"
mkdir -p "$APP/Contents/MacOS" "$APP/Contents/Resources"
lipo -create -output "$APP/Contents/MacOS/dd-pits" "$WORK/dd-pits-arm64" "$WORK/dd-pits-amd64"
cp "$ROOT/packaging/macos/launcher.sh" "$APP/Contents/MacOS/dd-pits-launcher"
chmod +x "$APP/Contents/MacOS/dd-pits" "$APP/Contents/MacOS/dd-pits-launcher"
sed "s/VERSION/${VERSION#v}/g" "$ROOT/packaging/macos/Info.plist" > "$APP/Contents/Info.plist"
# Icon from the app's 256px icon.
ICONSET="$WORK/dd-pits.iconset"; mkdir -p "$ICONSET"
sips -s format png "$ROOT/cmd/dd-pits/assets/icon-256.webp" --out "$WORK/icon.png" >/dev/null
for s in 16 32 64 128 256; do
  sips -z $s $s "$WORK/icon.png" --out "$ICONSET/icon_${s}x${s}.png" >/dev/null
done
for s in 16 32 128; do
  d=$((s * 2)); sips -z $d $d "$WORK/icon.png" --out "$ICONSET/icon_${s}x${s}@2x.png" >/dev/null
done
cp "$ICONSET/icon_256x256.png" "$ICONSET/icon_256x256@2x.png"
iconutil -c icns "$ICONSET" -o "$APP/Contents/Resources/dd-pits.icns"
# Ad-hoc signed: not notarised, so the first open needs Open Anyway.
codesign --force --deep --sign - "$APP"
mkdir -p "$OUT"
# The disk image: the app beside an Applications shortcut.
STAGE="$WORK/dmg"; mkdir -p "$STAGE"
cp -R "$APP" "$STAGE/"
ln -s /Applications "$STAGE/Applications"
rm -f "$OUT/dd-pits-macos.dmg"
hdiutil create -quiet -volname "dd-pits" -srcfolder "$STAGE" -fs HFS+ -format UDZO -ov "$OUT/dd-pits-macos.dmg"
codesign --force --sign - "$OUT/dd-pits-macos.dmg"
echo "$OUT/dd-pits-macos.dmg"
