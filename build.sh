#!/usr/bin/env bash
# 构建 sriov-cni 插件
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
cd "$ROOT"

VERSION="${VERSION:-v0.1.0}"
OUT="${OUT:-$ROOT/build/sriov}"

# 国内环境可换成 goproxy.cn
export GOPROXY="${GOPROXY:-https://goproxy.cn,direct}"
export CGO_ENABLED=0

echo "==> 编译 $VERSION -> $OUT"
mkdir -p "$(dirname "$OUT")"
go build -trimpath -ldflags "-s -w" -o "$OUT" ./cmd/sriov

echo "==> 校验"
file "$OUT"
"$OUT" <<< '{}' >/dev/null 2>&1 || true
echo "VERSION: $(CNI_COMMAND=VERSION "$OUT")"

echo "==> 单元测试"
go test ./...

echo "完成: $OUT"
