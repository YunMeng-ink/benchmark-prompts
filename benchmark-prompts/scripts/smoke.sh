#!/usr/bin/env bash
# 端到端冒烟测试：真实编译 + 真实 HTTP + 真实 SQLite。
# 覆盖：注册 Key → 上传 → 审核 → 列表/随机/详情 → ETag 304 → gzip
#       → delta 增量 → 打分 → 鉴权/参数错误 → 指标 → 备份
set -uo pipefail

cd "$(dirname "$0")/.." || exit 1

GO="${GO:-go}"
CURL="${CURL:-curl}"
PORT="${PORT:-18080}"
BASE="http://127.0.0.1:${PORT}"
API="${BASE}/v1"
WORK=".smoke"

PASS=0
FAIL=0

ok()   { PASS=$((PASS+1)); printf '  \033[32mok\033[0m   %s\n' "$1"; }
bad()  { FAIL=$((FAIL+1)); printf '  \033[31mFAIL\033[0m %s\n' "$1"; [ -n "${2:-}" ] && printf '        %s\n' "$2"; }
desc() { printf '\n\033[1m%s\033[0m\n' "$1"; }

SRV=""
cleanup() {
  [ -n "$SRV" ] && kill "$SRV" 2>/dev/null
  wait 2>/dev/null
  rm -rf "$WORK"
}
trap cleanup EXIT

# 从 JSON 里抽一个字符串字段值（只用于本脚本，避免引入 jq 依赖）
jget() { sed -n "s/.*\"$1\":\"\([^\"]*\)\".*/\1/p" | head -1; }

# ---------- 准备 ----------
desc "准备：编译 server"
rm -rf "$WORK"; mkdir -p "$WORK"
if ! "$GO" build -o "$WORK/bench-server.exe" ./cmd/server 2>"$WORK/build.err"; then
  bad "编译失败" "$(cat "$WORK/build.err")"
  exit 1
fi
ok "编译成功"

# 固定的 64-hex 主密钥（仅冒烟用；真实部署必须随机生成）
export BENCH_SECRET_KEY="1111111111111111111111111111111111111111111111111111111111111111"

DB="$WORK/smoke.db"
CFG="$WORK/config.yaml"
cat > "$CFG" <<YAML
server:
  addr: ":${PORT}"
  read_timeout: 5s
  write_timeout: 10s
store:
  path: ${DB}
  migrate: true
auth:
  readonly_anonymous: true
  max_clock_skew: 300s
bandwidth:
  watch_enabled: false
moderation:
  enabled: true
  max_prompt_len: 8192
cors:
  allowed_origins: ["*"]
YAML

SRV_BIN="$WORK/bench-server.exe"

desc "准备：登记 API Key 并启动服务"
"$SRV_BIN" -config "$CFG" -put-key "smoke:local-key:hmac-secret" >"$WORK/putkey.log" 2>&1
if [ $? -ne 0 ]; then bad "登记 Key 失败" "$(cat "$WORK/putkey.log")"; else ok "API Key 已登记"; fi

"$SRV_BIN" -config "$CFG" -dev -log-level warn >"$WORK/server.log" 2>&1 &
SRV=$!

ready=0
for _ in $(seq 1 60); do
  if "$CURL" -fsS "$BASE/-/healthz" >/dev/null 2>&1; then ready=1; break; fi
  sleep 0.25
done
if [ "$ready" -ne 1 ]; then
  bad "服务未在 15s 内就绪" "$(cat "$WORK/server.log")"
  exit 1
fi
ok "服务已就绪 (pid $SRV)"

# ---------- 只读端点（空库） ----------
desc "空库行为"
body="$("$CURL" -sS "$API/prompts")"
echo "$body" | grep -q '"ok":true' && echo "$body" | grep -q '"items":\[\]' \
  && ok "空列表返回 items:[]" || bad "空列表异常" "$body"

code="$("$CURL" -s -o /dev/null -w '%{http_code}' "$API/prompts/random")"
[ "$code" = "404" ] && ok "空库随机返回 404" || bad "空库随机应为 404" "got $code"

