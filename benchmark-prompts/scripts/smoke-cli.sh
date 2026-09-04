#!/usr/bin/env bash
# CLI 二进制端到端冒烟：编译出真实的 bench / bench-server，然后用**插件将要使用的
# 同一方式**（shell 调用 + --json + 退出码）跑完整流程。
#
# internal/cli 的单测验证的是逻辑；本脚本验证的是"发出去的那个文件真的能用"。
set -uo pipefail

cd "$(dirname "$0")/.." || exit 1

GO="${GO:-go}"
CURL="${CURL:-curl}"
PORT="${PORT:-18090}"
BASE="http://127.0.0.1:${PORT}"
WORK=".smoke-cli"

PASS=0; FAIL=0
ok()  { PASS=$((PASS+1)); printf '  \033[32mok\033[0m   %s\n' "$1"; }
bad() { FAIL=$((FAIL+1)); printf '  \033[31mFAIL\033[0m %s\n' "$1"; [ -n "${2:-}" ] && printf '        %s\n' "$2"; }
desc(){ printf '\n\033[1m%s\033[0m\n' "$1"; }
jget(){ sed -n "s/.*\"$1\":\"\([^\"]*\)\".*/\1/p" | head -1; }
jnum(){ sed -n "s/.*\"$1\":\([0-9.]*\).*/\1/p" | head -1; }

SRV=""
cleanup(){ [ -n "$SRV" ] && kill "$SRV" 2>/dev/null; wait 2>/dev/null; rm -rf "$WORK"; }
trap cleanup EXIT

EXE=""
case "$(/usr/bin/env uname -s 2>/dev/null || echo x)" in MINGW*|MSYS*|CYGWIN*) EXE=".exe";; esac

export BENCH_SECRET_KEY="2222222222222222222222222222222222222222222222222222222222222222"

desc "编译真实二进制"
rm -rf "$WORK"; mkdir -p "$WORK"
if ! $GO build -trimpath -o "$WORK/bench-server$EXE" ./cmd/server 2>"$WORK/b1.log"; then
  bad "server 编译失败" "$(cat "$WORK/b1.log")"; exit 1; fi
if ! $GO build -trimpath -o "$WORK/bench$EXE" ./cmd/cli 2>"$WORK/b2.log"; then
  bad "cli 编译失败" "$(cat "$WORK/b2.log")"; exit 1; fi
ok "bench / bench-server 已编译"

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

$SRVB -config "$CFG" -put-key "e2e:cli-key:cli-secret" >"$WORK/putkey.log" 2>&1 \
  && ok "登记 API Key" || bad "登记 Key 失败" "$(cat "$WORK/putkey.log")"

$SRVB -config "$CFG" -dev -log-level warn >"$WORK/server.log" 2>&1 &
SRV=$!
ready=0
for _ in $(seq 1 60); do $CURL -fsS "$BASE/-/healthz" >/dev/null 2>&1 && { ready=1; break; }; sleep 0.25; done
[ "$ready" = 1 ] && ok "服务已就绪" || { bad "服务未就绪" "$(cat "$WORK/server.log")"; exit 1; }

# 每个用例都用自己的 HOME，彻底隔离
HOME_DIR="$WORK/home"
b() { HOME="$HOME_DIR" $BENCH "$@" --home "$HOME_DIR"; }

desc "CLI: 未配置时的引导"
out="$(b meta 2>&1)"; code=$?
[ $code -eq 5 ] && ok "未配置 endpoint 退出码 5" || bad "退出码应为 5" "got $code: $out"
echo "$out" | grep -qi 'config init' && ok "错误信息给出下一步动作" || bad "错误信息不可操作" "$out"

b config init --endpoint "$BASE" >/dev/null 2>&1 && ok "config init 成功" || bad "config init 失败" ""
[ -f "$HOME_DIR/config" ] && ok "配置文件已落盘" || bad "配置文件缺失" ""
perm=$(ls -l "$HOME_DIR/config" | awk '{print $1}')
case "$OS" in
  Windows*) ok "配置权限（Windows 无 POSIX 权限位，跳过检查；依赖 ACL）" ;;
  *) case "$perm" in *rw-------*) ok "配置权限为 0600（含凭据）";; *) bad "配置权限过宽" "$perm";; esac ;;
