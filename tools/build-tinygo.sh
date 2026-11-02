#!/bin/sh
set -eu
cd "$(dirname "$0")/.."
GO_ROOT=${TINYGO_GO_ROOT:-$(go env GOROOT)}
if [ ! -x "$GO_ROOT/bin/go" ]; then
  echo "Set TINYGO_GO_ROOT to a Go toolchain compatible with this TinyGo release" >&2
  exit 1
fi
mkdir -p build
TINYGO_GC=${TINYGO_GC:-leaking}
PATH="$GO_ROOT/bin:$PATH" GOROOT="$GO_ROOT" GOOS=linux GOARCH=arm64 \
  tinygo build -gc="$TINYGO_GC" -no-debug -o build/wmeshd ./cmd/wmeshd
file build/wmeshd
