#!/usr/bin/env bash
# 本机验证用的临时环境：源站 + 静态服务器 + 一条种子 + 一个邀请码。
# 不进版本库（.gitignore 已排除 .webverify/），仅用于手工/浏览器验证。
set -uo pipefail
cd "$(dirname "$0")/.." || exit 1
W="$PWD/.webverify"
mkdir -p "$W"
export PATH="/d/Scoop/apps/go/current/bin:$PATH"
export BENCH_SECRET_KEY="1111111111111111111111111111111111111111111111111111111111111111"

go build -o "$W/bench-server.exe" ./cmd/server || exit 1
go build -o "$W/bench.exe" ./cmd/cli || exit 1

cat > "$W/config.yaml" <<'YAML'
server:
  addr: ":18099"
store:
  path: .webverify/web.db
  migrate: true
auth:
  readonly_anonymous: true
bandwidth:
  watch_enabled: false
ratelimit:
  window: 60s
  anonymous:
    keys: 50
    list: 3000
    get: 3000
  authed:
    keys: 300
    list: 3000
    get: 3000
    scores: 300
    upload: 300
cors:
  allowed_origins: ["http://127.0.0.1:18097"]
YAML

"$W/bench-server.exe" -config "$W/config.yaml" -put-key "web:web-key:web-secret" >/dev/null 2>&1 || exit 1

nohup "$W/bench-server.exe" -config "$W/config.yaml" -dev -log-level warn > "$W/server.log" 2>&1 &
echo $! > "$W/server.pid"

nohup node -e '
const http=require("http"),fs=require("fs"),path=require("path");
const root=path.resolve("web/dist");
const types={".html":"text/html; charset=utf-8",".js":"text/javascript; charset=utf-8",".svg":"image/svg+xml"};
http.createServer((req,res)=>{
  const u=decodeURIComponent(req.url.split("?")[0]);
  const f=path.join(root,u==="/"?"/index.html":u);
  fs.readFile(f,(e,b)=>{ if(e){res.writeHead(404).end("nf");return;}
    res.writeHead(200,{"Content-Type":types[path.extname(f)]||"application/octet-stream"}).end(b); });
}).listen(18097,"127.0.0.1");
' > "$W/static.log" 2>&1 &
echo $! > "$W/static.pid"

for _ in $(seq 1 40); do curl -fsS http://127.0.0.1:18099/-/healthz >/dev/null 2>&1 && break; sleep 0.25; done
for _ in $(seq 1 40); do curl -fsS http://127.0.0.1:18097/ >/dev/null 2>&1 && break; sleep 0.25; done

# 种子数据
curl -sS -X POST http://127.0.0.1:18099/v1/prompts -H "Authorization: Bearer web-key" \
  -H "Content-Type: application/json" \
  -d '{"p":"浏览器验证：请用三句话解释增量同步，并举一个会因竞态丢数据的反例。标记 BROWSER2-7h3k","t":["browser2"],"clientId":"browser2-1"}' >/dev/null
ID=$(curl -sS "http://127.0.0.1:18099/v1/prompts?limit=1" | sed -n 's/.*"id":"\([^"]*\)".*/\1/p' | head -1)
[ -n "$ID" ] && "$W/bench-server.exe" -config "$W/config.yaml" -approve "$ID" >/dev/null 2>&1

# 邀请码（写文件，供验证时读取）
"$W/bench-server.exe" -config "$W/config.yaml" -gen-invite "浏览器验证:5" 2>/dev/null \
  | sed -n 's/邀请码：\([A-Z0-9-]*\).*/\1/p' > "$W/invite.txt"

echo "源站 pid=$(cat "$W/server.pid")  静态 pid=$(cat "$W/static.pid")"
echo "种子 id=$ID"
echo "邀请码=$(cat "$W/invite.txt")"