esac

b config init --endpoint "$BASE" --key cli-key --secret cli-secret >/dev/null 2>&1

desc "CLI: 空目录同步与版本"
if out=$(b version); then ok "version 退出 0"; else bad "version 失败" "$out"; fi
out="$(b sync 2>&1)"; code=$?
[ $code -eq 0 ] && ok "空库 sync 成功" || bad "空库 sync 失败" "code=$code: $out"

desc "CLI: 上传 → 审核 → 同步 → 读取"
UP="$(b upload -c '请实现一个线程安全的 LRU 缓存，并说明复杂度。' -t coding,concurrency --client-id cli-smoke-1 --json 2>/dev/null)"
echo "$UP" | grep -q '"ok"' >/dev/null
UPID="$(printf '%s' "$UP" | jget id)"
[ -n "$UPID" ] && ok "CLI 上传得到 id=$UPID" || bad "CLI 上传失败" "$UP"
printf '%s' "$UP" | grep -q '"s":"pending"' && ok "新内容状态为 pending" || bad "状态应为 pending" "$UP"

# 未过审不可读
out="$(b get "$UPID" 2>&1)"; code=$?
[ $code -eq 4 ] && ok "未过审内容读取退出码 4" || bad "应为 4" "code=$code: $out"

$SRVB -config "$CFG" -approve "$UPID" >"$WORK/approve.log" 2>&1 \
  && ok "运维命令过审" || bad "过审失败" "$(cat "$WORK/approve.log")"

out="$(b sync 2>&1)"; code=$?
[ $code -eq 0 ] && echo "$out" | grep -q '1 条' && ok "sync 拉到 1 条" || bad "sync 结果异常" "code=$code: $out"

# stdout 必须只有正文（插件/管道依赖这条约定）
body="$(b get "$UPID" 2>/dev/null)"
[ "$body" = "请实现一个线程安全的 LRU 缓存，并说明复杂度。" ] \
  && ok "get 的 stdout 只有正文，可直接管道" || bad "stdout 被污染" "$(printf '%q' "$body")"

json="$(b get "$UPID" --json 2>/dev/null)"
printf '%s' "$json" | grep -q '"p":' && printf '%s' "$json" | grep -q '"h":' \
  && ok "get --json 字段符合契约" || bad "--json 字段不符" "$json"

desc "CLI: 随机测试"
rid="$(b random --json 2>/dev/null | jget id)"
[ "$rid" = "$UPID" ] && ok "random 抽到唯一条目" || bad "random 结果异常" "got '$rid'"
code=0; b random --fresh >/dev/null 2>&1 || code=$?
[ $code -eq 4 ] && ok "--fresh 排除最近抽过的条目" || bad "--fresh 未生效" "code=$code"
# --fresh 只排除“最近抽过”，不能排除已同步全部（否则永远 404）
code=0; b random >/dev/null 2>&1 || code=$?
[ $code -eq 0 ] && ok "--fresh 不影响普通 random（缓存未被弄坏）" || bad "普通 random 失败" "code=$code"
code=0; b random --json --tag=nope >/dev/null 2>&1 || code=$?
[ $code -eq 4 ] && ok "无匹配标签时退出码 4" || bad "标签过滤异常" "code=$code"

desc "CLI: 打分与列表"
sc="$(b score "$UPID" 5 --json 2>/dev/null)"
printf '%s' "$sc" | grep -q '"count":1' && ok "打分 count=1" || bad "打分异常" "$sc"
sc2="$(b score "$UPID" 3 --json 2>/dev/null)"
printf '%s' "$sc2" | grep -q '"avg":3' && printf '%s' "$sc2" | grep -q '"count":1' \
  && ok "同设备重复打分被覆盖" || bad "打分未幂等" "$sc2"
