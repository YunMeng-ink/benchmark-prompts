#!/usr/bin/env bash
# DSH 插件的真实验证（结构对齐 scripts/smoke-pi.sh 的对照实验方法论）。
#
# 层次：
#   A 前置       node/git-bash/DSH 安装树，缺 → SKIP（不是通过）
#   B 单元       bench-core 副本回归 + plugin.test.ts（假 ctx 真 defineTool）+ tsc 类型检查
#   C 对照实验   故意写坏的插件必须被 DSH 报告，否则后面所有"没报错"都不作数
#   D 真实调用   headless 真会话：模型经我们的工具拿到带唯一标记的源站提示词
#   E 失败分支   not_found / bench 缺失：可行动中文、且不得被当成成功
#   F skill      customSkillDirs 指向仓库现有 SKILL.md，模型上下文里能报出名字
#
# 用法：bash scripts/smoke-dsh.sh          （git-bash；WSL bash 会被拒）
# LLM 不可用（无凭据/离线）：D/E/F SKIP 并以 0 退出，A/B/C 仍然跑——SKIP 明确区别于 PASS。
set -uo pipefail

case "$(uname 2>/dev/null)" in
  Linux*)
    echo "SKIP smoke-dsh：当前是 WSL bash，路径语义不匹配；请用 git-bash 运行（这不是通过）"
    exit 0;;
esac

cd "$(dirname "$0")/.." || exit 1

GO="${GO:-go}"
PORT="${PORT:-18098}"
BASE="http://127.0.0.1:${PORT}"
WORK=".smoke-dsh"
PASS=0; FAIL=0; SKIPN=0
ok(){ PASS=$((PASS+1)); printf '  \033[32mok\033[0m   %s\n' "$1"; }
bad(){ FAIL=$((FAIL+1)); printf '  \033[31mFAIL\033[0m %s\n' "$1"; [ -n "${2:-}" ] && printf '        %s\n' "$2"; }
skip(){ SKIPN=$((SKIPN+1)); printf '  \033[33mSKIP\033[0m %s（不是通过）\n' "$1"; }
desc(){ printf '\n\033[1m%s\033[0m\n' "$1"; }

SRV=""
DSH_PROFILE_DIR="${DSH_PROFILES:-$HOME/.dsh/profiles}"
STAGE="$DSH_PROFILE_DIR/headless/.smoke-dsh-plugin"

cleanup(){
  [ -n "$SRV" ] && kill "$SRV" 2>/dev/null
  wait 2>/dev/null
  rm -rf "$WORK"
  [ -n "$STAGE" ] && rm -rf "$STAGE"
}
trap cleanup EXIT

# ---------- A 前置 ----------
desc "A. 前置检查"
if ! command -v node >/dev/null 2>&1; then
  echo "SKIP：未找到 node（不是通过）"; exit 0
fi
DSH_JS="$DSH_PROFILE_DIR/node_modules/@deepseek-ai/dsh/lib/bin.js"
if [ ! -f "$DSH_JS" ]; then
  echo "SKIP：未找到 DSH 安装树（$DSH_JS；可设 DSH_PROFILES）——不是通过"; exit 0
fi
ok "DSH 安装树：$DSH_PROFILE_DIR"

EXE=""
case "$(uname -s 2>/dev/null || echo x)" in MINGW*|MSYS*|CYGWIN*) EXE=".exe";; esac

rm -rf "$WORK"; mkdir -p "$WORK"
if ! $GO build -o "$WORK/bench-server$EXE" ./cmd/server 2>"$WORK/build.log"; then
  bad "server 编译失败" "$(cat "$WORK/build.log")"; exit 1; fi
if ! $GO build -o "$WORK/bench$EXE" ./cmd/cli 2>>"$WORK/build.log"; then
  bad "cli 编译失败" "$(cat "$WORK/build.log")"; exit 1; fi
ok "二进制已构建"

