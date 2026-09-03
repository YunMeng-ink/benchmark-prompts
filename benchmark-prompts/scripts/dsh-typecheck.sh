#!/usr/bin/env bash
# dsh-typecheck.sh —— 用 DSH 安装树自带的 typescript 对本插件做 noEmit 类型检查。
# 解析策略：在插件目录临时挂一个 junction 到 ~/.dsh/profiles/node_modules
# （与装载进 DSH 时的真实解析路径同构），检查完安全摘除（rmdir 只删链接）。
# 用法：bash scripts/dsh-typecheck.sh   （环境缺件时 SKIP 并 exit 0，打印"这不是通过"）
set -uo pipefail
cd "$(dirname "$0")/.." || exit 1

PROFILES="${DSH_PROFILES:-$HOME/.dsh/profiles}"
TSC="$PROFILES/node_modules/typescript/bin/tsc"
if [ ! -f "$TSC" ]; then
  echo "SKIP typecheck-dsh：未找到 $TSC —— 不是通过，是环境缺 DSH 安装树"
  exit 0
fi

PLUG_DIR="$PWD/plugins/dsh"
LINK="$PLUG_DIR/node_modules"
TARGET="$PROFILES/node_modules"
WORK_DIR=".dsh-typecheck"
pw_esc_single() { printf '%s' "$1" | sed "s/'/''/g"; }

# 只删链接与临时目录；绝不 rm -rf junction（会顺链接删掉安装树）。
# [IO.Directory]::Delete 对重解析点只摘链接、不递归目标，且无确认提示（Remove-Item 在
# 非交互环境会因 junction 确认弹窗失败）。
cleanup() {
  [ "${CREATED_LINK:-0}" = 1 ] && powershell -NoProfile -Command "[System.IO.Directory]::Delete('$(pw_esc_single "$(cygpath -w "$LINK" 2>/dev/null || echo "$LINK")")')" >/dev/null 2>&1
  rm -rf "$WORK_DIR"
}
trap cleanup EXIT

CREATED_LINK=0
if [ ! -e "$LINK" ]; then
  powershell -NoProfile -Command "New-Item -ItemType Junction -Path '$(pw_esc_single "$(cygpath -w "$LINK" 2>/dev/null || echo "$LINK")")' -Target '$(pw_esc_single "$(cygpath -w "$TARGET" 2>/dev/null || echo "$TARGET")")' | Out-Null" >/dev/null 2>&1
  [ -e "$LINK" ] && CREATED_LINK=1
  if [ "$CREATED_LINK" != 1 ]; then
    echo "SKIP typecheck-dsh：无法创建 $LINK 联接（junction）—— 不是通过"
    exit 0
  fi
fi

# @types/node 探测：DSH 树 → 仓库上一级 → 都无则最小替身 + 只查插件源码
NODE_TYPES=""
for cand in "$TARGET/@types/node" "$(dirname "$PWD")/node_modules/@types/node"; do
  if [ -f "$cand/package.json" ]; then NODE_TYPES="$cand"; break; fi
done

to_win() { local w; w=$(cygpath -w "$1" 2>/dev/null || echo "$1"); echo "${w//\\//}"; }
PLUG_W="$(to_win "$PLUG_DIR")"
SCRIPTS_W="$(to_win "$PWD/scripts")"

TYPES_JSON='[]'
TYPE_ROOTS='[]'
MODE="minimal"
if [ -n "$NODE_TYPES" ]; then
  MODE="full"
  TYPES_JSON='["node"]'
  TYPE_ROOTS="[\"$(to_win "$(dirname "$NODE_TYPES")")\"]"
  INCLUDES="\"$PLUG_W/index.ts\", \"$PLUG_W/bench-core.ts\", \"$PLUG_W/bench-core.test.ts\", \"$PLUG_W/plugin.test.ts\""
else
  echo "note: 未找到 @types/node，用最小全局替身，仅检查插件源码"
  INCLUDES="\"$PLUG_W/index.ts\", \"$PLUG_W/bench-core.ts\", \"$SCRIPTS_W/dsh-node-min.d.ts\""
fi

mkdir -p "$WORK_DIR"
cat > "$WORK_DIR/tsconfig.json" <<JSON
{
  "compilerOptions": {
    "target": "es2023",
    "module": "nodenext",
    "moduleResolution": "nodenext",
    "lib": ["es2023", "dom"],
    "strict": true,
    "noEmit": true,
    "allowImportingTsExtensions": true,
    "verbatimModuleSyntax": true,
    "skipLibCheck": true,
    "typeRoots": $TYPE_ROOTS,
    "types": $TYPES_JSON
  },
  "include": [$INCLUDES]
}
JSON
echo "tsc: $(node "$TSC" --version) (mode=$MODE)"
node "$TSC" -p "$WORK_DIR/tsconfig.json"
code=$?
[ $code -eq 0 ] && echo "typecheck-dsh 通过" || echo "typecheck-dsh 失败（exit=$code）"
exit $code