# ---------- 鉴权 ----------
desc "鉴权"
code="$("$CURL" -s -o /dev/null -w '%{http_code}' -X POST "$API/scores" -H 'Content-Type: application/json' -d '{"id":"p_x","value":5,"deviceId":"d"}')"
[ "$code" = "401" ] && ok "未带凭据打分返回 401" || bad "应为 401" "got $code"

code="$("$CURL" -s -o /dev/null -w '%{http_code}' -X POST "$API/scores" -H 'Authorization: Bearer wrong-key' -d '{}')"
[ "$code" = "401" ] && ok "错误 Key 返回 401" || bad "错误 Key 应为 401" "got $code"

# ---------- 上传与审核 ----------
desc "上传 → 审核 → 公开"
up1="$("$CURL" -sS -X POST "$API/prompts" -H "Authorization: Bearer local-key" \
  -H 'Content-Type: application/json' \
  -d '{"p":"你是一名资深算法工程师，请实现 LRU 缓存并分析复杂度。","t":["coding"],"clientId":"cid-1"}')"
echo "$up1" | grep -q '"ok":true' && ok "上传成功" || bad "上传失败" "$up1"
ID1="$(printf '%s' "$up1" | jget id)"
[ -n "$ID1" ] && ok "取到 id=$ID1" || bad "未取到 id" "$up1"

"$CURL" -sS "$API/prompts" | grep -q "$ID1" \
  && bad "pending 内容不应公开" "" || ok "pending 内容未公开"

"$SRV_BIN" -config "$CFG" -approve "$ID1" >"$WORK/approve.log" 2>&1 \
  && ok "跨进程审核通过" || bad "审核失败" "$(cat "$WORK/approve.log")"

"$CURL" -sS "$API/prompts" | grep -q "$ID1" \
  && ok "审核后出现在列表" || bad "审核后仍不可见" "$("$CURL" -sS "$API/prompts")"

# 列表不得含正文（2M 带宽硬约束）
"$CURL" -sS "$API/prompts" | grep -q '"p":' \
  && bad "列表泄漏正文 p" "" || ok "列表不含正文"

# clientId 幂等
replay="$("$CURL" -sS -X POST "$API/prompts" -H "Authorization: Bearer local-key" \
  -d '{"p":"完全不同的正文内容","clientId":"cid-1"}')"
echo "$replay" | grep -q "$ID1" && ok "clientId 重放幂等返回原 id" || bad "幂等失效" "$replay"

# 内容去重（排版不同）
dup="$("$CURL" -sS -X POST "$API/prompts" -H "Authorization: Bearer local-key" \
  -d '{"p":"你是一名资深算法工程师，请实现 LRU 缓存并分析复杂度。","clientId":"cid-2"}')"
echo "$dup" | grep -q "$ID1" && ok "content_hash 去重生效" || bad "去重失效" "$dup"

# 校验
code="$("$CURL" -s -o /dev/null -w '%{http_code}' -X POST "$API/prompts" -H "Authorization: Bearer local-key" -d '{"p":"   ","clientId":"cid-3"}')"
[ "$code" = "422" ] && ok "空正文 422" || bad "空正文应为 422" "got $code"

code="$("$CURL" -s -o /dev/null -w '%{http_code}' -X POST "$API/prompts" -H "Authorization: Bearer local-key" -d '{"p":"非法标签内容","t":["BAD TAG"],"clientId":"cid-4"}')"
[ "$code" = "422" ] && ok "非法标签 422" || bad "非法标签应为 422" "got $code"

# ---------- 详情 / ETag ----------
desc "详情与缓存协商"
hdr="$("$CURL" -sSD - -o /dev/null "$API/prompts/$ID1")"
echo "$hdr" | grep -qi '^etag:' && ok "详情返回 ETag" || bad "详情缺少 ETag" "$hdr"
ETAG="$(printf '%s' "$hdr" | tr -d '\r' | sed -n 's/^[Ee][Tt]ag: *//p' | head -1)"
code="$("$CURL" -s -o /dev/null -w '%{http_code}' -H "If-None-Match: $ETAG" "$API/prompts/$ID1")"
[ "$code" = "304" ] && ok "If-None-Match 命中 304" || bad "应 304" "got $code (etag=$ETAG)"

