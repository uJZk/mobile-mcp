#!/bin/sh
# Builds the flashable Magisk / KernelSU module zip into dist/.
set -eu

cd "$(dirname "$0")"
VERSION=$(sed -n 's/^version=v//p' module/module.prop)
STAGE=$(mktemp -d)
trap 'rm -rf "$STAGE"' EXIT

cp -R module/. "$STAGE/"
mkdir -p "$STAGE/bin"

# magisk $ARCH name -> GOARCH (GOARM for 32-bit arm)
for target in arm64:arm64 arm:arm x64:amd64 x86:386; do
  magisk_arch=${target%%:*}
  goarch=${target##*:}
  echo "building $magisk_arch ($goarch)"
  (cd agent && CGO_ENABLED=0 GOOS=linux GOARCH=$goarch GOARM=7 \
    go build -trimpath -ldflags "-s -w" -o "$STAGE/bin/mobile-mcp-agent-$magisk_arch" .)
done

mkdir -p dist
OUT="$PWD/dist/mobile-mcp-agent-v$VERSION.zip"
rm -f "$OUT"
(cd "$STAGE" && zip -qr9 "$OUT" .)
echo "wrote $OUT"
