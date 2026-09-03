#!/usr/bin/env bash
# 本地质量门禁（**仅 Go 侧**）：格式化检查 -> vet -> build -> test
#
# 注意：完整门禁请用 `make check` —— 它还包含 biome、pi/dsh 两侧 TS 单测与
# 二进制构建。本脚本是改 Go 时的快速回路，不是项目的验收入面。
# 用法：scripts/check.sh          （全量）
#       scripts/check.sh -run TestXxx
set -euo pipefail

cd "$(dirname "$0")/.." || exit 1

GO="${GO:-go}"
args=("$@")

echo "== gofmt =="
bad="$("$GO" fmt ./... 2>/dev/null || true)"
if [ -n "$bad" ]; then
  echo "已自动格式化以下文件:"
  echo "$bad"
fi

echo "== go vet =="
"$GO" vet ./...

echo "== go build =="
"$GO" build ./...

echo "== go test =="
if [ ${#args[@]} -gt 0 ]; then
  "$GO" test -race ./... "${args[@]}"
else
  "$GO" test -race ./...
fi

echo "GO 侧检查通过（TS 侧未跑；完整门禁用 make check）"