body="$("$CURL" -sS "$API/prompts/$ID1")"
echo "$body" | grep -q '"p":' && ok "详情含正文" || bad "详情缺正文" "$body"

code="$("$CURL" -s -o /dev/null -w '%{http_code}' "$API/prompts/p_deadbeef")"
[ "$code" = "404" ] && ok "不存在 id 返回 404" || bad "应 404" "got $code"

body="$("$CURL" -sS "$API/prompts/random?exclude=$ID1")"
echo "$body" | grep -q '"code":"not_found"' && ok "exclude 生效" || bad "exclude 未生效" "$body"

# ---------- gzip ----------
desc "压缩"
"$CURL" -s -H 'Accept-Encoding: gzip' -o "$WORK/gz.bin" -D "$WORK/gz.hdr" "$API/meta"
grep -qi 'content-encoding: gzip' "$WORK/gz.hdr" && ok "响应已 gzip" || bad "未压缩" "$(cat "$WORK/gz.hdr")"
[ -s "$WORK/gz.bin" ] && ok "gzip 载荷非空" || bad "gzip 载荷为空" ""
# 确认确实是 gzip 魔术字节
head -c2 "$WORK/gz.bin" | od -An -tx1 | grep -qi '1f 8b' && ok "载荷是真实 gzip 数据" || bad "载荷不是 gzip 格式" ""

# ---------- delta 增量 ----------
desc "增量同步"
meta="$("$CURL" -sS "$API/meta")"
H0="$(printf '%s' "$meta" | jget catalog_hash)"
[ -n "$H0" ] && ok "取到 catalog_hash=${H0:0:12}…" || bad "未取到 hash" "$meta"
echo "$meta" | grep -q '"total":1' && ok "meta total=1" || bad "meta total 应为 1" "$meta"

"$CURL" -sS "$API/prompts/delta?since=$H0" | grep -q '"changes":\[\]' \
  && ok "无变更时 delta 为空" || bad "无变更应返回空集"

up2="$("$CURL" -sS -X POST "$API/prompts" -H "Authorization: Bearer local-key" \
  -d '{"p":"请解释 CAP 定理并给出一个分布式系统的取舍案例。","t":["systems"],"clientId":"cid-5"}')"
ID2="$(printf '%s' "$up2" | jget id)"
"$SRV_BIN" -config "$CFG" -approve "$ID2" >/dev/null 2>&1

d1="$("$CURL" -sS "$API/prompts/delta?since=$H0")"
echo "$d1" | grep -q "$ID2" && ok "delta 返回新增条目" || bad "delta 未含新增" "$d1"
H1="$(printf '%s' "$d1" | jget since)"
[ "$H1" != "$H0" ] && ok "since 已推进" || bad "since 未推进" "$H1"

"$CURL" -sS "$API/prompts/delta?since=$H1" | grep -q '"changes":\[\]' \
  && ok "再次 delta 无变化" || bad "再次 delta 应为空"

# 未知 since → 全量
"$CURL" -sS "$API/prompts/delta?since=0000000000000000000000000000000000000000000000000000000000000000" \
  | grep -q "$ID1" && ok "未知 since 回退全量" || bad "未回退全量"

# 软删除进入 deleted
"$SRV_BIN" -config "$CFG" -reject "$ID2" >/dev/null 2>&1
"$CURL" -sS "$API/prompts/delta?since=$H1" | grep -q "$ID2" \
  && ok "下架条目出现在 deleted" || bad "下架未反映在 delta"

# ---------- 打分 ----------
desc "评分"
s1="$("$CURL" -sS -X POST "$API/scores" -H "Authorization: Bearer local-key" -d "{\"id\":\"$ID1\",\"value\":5,\"deviceId\":\"dev-1\"}")"
echo "$s1" | grep -q '"count":1' && ok "首次打分 count=1" || bad "打分异常" "$s1"
s2="$("$CURL" -sS -X POST "$API/scores" -H "Authorization: Bearer local-key" -d "{\"id\":\"$ID1\",\"value\":3,\"deviceId\":\"dev-1\"}")"
echo "$s2" | grep -q '"count":1' && echo "$s2" | grep -q '"avg":3' \
  && ok "同设备重复打分被覆盖" || bad "打分未幂等覆盖" "$s2"