BENCH="$WORK/bench$EXE"; SRVB="$WORK/bench-server$EXE"
export BENCH_SECRET_KEY="3333333333333333333333333333333333333333333333333333333333333333"
CFG="$WORK/server.yaml"
cat > "$CFG" <<YAML
server: { addr: ":${PORT}", read_timeout: 5s, write_timeout: 10s }
store: { path: ${WORK}/server.db, migrate: true }
auth: { readonly_anonymous: true, max_clock_skew: 300s }
bandwidth: { watch_enabled: false }
moderation: { enabled: true, max_prompt_len: 8192 }
cors: { allowed_origins: ["*"] }
YAML
$SRVB -config "$CFG" -put-key "dsh:smoke-dsh-key:smoke-dsh-secret" >"$WORK/putkey.log" 2>&1 \
  && ok "登记 Key" || bad "登记 Key 失败" "$(tail -2 "$WORK/putkey.log")"
nohup $SRVB -config "$CFG" -dev -log-level warn >"$WORK/server.log" 2>&1 &
SRV=$!
ready=0
for _ in $(seq 1 60); do curl -fsS "$BASE/-/healthz" >/dev/null 2>&1 && { ready=1; break; }; sleep 0.25; done
[ "$ready" = 1 ] && ok "源站已就绪" || { bad "源站未就绪" "$(tail -5 "$WORK/server.log")"; exit 1; }

BENCH_HOME="$WORK/home"
$BENCH config init --home "$BENCH_HOME" --endpoint "$BASE" --key smoke-dsh-key --secret smoke-dsh-secret >/dev/null 2>&1 \
  && ok "bench 配置完成（--home 隔离在 $BENCH_HOME）" || bad "bench 配置失败"
MARKER="DSH冒烟标记Qz7m$RANDOM"
UP="$($BENCH upload --home "$BENCH_HOME" -c "请解释为什么冒烟测试要先证明检测手段有效。$MARKER" -t reasoning --json 2>/dev/null)"
PID=$(printf '%s' "$UP" | sed -n 's/.*"id":"\([^"]*\)".*/\1/p')
[ -n "$PID" ] && ok "上传得到 id=$PID" || bad "上传失败" "$UP"
$SRVB -config "$CFG" -approve "$PID" >/dev/null 2>&1 && ok "已审核通过" || bad "审核失败"
$BENCH sync --home "$BENCH_HOME" >/dev/null 2>&1

# 把插件复制进 profile 树（裸 @deepseek-ai/* 导入沿目录上溯命中安装树，与 npm 包装载同构）
mkdir -p "$STAGE"
cp plugins/dsh/index.ts plugins/dsh/bench-core.ts "$STAGE/"
to_win() { local w; w=$(cygpath -w "$1" 2>/dev/null || echo "$1"); echo "$w"; }

# 关掉模型一切"绕过工具自己取数"的口子：通过=数据只能来自我们的工具
cat > "$STAGE/patch.yml" <<YAML
- insert:
    - id: bench
      name: './index.ts'
      config:
        bin: '$(to_win "$PWD/$BENCH")'
        home: '$(to_win "$PWD/$BENCH_HOME")'
- id: tool-bash
  disabled: true
- id: tool-pwsh
  disabled: true
- id: tool-jobs
  disabled: true
- id: tool-subagent
  disabled: true
- id: tool-subagent-fork
  disabled: true
- id: tool-workflow
  disabled: true
- id: tool-ralph
  disabled: true
- id: tool-fs
  disabled: true
- id: tool-fs-search
  disabled: true
- id: tool-web
  disabled: true
YAML
ok "插件已装载进 $STAGE"

# ---------- B 单元 ----------
desc "B. 单元与类型（无 LLM，确定性）"
node --test plugins/dsh/bench-core.test.ts >"$WORK/core.test.log" 2>&1 \
  && ok "bench-core（dsh 副本）测试" || bad "bench-core 测试失败" "$(tail -8 "$WORK/core.test.log")"
node --import ./scripts/dsh-module-hook.mjs --test plugins/dsh/plugin.test.ts >"$WORK/plugin.test.log" 2>&1 \
  && ok "plugin.test.ts（假 ctx + 真 defineTool，23 项）" \
  || bad "plugin.test.ts 失败" "$(tail -12 "$WORK/plugin.test.log")"
