#!/usr/bin/env bash
# M5 前端门禁：真实源站 + 真实构建产物 + 浏览器实际执行的那份取数代码。
#
# 为什么不用浏览器：本机 chrome-devtool MCP 拉不起 Chrome，而为拿一张截图去动用
# 用户的真实 Chrome profile 不值得。这里改为：
#   1) 用带 Origin 的 curl 证明浏览器不会被 CORS 挡住（浏览器拦不拦只看响应头）；
#   2) 用 scripts/web-api-check.mjs 直接 import web/src/lib/api.ts 打真源站，
#      测的就是浏览器里跑的那份代码，而不是"看起来一样"的复刻。
#
# 覆盖：前置 → 源站+种子 → 产物纯静态与资产引用完整 → CORS（白名单/预检/Vary/
#
#       陌生源拒绝）→ gzip → api.ts 的 8 组行为 → 体积报告
set -uo pipefail

cd "$(dirname "$0")/.." || exit 1

NODE="${NODE:-node}"
CURL="${CURL:-curl}"
GO="${GO:-go}"
PORT="${PORT:-18098}"
WEB_PORT="${WEB_PORT:-18097}"
BASE="http://127.0.0.1:${PORT}"
API="${BASE}/v1"
ORIGIN="http://127.0.0.1:${WEB_PORT}"
WORK=".smoke-web"
MARK="WEBMARK-9f4c27"
TAG="smoke-web"

PASS=0
FAIL=0
SKIPN=0
ok() { PASS=$((PASS + 1)); printf '  \033[32mok\033[0m   %s\n' "$1"; }
bad() {
	FAIL=$((FAIL + 1))
	printf '  \033[31mFAIL\033[0m %s\n' "$1"
	[ -n "${2:-}" ] && printf '        %s\n' "$2"
}
skip() {
	SKIPN=$((SKIPN + 1))
	printf '  \033[33mSKIP\033[0m %s（不是通过）\n' "$1"
}
desc() { printf '\n\033[1m%s\033[0m\n' "$1"; }
summary() {
	printf '\n\033[1m前端冒烟结果\033[0m  通过 %d，失败 %d，跳过 %d\n' "$PASS" "$FAIL" "$SKIPN"
}
# SKIP 一律以 0 退出（缺环境不阻塞 CI），但必须把「跳过」显式打出来。
bail_skip() {
	skip "$1"
	summary
	exit 0
}

SRV=""
STATIC=""
cleanup() {
	[ -n "$SRV" ] && kill "$SRV" 2>/dev/null
	[ -n "$STATIC" ] && kill "$STATIC" 2>/dev/null
	wait 2>/dev/null
	rm -rf "$WORK"
}
trap cleanup EXIT

# ---------- 前置 ----------
desc "前置"
command -v "$NODE" >/dev/null 2>&1 || bail_skip "缺 node"
[ -d web/dist ] || bail_skip "web/dist 不存在，先跑 make web-build"
[ -f web/src/lib/api.ts ] || bail_skip "web/src/lib/api.ts 不存在"
ok "node 与站点源码/产物齐全"

mkdir -p "$WORK"

# ---------- 编译并启动源站 ----------
desc "准备源站与种子数据"
SRV_BIN="$WORK/bench-server"
if ! "$GO" build -o "$SRV_BIN" ./cmd/server 2>"$WORK/build.err"; then
	bad "编译 server 失败" "$(cat "$WORK/build.err")"
	summary
	exit 1
fi
ok "server 编译完成"

export BENCH_SECRET_KEY="1111111111111111111111111111111111111111111111111111111111111111"
CFG="$WORK/config.yaml"
cat >"$CFG" <<YAML
server:
  addr: ":${PORT}"
store:
  path: ${WORK}/web.db
  migrate: true
auth:
  readonly_anonymous: true
bandwidth:
  watch_enabled: false
moderation:
  enabled: true
  max_prompt_len: 8192
cors:
  allowed_origins: ["${ORIGIN}"]
YAML

"$SRV_BIN" -config "$CFG" -put-key "web:web-smoke-key:web-smoke-secret" >"$WORK/putkey.log" 2>&1 &&
	ok "登记 API Key" || bad "登记 Key 失败" "$(cat "$WORK/putkey.log")"

"$SRV_BIN" -config "$CFG" -dev -log-level warn >"$WORK/server.log" 2>&1 &
SRV=$!
ready=0
for _ in $(seq 1 60); do
	"$CURL" -fsS "$BASE/-/healthz" >/dev/null 2>&1 && { ready=1; break; }
	sleep 0.25
done
if [ "$ready" != "1" ]; then
	bad "源站未在 15s 内就绪" "$(cat "$WORK/server.log")"
	summary
	exit 1
