#!/usr/bin/env bash
# Build the sriov-cni plugin
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
cd "$ROOT"

VERSION="${VERSION:-v0.1.0}"
OUT="${OUT:-$ROOT/build/sriov}"

# In mainland China, switch to goproxy.cn
export GOPROXY="${GOPROXY:-https://goproxy.cn,direct}"
export CGO_ENABLED=0

echo "==> Building $VERSION -> $OUT"
mkdir -p "$(dirname "$OUT")"
go build -trimpath -ldflags "-s -w" -o "$OUT" ./cmd/sriov

echo "==> Verifying"
file "$OUT"
"$OUT" <<< '{}' >/dev/null 2>&1 || true
echo "VERSION: $(CNI_COMMAND=VERSION "$OUT")"

echo "==> Unit tests"
go test ./...

echo "Done: $OUT"
