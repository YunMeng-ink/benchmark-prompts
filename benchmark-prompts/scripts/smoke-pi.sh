#!/usr/bin/env bash
# Pi 扩展的真实验证：起源站 → 灌数据 → 让真实的 pi 加载扩展并调用 bench_random。
#
# 这一层是 node --test 覆盖不到的：扩展能否被 jiti 解析（含 ./bench-core.ts 的
# 显式 .ts 后缀）、typebox/pi-ai 导入能否解析、工具是否真的被 LLM 调用并把
# bench 的输出带回会话。
#
# 用法：bash scripts/smoke-pi.sh
# 跳过：pi 不可用或没有模型凭据时本脚本会 SKIP 并以 0 退出，不阻塞 CI。
set -uo pipefail

cd "$(dirname "$0")/.." || exit 1

GO="${GO:-go}"
PI="${PI:-pi}"
PORT="${PORT:-18099}"
BASE="http://127.0.0.1:${PORT}"
WORK=".smoke-pi"

PASS=0; FAIL=0
ok(){ PASS=$((PASS+1)); printf '  \033[32mok\033[0m   %s\n' "$1"; }
bad(){ FAIL=$((FAIL+1)); printf '  \033[31mFAIL\033[0m %s\n' "$1"; [ -n "${2:-}" ] && printf '        %s\n' "$2"; }
desc(){ printf '\n\033[1m%s\033[0m\n' "$1"; }

SRV=""
cleanup(){ [ -n "$SRV" ] && kill "$SRV" 2>/dev/null; wait 2>/dev/null; rm -rf "$WORK"; }
trap cleanup EXIT

desc "前置检查"
if ! command -v "$PI" >/dev/null 2>&1; then
  echo "SKIP：未找到 pi 可执行文件（可设 PI=/path/to/pi）"; exit 0
fi
ok "pi 可用：$($PI --version 2>&1 | head -1)"

EXE=""
case "$(uname -s 2>/dev/null || echo x)" in MINGW*|MSYS*|CYGWIN*) EXE=".exe";; esac

rm -rf "$WORK"; mkdir -p "$WORK"
if ! $GO build -o "$WORK/bench-server$EXE" ./cmd/server 2>"$WORK/build.log"; then
  bad "server 编译失败" "$(cat "$WORK/build.log")"; exit 1; fi
if ! $GO build -o "$WORK/bench$EXE" ./cmd/cli 2>>"$WORK/build.log"; then
  bad "cli 编译失败" "$(cat "$WORK/build.log")"; exit 1; fi
ok "二进制已构建"

export BENCH_SECRET_KEY="3333333333333333333333333333333333333333333333333333333333333333"
SRVB="$WORK/bench-server$EXE"; BENCH="$WORK/bench$EXE"
CFG="$WORK/server.yaml"
cat > "$CFG" <<YAML
server: { addr: ":${PORT}", read_timeout: 5s, write_timeout: 10s }
store: { path: ${WORK}/server.db, migrate: true }
auth: { readonly_anonymous: true, max_clock_skew: 300s }
bandwidth: { watch_enabled: false }
moderation: { enabled: true, max_prompt_len: 8192 }
cors: { allowed_origins: ["*"] }
YAML

$SRVB -config "$CFG" -put-key "pi:pi-key:pi-secret" >"$WORK/putkey.log" 2>&1 \
  && ok "登记 Key" || bad "登记 Key 失败" "$(cat "$WORK/putkey.log")"

$SRVB -config "$CFG" -dev -log-level warn >"$WORK/server.log" 2>&1 &
SRV=$!
ready=0
for _ in $(seq 1 60); do curl -fsS "$BASE/-/healthz" >/dev/null 2>&1 && { ready=1; break; }; sleep 0.25; done
[ "$ready" = 1 ] && ok "源站已就绪" || { bad "源站未就绪" "$(tail -5 "$WORK/server.log")"; exit 1; }

# bench 自己的配置与缓存，隔离到临时目录
export BENCH_HOME="$WORK/home"
export BENCH_BIN="$BENCH"
MARKER="Pi扩展验证专用提示词标记XYZ"
$BENCH config init --endpoint "$BASE" --key pi-key --secret pi-secret >/dev/null 2>&1 \
  && ok "bench 配置完成" || bad "bench 配置失败" ""