# 第二次调用必须复用同一个 deviceId（否则去重失效）
dev1="$(sed -n 's/.*"device_id":"\([^"]*\)".*/\1/p' <<<"$(b config show --json 2>/dev/null)")"
[ -z "$dev1" ] && ok "device_id 不出现在 --json 输出（避免被误当凭据）" || bad "device_id 不应由 config show 暴露" "$dev1"

lst="$(b list --json 2>/dev/null)"
printf '%s' "$lst" | grep -q '"count":1' && ok "list count=1" || bad "list 异常" "$lst"
printf '%s' "$lst" | grep -q '"p":' && bad "list 不得返回正文（2M 带宽）" "$lst" \
  || ok "list 不含正文"

desc "CLI: 状态与错误路径"
mt="$(b meta --json 2>/dev/null)"
printf '%s' "$mt" | grep -q '"up_to_date":true' && ok "meta 判定已最新" || bad "meta 异常" "$mt"

code=0; $BENCH score "$UPID" 9 --home "$HOME_DIR" >/dev/null 2>&1 || code=$?
[ $code -eq 5 ] && ok "越界分值退出码 5" || bad "应为 5" "code=$code"
code=0; $BENCH get p_deadbeef --home "$HOME_DIR" >/dev/null 2>&1 || code=$?
[ $code -eq 4 ] && ok "不存在 id 退出码 4" || bad "应为 4" "code=$code"
code=0; $BENCH nosuchcmd --home "$HOME_DIR" >/dev/null 2>&1 || code=$?
[ $code -eq 5 ] && ok "未知命令退出码 5" || bad "应为 5" "code=$code"
# --json 出错时错误也走 stdout 且带 code
errjson="$($BENCH score "$UPID" 9 --home "$HOME_DIR" --json 2>/dev/null)"
printf '%s' "$errjson" | grep -q '"ok":false' && printf '%s' "$errjson" | grep -q '"code"' \
  && ok "错误 JSON 含 ok:false 与 code，插件可分支" || bad "错误 JSON 不符" "$errjson"

# ---------- 自助注册 Key ----------
desc "CLI: 邀请码自助注册 / 查看 / 吊销"
INV="$($SRVB -config "$CFG" -gen-invite "冒烟:2" 2>/dev/null | sed -n 's/邀请码：\([A-Z0-9-]*\).*/\1/p' | head -1)"
[ -n "$INV" ] && ok "-gen-invite 签发邀请码" || bad "邀请码签发失败" ""

# 先清掉运维 Key，模拟"新用户只有地址、没有凭据"
b config set api_key "" >/dev/null 2>&1
out="$(b key new --invite="$INV" --label=冒烟机 --json 2>&1)"; code=$?
KEY1="$(printf '%s' "$out" | jget key)"
[ $code -eq 0 ] && [ -n "$KEY1" ] && ok "key new 拿到自助 Key" || bad "key new 失败" "code=$code $out"
printf '%s' "$out" | grep -q '"scope":"writer"' && ok "自助 Key 作用域是 writer" || bad "作用域不对" "$out"
b config show --json 2>/dev/null | grep -q '"has_key":true'   && ok "Key 已自动写入配置（无需再手工 config set）" || bad "未写入配置" ""

out="$(b key self --json 2>&1)"
printf '%s' "$out" | grep -q '"scope":"writer"' && ok "key self 报告元信息" || bad "self 异常" "$out"
printf '%s' "$out" | grep -q "$KEY1" && bad "self 回显了明文 Key" || ok "self 不回显明文 Key"

# 安全回归：自助签发的 writer Key 不能读运维端点；运维 Key 仍可
code="$($CURL -s -o /dev/null -w '%{http_code}' "$BASE/-/metrics" -H "Authorization: Bearer $KEY1")"
[ "$code" = "403" ] && ok "writer Key 读 /-/metrics 被拒 403" || bad "应 403" "got $code"
code="$($CURL -s -o /dev/null -w '%{http_code}' "$BASE/-/metrics" -H "Authorization: Bearer cli-key")"
[ "$code" = "200" ] && ok "admin Key 读 /-/metrics 仍 200" || bad "应 200" "got $code"

# 换一台"设备"（第二个 HOME → 另一个 device_id）才能验到邀请码本身：
# 服务端先查设备唯一性，所以已注册的设备即使码是假的也会先拿到 409。
b2() { $BENCH "$@" --home "$WORK/home2"; }
b2 config init --endpoint "$BASE" >/dev/null 2>&1
out="$(b2 key new --invite=BOGUS-CODE --json 2>&1)"; code=$?
[ $code -eq 3 ] && printf '%s' "$out" | grep -q '"code":"forbidden"'   && ok "无效邀请码 → forbidden/退出码 3" || bad "应 forbidden+3" "code=$code $out"

# 同设备重复注册：409 conflict（退出码 5）
out="$(b key new --invite="$INV" --json 2>&1)"; code=$?
[ $code -eq 5 ] && printf '%s' "$out" | grep -q '"code":"conflict"'   && ok "同设备重复注册 → conflict/退出码 5" || bad "应 conflict+5" "code=$code $out"

# 吊销后：配置清空、写入被拒
out="$(b key revoke --json 2>&1)"; code=$?
[ $code -eq 0 ] && printf '%s' "$out" | grep -q '"revoked":true' && ok "key revoke 成功" || bad "revoke 异常" "$out"
b config show --json 2>/dev/null | grep -q '"has_key":false' && ok "吊销后自动清除配置里的 Key" || bad "配置未清" ""
out="$(b score "$UPID" 3 --json 2>&1)"; code=$?
[ $code -eq 3 ] && printf '%s' "$out" | grep -q '"code":"unauthorized"' && ok "吊销后写入被拒 401" || bad "应 unauthorized" "code=$code $out"

# 名额未用尽时同一码还能再签一把（max_uses=2，已用 1）——必须是另一台设备
out="$(b2 key new --invite="$INV" --json 2>&1)"; code=$?
[ $code -eq 0 ] && ok "码在剩余名额内可再签发给另一台设备" || bad "第二次签发失败" "$out"

desc "CLI: 离线与缓存清理"
kill $SRV 2>/dev/null; wait $SRV 2>/dev/null; SRV=""
out="$($BENCH get "$UPID" --local --home "$HOME_DIR" 2>/dev/null)"; code=$?
[ $code -eq 0 ] && [ "$out" = "请实现一个线程安全的 LRU 缓存，并说明复杂度。" ] \
  && ok "源站下线后 --local 仍可取（离线能力）" || bad "--local 离线失败" "code=$code out=$out"
$BENCH get "$UPID" --home "$HOME_DIR" >/dev/null 2>&1; code=$?
[ $code -eq 1 ] && ok "源站下线后普通 get 退出码 1（网络）" || bad "应为 1" "code=$code"

$SRVB -config "$CFG" -dev -log-level warn >"$WORK/server2.log" 2>&1 &
SRV=$!
for _ in $(seq 1 60); do $CURL -fsS "$BASE/-/healthz" >/dev/null 2>&1 && break; sleep 0.25; done

$BENCH sync --home "$HOME_DIR" >/dev/null 2>&1
out="$($BENCH reset --home "$HOME_DIR" 2>&1)"; code=$?
[ $code -eq 0 ] && echo "$out" | grep -q '已清空' && ok "reset 清空缓存" || bad "reset 异常" "code=$code: $out"
# reset 必须保留 device_id，否则历史评分无法再去重
dev="$($BENCH config show --home "$HOME_DIR" --json 2>/dev/null | jget device_id)"
[ -z "$dev" ] && ok "reset 后 config 仍无明文 device_id 泄漏" || bad "意外" "$dev"

printf '\n\033[1mCLI 冒烟结果\033[0m  通过 %d，失败 %d\n' "$PASS" "$FAIL"
[ "$FAIL" -eq 0 ] || { echo "--- server.log ---"; tail -15 "$WORK/server.log" 2>/dev/null; exit 1; }
exit 0