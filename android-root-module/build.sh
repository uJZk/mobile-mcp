#!/bin/sh
# Builds the flashable Magisk / KernelSU module zip into dist/.
#
# Needs Go 1.22+, a JDK (javac/java 11+), zip and curl. The Android compile
# stubs and d8 are downloaded once into .cache/ unless ANDROID_JAR / R8_JAR
# point at local copies.
set -eu

cd "$(dirname "$0")"
VERSION=$(sed -n 's/^version=v//p' module/module.prop)
STAGE=$(mktemp -d)
trap 'rm -rf "$STAGE"' EXIT

ANDROID_JAR_URL=https://repo1.maven.org/maven2/com/google/android/android/4.1.1.4/android-4.1.1.4.jar
ANDROID_JAR_SHA256=84072541cbb711eff89f7277100ff854929a446dba7ceb1b195c340e0b4fd3cb
R8_JAR_URL=https://dl.google.com/android/maven2/com/android/tools/r8/8.13.25/r8-8.13.25.jar
R8_JAR_SHA256=0d4f2f68c4fcbc9434eff122f41feb6fb1d98efd2faa530cec1abacab1cea03d

# fetch URL SHA256 -> prints the cached path
fetch() {
  mkdir -p .cache
  dest=.cache/$(basename "$1")
  if [ ! -f "$dest" ]; then
    curl -sSfL -o "$dest.part" "$1"
    mv "$dest.part" "$dest"
  fi
  if [ "$(sha256sum "$dest" | cut -d' ' -f1)" != "$2" ]; then
    rm -f "$dest"
    echo "checksum mismatch for $1" >&2
    exit 1
  fi
  echo "$dest"
}

ANDROID_JAR=${ANDROID_JAR:-$(fetch "$ANDROID_JAR_URL" "$ANDROID_JAR_SHA256")}
R8_JAR=${R8_JAR:-$(fetch "$R8_JAR_URL" "$R8_JAR_SHA256")}

cp -R module/. "$STAGE/"
mkdir -p "$STAGE/bin" "$STAGE/lib" "$STAGE/classes"

echo "building input-server"
javac -nowarn -Xlint:-options --release 8 -encoding UTF-8 -cp "$ANDROID_JAR" -d "$STAGE/classes" \
  $(find input-server/src -name '*.java')
java -cp "$R8_JAR" com.android.tools.r8.D8 --release --min-api 23 --lib "$ANDROID_JAR" \
  --output "$STAGE/lib/input-server.jar" $(find "$STAGE/classes" -name '*.class')
rm -rf "$STAGE/classes"

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