tc_out="$(bash scripts/dsh-typecheck.sh 2>&1)"; tc_code=$?
if [ $tc_code -eq 0 ]; then
  if printf '%s' "$tc_out" | grep -q '^SKIP'; then skip "typecheck-dsh（环境缺件）"; else ok "tsc noEmit 类型检查"; fi
else
  bad "tsc 类型检查失败" "$(printf '%s' "$tc_out" | tail -8)"
fi

# ---------- C 对照实验 ----------
run_headless() { # $1=patch(可空) $2=任务 → OUT/CODE；超时 240s
  local patch="$1" task="$2"
  if [ -n "$patch" ]; then
    OUT="$(timeout 240 node "$DSH_JS" --profile headless --patch "$patch" "$task" 2>"$WORK/hl.err")"
  else
    OUT="$(timeout 240 node "$DSH_JS" --profile headless "$task" 2>"$WORK/hl.err")"
  fi
  CODE=$?
}

desc "C. 对照实验：坏插件必须被报告"
cat > "$STAGE/broken.ts" <<'TS'
throw new Error('SMOKE-DSH-BROKEN-9f2k')
export {}
TS
cat > "$STAGE/broken-patch.yml" <<'YAML'
- insert:
    - id: bench-broken
      name: './broken.ts'
YAML
run_headless "$(to_win "$STAGE/broken-patch.yml")" "只回答OK"
if printf '%s%s' "$OUT" "$(cat "$WORK/hl.err" 2>/dev/null)" | grep -qiE 'failed to (import|apply|start).*bench-broken|SMOKE-DSH-BROKEN'; then
  ok "检测手段有效：DSH 确实报告坏插件（exit=$CODE）"
else
  bad "对照组失效：坏插件未被报告，本脚本其余判定不作数" "$(tail -5 "$WORK/hl.err")"
  exit 1
fi

# ---------- D/E/F 真会话（需 LLM） ----------
desc "D. LLM 可用性探测"
run_headless "" "只回答OK"
if [ $CODE -ne 0 ] || ! printf '%s' "$OUT" | grep -qi 'ok'; then
  echo "  LLM 探测失败（凭据/网络/模型配置问题）。D/E/F 全部 SKIP —— 这不是通过。"
  echo "  A/B/C 结果已给出；修好凭据后重跑本脚本。"
  skip "真实调用（random/status/score）"; skip "失败分支（not_found/bench缺失）"; skip "skill 发现"
  printf '\n\033[1mDSH 插件验证\033[0m  通过 %d，失败 %d，跳过 %d\n' "$PASS" "$FAIL" "$SKIPN"
  [ "$FAIL" -eq 0 ] || exit 1
  exit 0
fi
ok "headless LLM 可用"

desc "D. 真实调用：模型只能经我们的工具拿到源站标记"
run_headless "$(to_win "$STAGE/patch.yml")" \
  "立刻调用 bench_random 工具（不要问确认）。你没有 shell、文件或联网工具，取题的唯一途径就是这个工具。调用成功后把工具返回的提示词正文原样贴出来，一个字都不要改；调用失败就只回答 TOOL-FAILED 并附错误原文。"
if printf '%s' "$OUT" | grep -q "$MARKER" && ! printf '%s' "$OUT" | grep -q 'TOOL-FAILED'; then
  ok "bench_random：标记串只能来自源站→bench→工具→模型这条链（非模型编造）"
else
  bad "bench_random 未走通（exit=$CODE）" "$(printf '%s\n' "$OUT" | tail -6)"
fi

run_headless "$(to_win "$STAGE/patch.yml")" \
  "立刻调用 bench_catalog 工具，参数 action=status，然后把工具返回的整行文本原样贴出来。"
if printf '%s' "$OUT" | grep -qE '服务端 [0-9]+ 条'; then
  ok "bench_catalog status 跑通"
else
  bad "bench_catalog status 未返回预期文本" "$(printf '%s\n' "$OUT" | tail -6)"
fi

desc "E. 打分真实写路径 + 错误分支（可行动、不被吞）"
run_headless "$(to_win "$STAGE/patch.yml")" \
  "立刻调用 bench_score 工具，参数 id=\"$PID\", value=4。把工具返回的文本原样贴出来。"
