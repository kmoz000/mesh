#!/bin/sh
# mkstubs.sh — regenerate link-time stub shared objects for the device's native
# libraries so a cgo build can link against them on a non-musl host.
#
# Why stubs: the vendor .so files pulled from the T2 (QSDK/OpenWrt musl) ship
# with their ELF *section header table stripped* (`file` reports "no section
# header"). zig's linker (ld.lld) resolves a shared object's exported symbols
# through the section-based .dynsym, which is absent, so it sees zero symbols and
# every reference is "undefined" — even though `nm -D` (which reads the dynamic
# segment) lists them. The fix is standard cross-compilation practice: link
# against a stub .so that carries the same SONAME and the same exported symbol
# names but has real section headers. At runtime the device's real library
# satisfies the symbols; the stub is never deployed.
#
# Usage: tools/mkstubs.sh [sysroot]   (default sysroot: ./vendor-sysroot)
# Produces <sysroot>/stublib/*.so(.N) + link-name symlinks. Requires the aarch64
# binutils (nm/readelf) and zig on PATH. Re-run whenever vendor-sysroot changes.
set -e
SR="${1:-$(cd "$(dirname "$0")/.." && pwd)/vendor-sysroot}"
NM="${NM:-aarch64-linux-gnu-nm}"
READELF="${READELF:-aarch64-linux-gnu-readelf}"
ZIG="${ZIG:-zig}"
STUB="$SR/stublib"
mkdir -p "$STUB"

mkstub() { # $1 = real .so path
  real="$1"
  base="$(basename "$real")"
  soname="$("$READELF" -d "$real" 2>/dev/null | awk -F'[][]' '/SONAME/{print $2}')"
  [ -n "$soname" ] || soname="$base"
  tmp="$(mktemp "${TMPDIR:-/tmp}/wmstub.XXXXXX.c")"
  "$NM" -D --defined-only "$real" 2>/dev/null \
    | awk '$2 ~ /^[TWiBDR]$/ {n=$3; sub(/@@.*/,"",n); if(n!="_init"&&n!="_fini") print "void "n"(void){}"}' \
    | sort -u > "$tmp"
  "$ZIG" cc -target aarch64-linux-musl -shared -nostdlib -fPIC \
    -Wl,-soname,"$soname" "$tmp" -o "$STUB/$soname"
  rm -f "$tmp"
  # link-name symlink: lib<x>.so -> lib<x>.so.N so `-l<x>` resolves.
  link="$(printf '%s' "$soname" | sed 's/\.so\..*/.so/')"
  [ "$link" = "$soname" ] || ln -sf "$soname" "$STUB/$link"
  echo "  stub $soname (from $base)"
}

echo "sysroot: $SR"
for so in \
  "$SR"/usr/lib/libnl-3.so.200 \
  "$SR"/usr/lib/libnl-genl-3.so.200 \
  "$SR"/usr/lib/libhyfi-bridge.so \
  "$SR"/usr/lib/libhyficommon.so \
  "$SR"/usr/lib/libieee1905.so \
  "$SR"/usr/lib/libieee1905layer.so \
  "$SR"/usr/lib/libqca_wifison_ext.so \
  "$SR"/usr/lib/libqca_tools.so \
  "$SR"/usr/lib/libacfg.so ; do
  [ -f "$so" ] && mkstub "$so" || echo "  skip (absent): $so"
done
echo "stubs written to $STUB"
