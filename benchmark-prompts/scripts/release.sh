#!/usr/bin/env bash
# 打包全平台发布产物：交叉编译 → tar.gz → sha256sums.txt → RELEASE-INFO。
#
# 关键约定：**构建时间戳由本脚本决定**，再显式传给 make。
# 原因：Makefile 里的 BUILD_DATE 每次调用都会重算，若 release 与 release-verify
# 分别是两次 make，验证器拿到的日期与产物里烘进去的日期必然不同，字节级校验会假失败。
# 把日期的所有权收归本脚本 + 写进 dist/RELEASE-INFO，验证才有确定对象。
set -uo pipefail
cd "$(dirname "$0")/.." || exit 1

GO="${GO:-go}"
VERSION="${VERSION:-dev}"
COMMIT="${COMMIT:-none}"
BUILD_DATE="${BUILD_DATE:-$(date -u +%Y-%m-%dT%H:%M:%SZ)}"
DIST="dist"
TMP="$DIST/.stage"

BENCH_TARGETS="linux-amd64 linux-arm64 darwin-amd64 darwin-arm64 windows-amd64"
SERVER_TARGETS="linux-amd64 linux-arm64 darwin-arm64"

if [ "$VERSION" = "dev" ]; then
  echo "版本号为 dev（无 VERSION 文件也无 git tag），拒绝产出不可发布的产物" >&2
  exit 1
fi
command -v sha256sum >/dev/null 2>&1 || { echo "缺少 sha256sum（coreutils），发布中止" >&2; exit 1; }

rm -rf "$TMP" "$DIST"/bench-"$VERSION"-*.tar.gz "$DIST"/bench-server-*.tar.gz \
     "$DIST"/sha256sums.txt "$DIST"/RELEASE-INFO
mkdir -p "$TMP"

echo "==> 交叉编译 $VERSION (commit $COMMIT, $BUILD_DATE)"
# 命令行变量优先级高于 Makefile 内的 :=，因此这里能把日期钉死
make --no-print-directory build-all \
  VERSION="$VERSION" COMMIT="$COMMIT" BUILD_DATE="$BUILD_DATE" || exit 1

pack() { # $1=源二进制 $2=归档内二进制名 $3=归档名 [extra...]
  local src="$1" binname="$2" archive="$3" d
  d="$TMP/$(basename "$archive" .tar.gz)"
  shift 3
  if [ ! -f "$src" ]; then echo "缺少 $src" >&2; return 1; fi
  rm -rf "$d"; mkdir -p "$d"
  cp "$src" "$d/$binname"
  chmod 755 "$d/$binname" 2>/dev/null
  printf '%s\n' "$VERSION" > "$d/VERSION"
  local f
  for f in "$@"; do [ -f "$f" ] && cp "$f" "$d/"; done
  tar -czf "$DIST/$archive" -C "$TMP" "$(basename "$d")" || return 1
  rm -rf "$d"
  printf '  %-48s %s\n' "$archive" "$(du -h "$DIST/$archive" | cut -f1)"
}

echo "==> 打包 bench"
for t in $BENCH_TARGETS; do
  case "$t" in
    windows-*) pack "$DIST/bench-$t.exe" "bench.exe" "bench-$VERSION-$t.tar.gz" LICENSE || exit 1 ;;
    *)         pack "$DIST/bench-$t"     "bench"     "bench-$VERSION-$t.tar.gz" LICENSE || exit 1 ;;
  esac
done

echo "==> 打包 bench-server（附示例配置）"
for t in $SERVER_TARGETS; do
  pack "$DIST/bench-server-$t" "bench-server" "bench-server-$VERSION-$t.tar.gz" \
    config.example.yaml LICENSE || exit 1
done

# 发布清单：验证器据此判断产物是否来自同一次构建
{
  printf 'version=%s\n' "$VERSION"
  printf 'commit=%s\n' "$COMMIT"
  printf 'build_date=%s\n' "$BUILD_DATE"
  printf 'bench_targets=%s\n' "$BENCH_TARGETS"
  printf 'server_targets=%s\n' "$SERVER_TARGETS"
} > "$DIST/RELEASE-INFO"

echo "==> 生成校验值"
( cd "$DIST" && rm -f sha256sums.txt \
  && for a in bench-"$VERSION"-*.tar.gz bench-server-"$VERSION"-*.tar.gz RELEASE-INFO; do
       [ -f "$a" ] && sha256sum "$a" >> sha256sums.txt
     done ) || exit 1

n=$(wc -l < "$DIST/sha256sums.txt")
echo "  sha256sums.txt（$n 条）+ RELEASE-INFO"
[ "$n" -gt 0 ] || { echo "校验值为空，发布中止" >&2; exit 1; }
rm -rf "$TMP"
echo "==> 完成：$DIST/"