#!/usr/bin/env bash
# 采集 bench CLI 的真实 --json 输出与退出码，作为框架适配的契约地面数据。
# 交接文档里的示例必须来自这里，而不是作者的记忆或推测。
set -uo pipefail
cd "$(dirname "$0")/.." || exit 1   # 按仓库内相对路径定位，不写死绝对路径

GO="${GO:-go}"
CURL="${CURL:-curl}"
PORT=18099
BASE="http://127.0.0.1:${PORT}"
WORK=".contract"

EXE=""
case "$(/usr/bin/env uname -s 2>/dev/null || echo x)" in MINGW*|MSYS*|CYGWIN*) EXE=".exe";; esac
export BENCH_SECRET_KEY="3333333333333333333333333333333333333333333333333333333333333333"

rm -rf "$WORK"; mkdir -p "$WORK"
$GO build -trimpath -o "$WORK/bench-server$EXE" ./cmd/server || exit 1
$GO build -trimpath -o "$WORK/bench$EXE" ./cmd/cli || exit 1
SRVB="$WORK/bench-server$EXE"; B="$WORK/bench$EXE"

CFG="$WORK/server.yaml"
cat > "$CFG" <<YAML
server: { addr: ":${PORT}", read_timeout: 5s, write_timeout: 10s }
store: { path: ${WORK}/server.db, migrate: true }
auth: { readonly_anonymous: true, max_clock_skew: 300s }
bandwidth: { watch_enabled: false }
moderation: { enabled: true, max_prompt_len: 8192 }
cors: { allowed_origins: ["*"] }
YAML

$SRVB -config "$CFG" -put-key "doc:bench-key:bench-secret" >/dev/null 2>&1 || exit 1
$SRVB -config "$CFG" -dev -log-level warn >"$WORK/server.log" 2>&1 &
SRV=$!
trap 'kill $SRV 2>/dev/null; wait 2>/dev/null' EXIT
for _ in $(seq 1 60); do $CURL -fsS "$BASE/-/healthz" >/dev/null 2>&1 && break; sleep 0.25; done

H="$WORK/home"
b() { BENCH_HOME="$H" BENCH_ENDPOINT="$BASE" BENCH_API_KEY=bench-key BENCH_SECRET=bench-secret \
      "$B" "$@" --json --home "$H" 2>"$WORK/stderr.log"; }
# 打印：命令、stdout、退出码
cap() {
  printf '\n\033[1m$ bench %s\033[0m\n' "$*"
  out="$(b "$@" 2>/dev/null)"; code=$?
  printf 'exit=%s\n' "$code"
  [ -n "$out" ] && printf '%s\n' "$out" | head -c 1200
  printf '\n'
}

echo "################ 成功路径"
cap version
cap meta
UP="$(b upload -c '请用五号字写一封辞职信，语气要克制但锋利。' -t writing --client-id doc-1 2>/dev/null | sed -n 's/.*"id":"\([^"]*\)".*/\1/p' | head -1)"
$SRVB -config "$CFG" -approve "$UP" >/dev/null 2>&1
b upload -c '实现一个支持并发的有界队列，说明锁粒度取舍。' -t coding --client-id doc-2 >/dev/null 2>&1
cap sync
cap random
cap get "$UP"
cap list --all
cap score "$UP" 4
cap config show

echo; echo "################ 错误路径（退出码 + error.code 是插件分支依据）"
cap get does-not-exist
cap score "$UP" 9
cap random --tag=no-such-tag
cap upload -c 'x'

echo; echo "################ 离线/不可达（源站下线时插件会看到什么）"
printf '\n\033[1m$ bench meta --json   (endpoint 指向不可达端口)\033[0m\n'
"$B" config init --endpoint "http://127.0.0.1:1" --home "$WORK/off" >/dev/null 2>&1
out="$("$B" meta --json --home "$WORK/off" 2>/dev/null)"; printf 'exit=%s\n%s\n' "$?" "$out"

echo; echo "################ stderr 形状（请求元信息，--quiet 可抑制）"
BENCH_HOME="$H" "$B" get "$UP" --json --home "$H" >/dev/null 2>"$WORK/get.stderr"
cat "$WORK/get.stderr"

echo; echo "################ 文本模式（无 --json）错误走 stderr，stdout 为空"
rm -rf "$WORK/textmode"; mkdir -p "$WORK/textmode"
"$B" config init --endpoint http://127.0.0.1:1 --home "$WORK/textmode" >/dev/null 2>&1
"$B" get p_x --home "$WORK/textmode" 2>"$WORK/textmode.err"
echo "exit=$? stdout=空 stderr=↓"
cat "$WORK/textmode.err"
echo "→ 结论：机器调用必须带 --json，否则拿不到可解析的错误信封"