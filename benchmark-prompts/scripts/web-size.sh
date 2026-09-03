#!/usr/bin/env bash
# 报告 web/dist 的产物体积（原始 + gzip -9）。
#
# 为什么不卡阈值：docs/frontend.md §4.1 已取消首屏 JS 硬预算，改为
# 「必须报告、不得无声增长」。没有这份输出，"不预算"就退化成"没人看"。
set -uo pipefail

cd "$(dirname "$0")/.." || exit 1
DIST="${1:-web/dist}"

if [ ! -d "$DIST" ]; then
	printf 'SKIP 体积报告：%s 不存在（先跑 make web-build）——不是通过\n' "$DIST"
	exit 0
fi

printf '\n\033[1m前端产物体积\033[0m  %s\n' "$DIST"
total_raw=0
total_gz=0
for f in $(find "$DIST" -type f | sort); do
	raw="$(wc -c <"$f" | tr -d ' ')"
	gz="$(gzip -9 -c "$f" | wc -c | tr -d ' ')"
	total_raw=$((total_raw + raw))
	total_gz=$((total_gz + gz))
	printf '  %-42s %7s B  →  %7s B\n' "${f#"$DIST"/}" "$raw" "$gz"
done
printf '  %-42s %7s B  →  %7s B\n' "合计" "$total_raw" "$total_gz"

# 首屏必经路径：HTML + 全部 JS（CSS 可能被内联进 HTML）
js_gz=0
for f in "$DIST"/_astro/*.js; do
	[ -f "$f" ] || continue
	js_gz=$((js_gz + $(gzip -9 -c "$f" | wc -c | tr -d ' ')))
done
html_gz="$(gzip -9 -c "$DIST/index.html" | wc -c | tr -d ' ')"
printf '  %-42s %7s B\n' "首屏必经（index.html + 全部 JS，gzip）" "$((html_gz + js_gz))"
printf '  %-42s %7s B\n' "其中 JS（岛运行时 + 应用代码，gzip）" "$js_gz"