s3="$("$CURL" -sS -X POST "$API/scores" -H "Authorization: Bearer local-key" -d "{\"id\":\"$ID1\",\"value\":5,\"deviceId\":\"dev-2\"}")"
echo "$s3" | grep -q '"count":2' && ok "第二设备计入 count=2" || bad "count 应为 2" "$s3"

code="$("$CURL" -s -o /dev/null -w '%{http_code}' -X POST "$API/scores" -H "Authorization: Bearer local-key" -d "{\"id\":\"$ID1\",\"value\":9,\"deviceId\":\"dev-3\"}")"
[ "$code" = "422" ] && ok "value 越界 422" || bad "越界应 422" "got $code"

# 只读统计端点（docs/api.md §3.8）：不带任何鉴权头，且分数与 POST 响应一致。
# 上两行后 dev-1=3、dev-2=5 → avg=4、count=2；map 序列化按键排序，所以可连续匹配。
st="$("$CURL" -sS "$API/prompts/$ID1/score")"
case "$st" in
  *'"avg":4,"count":2'*) ok "只读统计反映已提交分数（匿名可读）" ;;
  *) bad "统计端点异常" "$st" ;;
esac
code="$("$CURL" -s -o /dev/null -w '%{http_code}' "$API/prompts/p_deadbeef/score")"
[ "$code" = "404" ] && ok "不存在 id 的统计 404" || bad "统计应 404" "got $code"

# ---------- 参数与运维 ----------
desc "参数校验与运维端点"
code="$("$CURL" -s -o /dev/null -w '%{http_code}' "$API/prompts?limit=abc")"
[ "$code" = "400" ] && ok "非法 limit 400" || bad "应 400" "got $code"
code="$("$CURL" -s -o /dev/null -w '%{http_code}' "$API/prompts?limit=9999")"
[ "$code" = "200" ] && ok "limit 超上限被钳制" || bad "应钳制而非报错" "got $code"

"$CURL" -fsS "$BASE/-/healthz" | grep -q '^ok$' && ok "healthz" || bad "healthz 异常"

# 指标端点属内部运维面：未鉴权必须 401，带 Key 才可用
code="$("$CURL" -s -o /dev/null -w '%{http_code}' "$BASE/-/metrics")"
[ "$code" = "401" ] && ok "metrics 未鉴权返回 401" || bad "metrics 应要求鉴权" "got $code"

"$CURL" -sS "$BASE/-/metrics" -H "Authorization: Bearer local-key" | grep -q '"requests"' \
  && ok "metrics 带 Key 可用" || bad "metrics 无输出"

"$CURL" -sS "$BASE/-/metrics" -H "Authorization: Bearer local-key" | grep -q '"hits_304":[1-9]' \
  && ok "metrics 已统计到 304 命中" || bad "hits_304 未统计"

# ---------- 备份 ----------
desc "维护子命令"
"$SRV_BIN" -config "$CFG" -backup "$WORK/backup.db" >"$WORK/backup.log" 2>&1
[ -s "$WORK/backup.db" ] && ok "备份文件已生成" || bad "备份失败" "$(cat "$WORK/backup.log")"

kill $SRV 2>/dev/null; wait $SRV 2>/dev/null; SRV=""
"$SRV_BIN" -config "$CFG" -review >"$WORK/review.log" 2>&1
grep -q '待审核' "$WORK/review.log" && ok "-review 可列出队列" || bad "-review 异常" "$(cat "$WORK/review.log")"

# ---------- 汇总 ----------
printf '\n\033[1m冒烟测试结果\033[0m  通过 %d，失败 %d\n' "$PASS" "$FAIL"
if [ "$FAIL" -ne 0 ]; then
  echo "--- server.log ---"; tail -20 "$WORK/server.log"
  exit 1
fi
exit 0