fi
ok "源站就绪 (pid $SRV)"

SEED_TEXT="请用三句话解释什么是增量同步，并给出一个会因竞态丢数据的反例。唯一标记 ${MARK}"
BODY="$("$NODE" -e 'console.log(JSON.stringify({p:process.argv[1],t:[process.argv[2]],clientId:"web-smoke-seed"}))' \
	"$SEED_TEXT" "$TAG")"
UP="$("$CURL" -sS -X POST "$API/prompts" -H "Authorization: Bearer web-smoke-key" \
	-H "Content-Type: application/json" -d "$BODY")"
ID1="$(printf '%s' "$UP" | sed -n 's/.*"id":"\([^"]*\)".*/\1/p' | head -1)"
if [ -z "$ID1" ]; then
	bad "种子上传失败" "$UP"
	summary
	exit 1
fi
ok "种子已上传 $ID1"
"$SRV_BIN" -config "$CFG" -approve "$ID1" >"$WORK/approve.log" 2>&1 &&
	ok "种子审核通过" || bad "审核失败" "$(cat "$WORK/approve.log")"

# ---------- 产物必须纯静态 ----------
desc "产物：纯静态、可原样上 CDN"
[ -f web/dist/index.html ] && ok "index.html 存在" || bad "缺 index.html"
if [ -d web/dist/server ] || [ -d web/dist/_server ]; then
	bad "dist 里出现服务端产物，说明不是纯静态输出"
else
	ok "无 SSR 产物目录"
fi
# 源站不得代管前端（否则零回源不成立）。注意：因为注册了 OPTIONS / 兼底，
# 根路径会返 405 而不是 404；断言看的是“有没有把前端让出去”，不是状态码形状。
root_code="$("$CURL" -s -o "$WORK/root.body" -w '%{http_code}' "${BASE}/" 2>/dev/null)"
root_ctype="$("$CURL" -sS -D - -o /dev/null "${BASE}/" 2>/dev/null | tr -d '\r' | grep -i '^content-type:' || true)"
if [ "$root_code" = "200" ] || printf '%s' "$root_ctype" | grep -qi "text/html"; then
	bad "源站竟能返回前端页面，零回源约束被破坏（code=$root_code ctype=$root_ctype）"
else
	ok "源站不代管前端（根路径 code=$root_code，非 HTML）"
fi

grep -q "_astro/" web/dist/index.html && ok "index.html 已挂载岛资产" ||
	bad "index.html 里没有岛资产引用"
[ -f web/dist/runtime-config.js ] && ok "runtime-config.js 在产物根目录（部署时可覆盖）" ||
	bad "缺 runtime-config.js"

# 从 index.html 出发，沿 import 遍历整张资产图（少一个文件就是 CDN 上白屏）
"$NODE" scripts/web-asset-graph.mjs >"$WORK/refs.log" 2>&1
ref_rc=$?
while IFS= read -r line; do
	case "$line" in
	MISS\ *) bad "资产在产物里不存在：${line#MISS }" ;;
	OK\ *)   ok "${line#OK }" ;;
	ERR\ *)  bad "${line#ERR }" ;;
	esac
done <"$WORK/refs.log"
[ "$ref_rc" = "0" ] || bad "资产图核对异常退出（code $ref_rc）"

# ---------- 用静态服务器模拟 CDN ----------
desc "以纯静态服务器模拟 CDN"
"$NODE" -e '
const http=require("http"),fs=require("fs"),path=require("path");
const root=path.resolve("web/dist");
const types={".html":"text/html; charset=utf-8",".js":"text/javascript; charset=utf-8",
  ".css":"text/css; charset=utf-8",".svg":"image/svg+xml",".json":"application/json"};
http.createServer((req,res)=>{
  const u=decodeURIComponent(req.url.split("?")[0]);
  const f=path.join(root,u==="/"?"/index.html":u);
  if(!f.startsWith(root)){res.writeHead(403).end();return;}
  fs.readFile(f,(e,b)=>{
    if(e){res.writeHead(404,{"Content-Type":"text/plain"}).end("nf");return;}
    res.writeHead(200,{"Content-Type":types[path.extname(f)]||"application/octet-stream"}).end(b);
  });
}).listen(Number(process.argv[1]),"127.0.0.1",()=>console.log("up"));
' "$WEB_PORT" >"$WORK/static.log" 2>&1 &
STATIC=$!
# node -e 的附加参数从 argv[1] 起算（上面种子那处同理）；端口没起来要能看到原因。
sup=0
for _ in $(seq 1 30); do
	"$CURL" -fsS "${ORIGIN}/" >/dev/null 2>&1 && { sup=1; break; }
	sleep 0.2