UP="$($BENCH upload -c "请用一句话解释 CAP 定理。$MARKER" -t systems --json 2>/dev/null)"
PID=$(printf '%s' "$UP" | sed -n 's/.*"id":"\([^"]*\)".*/\1/p')
[ -n "$PID" ] && ok "上传得到 id=$PID" || bad "上传失败" "$UP"
$SRVB -config "$CFG" -approve "$PID" >/dev/null 2>&1 && ok "已审核通过" || bad "审核失败" ""
$BENCH sync >/dev/null 2>&1

EXT="plugins/pi/extension/index.ts"

desc "扩展加载（对照实验）"
# 先确认 pi 真的会报告加载失败，否则"没报错"不能当成通过
printf 'import { nopeNotARealSymbol } from "@earendil-works/pi-coding-agent";\nexport default function(){ throw new Error("probe " + nopeNotARealSymbol); }\n' > "$WORK/broken.ts"
broken_out="$(timeout 90 $PI --offline --no-session --no-extensions --no-skills --no-themes --no-context-files -e "./$WORK/broken.ts" -p "hi" 2>&1)"
if printf '%s' "$broken_out" | grep -qi 'failed to load extension'; then
  ok "对照组有效：坏扩展确实会被 pi 报错"
else
  bad "对照组失效：pi 没有报告坏扩展，本脚本的通过判定不成立" "$(printf '%s' "$broken_out" | head -3)"
  exit 1
fi

load_out="$(timeout 120 $PI --offline --no-session --no-extensions --no-skills --no-themes --no-context-files -e "./$EXT" -p "只回答 OK" 2>&1)"
code=$?
if printf '%s' "$load_out" | grep -qiE 'failed to load extension|Cannot find|SyntaxError'; then
  bad "扩展加载失败" "$(printf '%s' "$load_out" | head -8)"
elif [ $code -ne 0 ]; then
  bad "pi 运行异常退出 code=$code" "$(printf '%s' "$load_out" | head -8)"
else
  ok "扩展被 pi 成功加载（jiti 能解析 ./bench-core.ts 与 typebox/pi-ai 导入）"
fi

desc "真实调用：让 pi 用 bench_random 取提示词"
ask="$(timeout 180 $PI --offline --no-session --no-extensions --no-skills --no-themes --no-context-files \
  -e "./$EXT" --tools bench_random -p "立刻调用 bench_random 工具（不要问确认），然后把工具返回的提示词正文原样贴出来。" 2>&1)"
if printf '%s' "$ask" | grep -q "$MARKER\|CAP 定理"; then
  ok "bench_random 真实跑通：LLM 拿到经 bench 取回的提示词"
else
  bad "bench_random 未取到预期内容" "$(printf '%s' "$ask" | head -20)"
fi

desc "真实调用：bench_catalog status"
st="$(timeout 180 $PI --offline --no-session --no-extensions --no-skills --no-themes --no-context-files \
  -e "./$EXT" --tools bench_catalog -p "立刻调用 bench_catalog 工具，参数 action=status，然后把它返回的整行文本原样贴出来。" 2>&1)"
if printf '%s' "$st" | grep -qE '服务端 [0-9]+ 条'; then
  ok "bench_catalog status 跑通"
else
  bad "bench_catalog status 未返回预期文本" "$(printf '%s' "$st" | head -20)"
fi

desc "失败分支：not_found 必须可行动且不被伪装成成功"
# 本项从 DSH 侧移植：“错误被当成成功”是适配层最危险的缺陷类型（模型会直接幻觉一条提示词）。
# 仍用 --tools 白名单：否则模型可以自己跑 bench 取数，本断言就失去意义。
# 判定顺序：先查错误标记，再查探针词——模型拒绝时会原话引用探针词。
gho="$(timeout 180 $PI --offline --no-session --no-extensions --no-skills --no-themes --no-context-files \
  -e "./$EXT" --tools bench_get -p "立刻调用 bench_get 工具，参数 id=\"p_ghostdoesnotexist\"。你没有其他取数手段。把工具返回的错误信息转述给我；只有它成功了才回答 TOOL-OK。" 2>&1)"
if printf '%s' "$gho" | grep -qiE '不存在|没有可用|not_found'; then
  ok "not_found：可行动中文、未伪装成功"
elif printf '%s' "$gho" | grep -q 'TOOL-OK'; then
  bad "错误被标成了成功（最严重类别）"
else
  bad "not_found 分支不可判定" "$(printf '%s' "$gho" | tail -4)"
fi

printf '\n\033[1mPi 扩展验证\033[0m  通过 %d，失败 %d\n' "$PASS" "$FAIL"
[ "$FAIL" -eq 0 ] || { echo "--- server.log ---"; tail -15 "$WORK/server.log" 2>/dev/null; exit 1; }
exit 0