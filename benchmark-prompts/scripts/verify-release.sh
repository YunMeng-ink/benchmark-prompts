#!/usr/bin/env bash
# 验证发布产物。
#
# 难点：跨平台产物在本机跑不了，怎么证明"版本确实注入进那一个文件"？
#   ❌ 曾想用 `go version -m` 读 build info —— 实测**不成立**：go1.27 的 build info
#      只记录 -buildmode/-compiler/-trimpath 与 GO* 环境变量，**不记录 -ldflags**。
#   ✅ 改用字节级证据：-X 注入的构建时间戳是唯一的（秒级、且只可能来自本次构建），
#      在目标二进制里能搜到它，就说明该产物是带着这份版本编译出来的。
#      再叠加"本机那一个真的跑起来报告同一版本"作为机制正确性的强证据。
#
# 另一条踩过的坑：本脚本开了 pipefail，`tar | grep -q` 会因为 grep 提前关管道
# 让 tar 收到 SIGPIPE 而整条管线返回 141 —— 内容没问题也会判失败。
# 因此凡是"列举后判断包含"的地方，一律先取进变量再用 case/[[ ]] 匹配。
set -uo pipefail
cd "$(dirname "$0")/.." || exit 1

DIST="dist"

# 从发布清单读回本次构建确实用了什么参数，而不是自己重算一份去"对"产物。
if [ ! -f "$DIST/RELEASE-INFO" ]; then
  echo "缺 $DIST/RELEASE-INFO：先跑 make release" >&2
  exit 1
fi
VERSION="${VERSION:-$(sed -n 's@^version=@@p' "$DIST/RELEASE-INFO")}"
BUILD_DATE="${BUILD_DATE:-$(sed -n 's@^build_date=@@p' "$DIST/RELEASE-INFO")}"
BENCH_TARGETS="$(sed -n 's@^bench_targets=@@p' "$DIST/RELEASE-INFO")"
SERVER_TARGETS="$(sed -n 's@^server_targets=@@p' "$DIST/RELEASE-INFO")"

PASS=0; FAIL=0; SKIP=0
ok()   { PASS=$((PASS+1)); printf '  \033[32mok\033[0m   %s\n' "$1"; }
bad()  { FAIL=$((FAIL+1)); printf '  \033[31mFAIL\033[0m %s\n' "$1"; [ -n "${2:-}" ] && printf '        %s\n' "$2"; }
skip() { SKIP=$((SKIP+1)); printf '  \033[33mskip\033[0m %s（不是通过）\n' "$1"; }

if [ -z "$BUILD_DATE" ]; then
  echo "缺少 BUILD_DATE：无法做字节级注入证据。请用 make release-verify（它会传入）。" >&2
  exit 1
fi

# 在二进制字节里找 marker（-a 允许二进制；grep 直接吃文件，无管道）
has_marker() { grep -a -q -- "$2" "$1" 2>/dev/null; }

check_binary() { # $1=文件 $2=标签
  if [ ! -f "$1" ]; then bad "$2：文件不存在"; return; fi
  if [ ! -s "$1" ]; then bad "$2：文件为空"; return; fi
  if ! has_marker "$1" "$BUILD_DATE"; then
    bad "$2：找不到构建时间戳 $BUILD_DATE，版本未经 -X 注入"
  elif ! has_marker "$1" "$VERSION"; then
    bad "$2：找不到版本号 $VERSION"
  else
    ok "$2：嵌入 $(basename "$1") 版本 $VERSION @ $BUILD_DATE"
  fi
}

echo "==> 1. 二进制存在性与 -X 注入证据（$VERSION）"
for t in $BENCH_TARGETS; do
  case "$t" in windows-*) f="$DIST/bench-$t.exe";; *) f="$DIST/bench-$t";; esac
  check_binary "$f" "bench-$t"
done
for t in $SERVER_TARGETS; do
  check_binary "$DIST/bench-server-$t" "bench-server-$t"
done

echo "==> 2. 归档与校验值"
if [ ! -f "$DIST/sha256sums.txt" ]; then
  bad "缺 sha256sums.txt"
