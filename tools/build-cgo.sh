#!/bin/sh
# build-cgo.sh — build wmeshd for the T2 (aarch64, musl) as a standard-Go cgo
# binary that binds the device's native vendor libraries (libnl-genl-3, hostapd
# control socket, and the QCA hyfi-bridge / ieee1905 libs).
#
# This replaces the TinyGo build for on-device use. Standard Go gives a real
# garbage collector (closing the leaking-GC risk) and first-class cgo; the binary
# is ~a few MB, negligible on an IPQ9574. The cross C compiler is `zig cc`
# targeting aarch64-linux-musl (see tools/zcc); linking is against the stub .so
# set produced by tools/mkstubs.sh. At runtime the device's real libraries
# satisfy the dynamic symbols.
#
# Prereqs: a Go toolchain (GO env, default `go`), zig on PATH, the aarch64
# binutils (for mkstubs), and ./vendor-sysroot populated from the device
# (lib/libc.so, usr/lib/libnl*, the vendor .so). Run tools/mkstubs.sh once after
# refreshing vendor-sysroot.
#
# Usage: tools/build-cgo.sh [output-path]   (default: build/wmeshd-aarch64)
set -e
ROOT="$(cd "$(dirname "$0")/.." && pwd)"
cd "$ROOT"
OUT="${1:-build/wmeshd-aarch64}"
GO="${GO:-go}"
SR="${SYSROOT:-$ROOT/vendor-sysroot}"
STUB="$SR/stublib"
mkdir -p "$(dirname "$OUT")"

[ -d "$SR/usr/lib" ] || { echo "missing sysroot $SR (pull device libs first)"; exit 1; }
[ -d "$STUB" ] || { echo "stublib missing; running mkstubs.sh"; "$ROOT/tools/mkstubs.sh" "$SR"; }

export CGO_ENABLED=1 GOOS=linux GOARCH=arm64
export CC="$ROOT/tools/zcc"
# Link against stubs; -lm/-lpthread come from musl. Order matters: genl before 3.
export CGO_LDFLAGS="-L$STUB -lnl-genl-3 -lnl-3"

echo "building $OUT (aarch64/musl, cgo, stublib=$STUB)"
"$GO" build -trimpath -ldflags "-s -w" -o "$OUT" ./cmd/wmeshd
echo "built: $OUT"
command -v aarch64-linux-gnu-readelf >/dev/null 2>&1 && {
  echo "interpreter:"; aarch64-linux-gnu-readelf -l "$OUT" | grep -i interpreter
  echo "NEEDED:"; aarch64-linux-gnu-readelf -d "$OUT" | awk -F'[][]' '/NEEDED/{print "  "$2}'
}