if printf '%s' "$OUT" | grep -q '已记录' && printf '%s' "$OUT" | grep -q '均分'; then
  ok "bench_score 真实写入成功（HMAC 签名链路可用）"
else
  bad "bench_score 未走通" "$(printf '%s\n' "$OUT" | tail -6)"
fi

run_headless "$(to_win "$STAGE/patch.yml")" \
  "立刻调用 bench_get 工具，参数 id=\"p_ghostdoesnotexist\"。你没有其他取数手段。把工具返回的错误信息转述给我；只有它成功了才回答 TOOL-OK。"
# 判定顺序很关键：**先查错误标记，再查探针词**。
# 模型拒绝得很对时，它会原话引用探针词（“因此不能回答 TOOL-OK”）——
# 若先 grep TOOL-OK，一个行为完全正确的插件会被判为“把错误当成成功”。
# 反过来仍然有诊断力：真发生了“错误被吞”，模型手里就没有错误文本可引，
# 它只会给出 TOOL-OK，仍会落到第二个分支。
if printf '%s' "$OUT" | grep -qiE '不存在|没有可用|not_found'; then
  ok "not_found：可行动中文、未伪装成功"
elif printf '%s' "$OUT" | grep -q 'TOOL-OK'; then
  bad "错误被标成了成功（最严重类别）"
else
  bad "not_found 分支不可判定" "$(printf '%s\n' "$OUT" | tail -4)"
fi

cat > "$STAGE/missing-patch.yml" <<YAML
- insert:
    - id: bench
      name: './index.ts'
      config:
        bin: 'bench-does-not-exist-9f2k'
        home: '$(to_win "$PWD/$BENCH_HOME")'
- id: tool-bash
  disabled: true
- id: tool-pwsh
  disabled: true
- id: tool-jobs
  disabled: true
- id: tool-subagent
  disabled: true
- id: tool-subagent-fork
  disabled: true
- id: tool-workflow
  disabled: true
- id: tool-ralph
  disabled: true
- id: tool-fs
  disabled: true
- id: tool-fs-search
  disabled: true
- id: tool-web
  disabled: true
YAML
run_headless "$(to_win "$STAGE/missing-patch.yml")" \
  "立刻调用 bench_random 工具。成功只回答 TOOL-OK；失败把工具错误信息原样转述。"
if printf '%s' "$OUT" | grep -q '找不到 bench 可执行文件' && printf '%s' "$OUT" | grep -q 'make build-cli'; then
  ok "bench 缺失：给出可照做的安装指引（make build-cli / BENCH_BIN）"
else
  bad "bench 缺失分支不可判定" "$(printf '%s\n' "$OUT" | tail -4)"
fi

desc "F. skill 发现（零代码复用）"
cat > "$STAGE/skill-patch.yml" <<YAML
- insert:
    - id: bench
      name: './index.ts'
      config:
        bin: '$(to_win "$PWD/$BENCH")'
        home: '$(to_win "$PWD/$BENCH_HOME")'
- id: skill-filesystem
  config:
    customSkillDirs: ['$(to_win "$PWD/plugins/pi/skill")']
- id: tool-bash
  disabled: true
- id: tool-pwsh
  disabled: true
- id: tool-fs
  disabled: true
- id: tool-fs-search
  disabled: true
YAML
run_headless "$(to_win "$STAGE/skill-patch.yml")" \
  "你的上下文里有哪些可用的 skill？把每个 skill 的名字逐字列出来（只列名字，一行一个）。如果一个都没有，只回答 NONE。"
if printf '%s' "$OUT" | grep -q 'benchmark-testing'; then
  ok "pi 侧 SKILL.md 被 DSH 发现（同一份文件两框架通用）"
else
  bad "skill 未发现" "$(printf '%s\n' "$OUT" | tail -4)"
fi

desc "汇总"
printf '\033[1mDSH 插件验证\033[0m  通过 %d，失败 %d，跳过 %d\n' "$PASS" "$FAIL" "$SKIPN"
[ "$FAIL" -eq 0 ] || { echo "--- server.log ---"; tail -15 "$WORK/server.log" 2>/dev/null; exit 1; }
exit 0