else
  n=$(wc -l < "$DIST/sha256sums.txt")
  if [ "$n" -eq 0 ]; then bad "sha256sums.txt 为空"
  else ok "sha256sums.txt 有 $n 条"; fi
  ( cd "$DIST" && sha256sum -c --strict sha256sums.txt >../.sha.log 2>&1 )
  rc=$?
  if [ "$rc" -ne 0 ]; then
    bad "校验值不匹配或有条目缺失" "$(grep -E ': (FAILED|MISSING|WARNING)' .sha.log | head -3)"
  else
    ok "全部归档 sha256 校验通过（--strict）"
  fi
  rm -f .sha.log
fi

echo "==> 3. 归档结构（解压后二进制必须叫 bench / bench-server）"
for a in "$DIST"/bench-"$VERSION"-*.tar.gz; do
  [ -f "$a" ] || continue
  listing="$(tar -tzf "$a")
"
  # 补尾行换行：$(...) 会吃掉它，否则最后一个条目永远匹配不上
  base="$(basename "$a")"
  case "$base" in
    *windows-*) want="bench.exe";;
    *)          want="bench";;
  esac
  case "$listing" in
    */$want"
"*) ok "$base → $want" ;;
    *)  bad "$base 内容异常" "$(printf '%s' "$listing" | head -3 | tr '\n' ' ')" ;;
  esac
  case "$listing" in
    */VERSION"
"*) : ;;
    *) bad "$base 缺 VERSION 文件（安装脚本要靠它校验）" ;;
  esac
done
for a in "$DIST"/bench-server-"$VERSION"-*.tar.gz; do
  [ -f "$a" ] || continue
  listing="$(tar -tzf "$a")
"
  # 补尾行换行：$(...) 会吃掉它，否则最后一个条目永远匹配不上
  base="$(basename "$a")"
  case "$listing" in
    */bench-server"
"*) ok "$base → bench-server" ;;
    *)  bad "$base 缺 bench-server" ;;
  esac
  case "$listing" in
    */config.example.yaml"
"*) : ;;
    *) bad "$base 缺 config.example.yaml（部署方拿不到配置模板）" ;;
  esac
done

echo "==> 4. 本机真执行（机制正确性的强证据）"
EXE=""; OSN="$(uname -s | tr 'A-Z' 'a-z')"
case "$OSN" in msys*|mingw*|cygwin*) EXE=".exe"; OSN=windows;; esac
nat="$DIST/bench-$OSN-amd64$EXE"
if [ -f "$nat" ]; then
  got="$("$nat" version --json 2>/dev/null | sed -n 's@.*"version":"\([^"]*\)".*@\1@p')"
  com="$("$nat" version --json 2>/dev/null | sed -n 's@.*"commit":"\([^"]*\)".*@\1@p')"
  if [ "$got" = "$VERSION" ]; then
    ok "bench version --json → $got（commit=$com，字段齐全）"
  else
    bad "真跑版本号不符：得到 '$got'，期望 '$VERSION'"
  fi
  d="$("$nat" version --json 2>/dev/null | sed -n 's@.*"date":"\([^"]*\)".*@\1@p')"
  if [ "$d" = "$BUILD_DATE" ]; then ok "date 字段与构建时间一致"; else bad "date 字段异常：'$d'"; fi
else
  skip "本机无对应 bench 产物（$nat）"
fi
sv="$DIST/bench-server-$OSN-amd64"
if [ -f "$sv" ]; then
  if "$sv" -version 2>&1 | grep -q "$VERSION"; then ok "bench-server -version → $VERSION"
  else bad "bench-server -version 未报告期望版本"; fi
else
  skip "发布矩阵不含本平台的 server（$OSN）；改用 make build 后本地验证"
fi

printf '\n\033[1m发布产物验证\033[0m  通过 %d，失败 %d，跳过 %d\n' "$PASS" "$FAIL" "$SKIP"
[ "$FAIL" -eq 0 ] || exit 1