done
if [ "$sup" = "1" ] && "$CURL" -fsS "${ORIGIN}/" >"$WORK/page.html" 2>"$WORK/page.err"; then
	ok "静态服务器能提供站点（CDN 只需做同一件事）"
	grep -q "<title>Benchmark 提示词库</title>" "$WORK/page.html" &&
	# 组件树确实渲染进来了（任一个组件抛错，astro build 会当场失败）
	for label in "列表" "随机一条" "上传" "连接设置" "API Key"; do
		grep -q "$label" "$WORK/page.html" && ok "预渲染含「$label」" || bad "预渲染缺「$label」"
	done
	grep -q "astro-island" "$WORK/page.html" && ok "岛容器已就位（注水后接管交互）" || bad "缺岛容器"
	grep -q "noscript" "$WORK/page.html" && ok "有 noscript 降级说明（并指向 bench CLI）" || bad "缺 noscript 降级"
	grep -q -- "--accent:" "$WORK/page.html" && ok "CSS 已内联进 HTML（少一次请求）" ||
		bad "样式没有内联，检查 build.inlineStylesheets"
else
	bad "站点取不到" "curl: $(cat "$WORK/page.err") / server: $(cat "$WORK/static.log")"
fi

# ---------- CORS ----------
desc "CORS（白名单只放行了 $ORIGIN）"
pre="$("$CURL" -sS -D - -o /dev/null -X OPTIONS "$API/prompts" \
	-H "Origin: $ORIGIN" -H "Access-Control-Request-Method: POST")"
case "$pre" in
*"Access-Control-Allow-Origin: $ORIGIN"*) ok "预检回显白名单 Origin" ;;
*) bad "预检未放行" "$(printf '%s' "$pre" | head -3)" ;;
esac
case "$pre" in
*"Access-Control-Allow-Headers:"*Authorization*) ok "预检放行 Authorization 头" ;;
*) bad "预检未放行 Authorization，写入会被拦" ;;
esac

get_h="$("$CURL" -sS -D - -o /dev/null "$API/prompts" -H "Origin: $ORIGIN")"
case "$get_h" in
*"Access-Control-Allow-Origin: $ORIGIN"*) ok "实际 GET 带 ACAO" ;;
*) bad "GET 未带 ACAO" ;;
esac
case "$get_h" in
*[Vv]ary:*[Oo]rigin*) ok "带 Vary: Origin（否则 CDN 会把放行响应串给别的站点）" ;;
*) bad "缺 Vary: Origin —— CDN 共享缓存下会串源" ;;
esac
case "$get_h" in
*"Content-Encoding: gzip"*) bad "未请求压缩却压了，代理层容易出问题" ;;
*) ok "未请求压缩时不压" ;;
esac

evil="$("$CURL" -sS -D - -o /dev/null "$API/prompts" -H "Origin: http://evil.example")"
case "$evil" in
*"Access-Control-Allow-Origin"*) bad "陌生 Origin 竟被放行（越权读写风险）" ;;
*) ok "陌生 Origin 不返回 ACAO，浏览器会拦掉" ;;
esac

# ---------- gzip ----------
desc "压缩"
z_h="$("$CURL" -sS -D - -o /dev/null --compressed "$API/prompts?limit=20" -H "Origin: $ORIGIN")"
case "$z_h" in
*"Content-Encoding: gzip"*) ok "list 响应是 gzip（2M/10M 之外的用户体验关键项）" ;;
*) bad "gzip 未生效" ;;
esac

# ---------- 浏览器那份取数代码对真源站 ----------
desc "web/src/lib/api.ts 对真源站的实际行为"
if WEB_BASE="$BASE/v1" WEB_KEY="web-smoke-key" WEB_SEED="$ID1" WEB_MARK="$MARK" WEB_TAG="$TAG" \
	"$NODE" scripts/web-api-check.mjs >"$WORK/run.log" 2>&1; then
	st=0
else
	st=$?
fi
while IFS= read -r line; do
	case "$line" in
	OK\ *) ok "${line#OK }" ;;
	ERR\ *) bad "${line#ERR }" ;;
	esac
done <"$WORK/run.log"
if [ "$st" != "0" ] && ! grep -q "^ERR" "$WORK/run.log"; then
	bad "api.ts 检查异常退出（code $st）" "$(tail -5 "$WORK/run.log")"
fi

# ---------- 体积报告 ----------
desc "产物体积（只报告不设阈值，见 docs/frontend.md §4.1）"
bash scripts/web-size.sh
ok "体积已报告"

summary
[ "$FAIL" = "0" ] || exit 1
