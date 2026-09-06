#!/usr/bin/env bash
# Linux 端到端预部署验证：用即将上服务器的同一个 linux/amd64 二进制，
# 在真实 Linux 文件系统上验证 API + 静态托管 + 可信代理取真实 IP。
# 只在 WSL/Linux 上跑，不进 CI 门禁（Windows 主机上无法执行 ELF）。
set -uo pipefail

BIN="${BENCH_VERIFY_BIN:-./dist/bench-server-linux-amd64}"
WEB="${BENCH_VERIFY_WEB:-./web/dist}"
ROOT="${BENCH_VERIFY_ROOT:-$HOME/bt}"
PORT="${BENCH_VERIFY_PORT:-18099}"

# 在 Windows/MSYS 上直接执行 ELF 是不可能的：能转 WSL 就转，不能就如实失败，
# 绝不把“没跑成”说成“跑过了”。
if [ "$(uname -s)" != "Linux" ]; then
	if command -v wsl >/dev/null 2>&1; then
		echo "非 Linux 主机，转交 WSL 执行"
		exec wsl -- bash scripts/verify-linux.sh
	fi
	echo "需要 Linux（或 WSL）才能执行 linux 二进制，当前 $(uname -s)" >&2
	exit 125
fi
PASS=0
FAIL=0

ok() { printf '  \033[32mok\033[0m   %s\n' "$1"; PASS=$((PASS + 1)); }
no() { printf '  \033[31mFAIL\033[0m %s\n' "$1"; FAIL=$((FAIL + 1)); }
has() {
	# has <说明> <实际> <期望子串>
	if printf '%s' "$2" | grep -qF -- "$3"; then ok "$1"; else no "$1（缺少 $3，实际：$(printf '%s' "$2" | head -c 160)）"; fi
}

rm -rf "$ROOT" && mkdir -p "$ROOT/data" "$ROOT/web"
cp -r "$WEB/." "$ROOT/web/"
ABS_BIN=$(realpath "$BIN")

cat >"$ROOT/config.yaml" <<YAML
server:
  addr: "127.0.0.1:$PORT"
  static_dir: "$ROOT/web"
  trusted_proxies: ["127.0.0.0/8"]
store:
  path: "$ROOT/data/bench.db"
bandwidth:
  max_mbps: 8.0
  watch_enabled: false
moderation:
  max_prompt_len: 8192
YAML

export BENCH_SECRET_KEY=000102030405060708090a0b0c0d0e0f101112131415161718191a1b1c1d1e1f
"$ABS_BIN" -config "$ROOT/config.yaml" -dev >"$ROOT/server.log" 2>&1 &
PID=$!
trap 'kill $PID 2>/dev/null' EXIT

for _ in $(seq 1 40); do
	if curl -fsS -o /dev/null "http://127.0.0.1:$PORT/-/healthz"; then break; fi
	sleep 0.25
done

B="http://127.0.0.1:$PORT"
echo "── Linux 端到端（$ABS_BIN）──"

if curl -fsS "$B/-/healthz" >/dev/null; then ok "healthz 200"; else no "healthz 不可达"; fi

H=$(curl -sS -D- -o /dev/null "$B/")
has "入口 index.html 返回 200" "$H" "HTTP/1.1 200"
has "入口 HTML 是 no-cache 级" "$H" "must-revalidate"
has "Content-Type 是 HTML" "$H" "text/html"

ASSET=$(cd "$ROOT/web" && find ./_astro -name '*.js' | head -1)
if [ -n "$ASSET" ]; then
	A=$(curl -sS -D- -o /dev/null "$B/${ASSET#./}")
	has "内容寻址资源 immutable 一年" "$A" "immutable"
else
	no "web/dist 里没有 _astro 资源，无法验证 immutable 层"
fi

J=$(curl -sS "$B/v1/meta")
has "API 仍返回信封 JSON" "$J" '"data"'

N404=$(curl -sS -D- "$B/v1/does-not-exist")
has "未知 API 路径不被静态兜底吞掉" "$N404" "not_found"
if printf '%s' "$N404" | grep -qF '<!doctype html>'; then no "未知 API 路径返回了 HTML"; else ok "未知 API 路径没有返回 HTML"; fi

# 可信代理 → 采信 X-Forwarded-For；日志里应出现被代理追加的真实地址。
curl -sS -o /dev/null -H 'X-Forwarded-For: 203.0.113.77' "$B/v1/prompts?limit=1"
sleep 0.3
if grep -qE 'ip["=:, ]+203\.0\.113\.77' "$ROOT/server.log"; then
	ok "回环对端 + 转发头 → 审计与限流按真实客户端 IP"
else
	no "转发头没被采信（日志里没有 ip=203.0.113.77）"
fi

# 配成“什么都不信任”时，转发头必须被忽略。改配置重启一次。
kill $PID 2>/dev/null
wait $PID 2>/dev/null
sed -i 's|trusted_proxies: \["127.0.0.0/8"\]|trusted_proxies: ["240.0.0.0/4"]|' "$ROOT/config.yaml"
: >"$ROOT/server.log"
"$ABS_BIN" -config "$ROOT/config.yaml" -dev >"$ROOT/server.log" 2>&1 &
PID=$!
for _ in $(seq 1 40); do
	if curl -fsS -o /dev/null "$B/-/healthz"; then break; fi
	sleep 0.25
done
curl -sS -o /dev/null -H 'X-Forwarded-For: 203.0.113.78' "$B/v1/prompts?limit=1"
sleep 0.3
# 必须先有“请求真的被处理了”的正向证据，否则“日志里没有伪造 IP”只是空断言。
if ! grep -qE 'path=/v1/prompts status=[0-9]' "$ROOT/server.log"; then
	no "第二条服务未起来或请求未送达 —— 不是通过"
elif grep -qE 'ip["=:, ]+203\.0\.113\.78' "$ROOT/server.log"; then
	no "对端不可信时仍然采信了转发头（可被伪造绕限流）"
elif ! grep -qE 'ip["=:, ]+127\.0\.0\.1' "$ROOT/server.log"; then
	no "既没采信转发头也没记直连地址，日志里没有可判定的 ip 字段"
else
	ok "对端不可信时忽略伪造的转发头，按直连地址记"
fi

# ── 阶段 3：带真实 CDN 回源网段（deploy/trusted-proxies.cdn.yaml）──────
CDN_FRAG="${BENCH_VERIFY_CDN:-deploy/trusted-proxies.cdn.yaml}"
if [ ! -f "$CDN_FRAG" ]; then
	printf '  \033[33mskip\033[0m 找不到 %s，跳过 CDN 链阶段（不是通过）\n' "$CDN_FRAG"
else
	# 用片段里的第一条非回环 IPv4 网段的**网络地址本身**充当 CDN 节点（它必然落在段内）
	CDN_NET=$(sed -n 's|^  - "\([0-9][0-9.]*\(/[0-9]*\)\)".*|\1|p' "$CDN_FRAG" | grep -v '^127\.' | head -1)
	CDN_NODE=${CDN_NET%%/*}
	if [ -z "$CDN_NODE" ]; then
		no "片段里没有非回环 IPv4 网段，无法构造 CDN 链"
	else
		kill $PID 2>/dev/null
		wait $PID 2>/dev/null
		# 把片段整段写进配置（YAML 流式序列）
		LIST=$(sed -n 's|^  - "\([^"]*\)".*|"\1"|p' "$CDN_FRAG" | paste -sd, -)
		sed -i "s|^  trusted_proxies:.*|  trusted_proxies: [${LIST}]|" "$ROOT/config.yaml"
		: >"$ROOT/server.log"
		"$ABS_BIN" -config "$ROOT/config.yaml" -dev >"$ROOT/server.log" 2>&1 &
		PID=$!
		for _ in $(seq 1 40); do
			if curl -fsS -o /dev/null "$B/-/healthz"; then break; fi
			sleep 0.25
		done
		# 链 = 真实客户端, CDN 节点；直连对端是回环 nginx
		curl -sS -o /dev/null -H "X-Forwarded-For: 203.0.113.200, ${CDN_NODE}" "$B/v1/prompts?limit=1"
		sleep 0.3
		if ! grep -qE 'path=/v1/prompts status=[0-9]' "$ROOT/server.log"; then
			no "第三条服务没起来 —— 用真实 CDN 网段配 trust 后无法启动（不是通过）"
		elif grep -qE "ip[\"=:, ]+${CDN_NODE//./\\.}" "$ROOT/server.log"; then
			no "把 CDN 节点 ${CDN_NODE} 当成了客户端：网段没生效或链没继续左移"
		elif grep -qE 'ip["=:, ]+203\.0\.113\.200' "$ROOT/server.log"; then
			ok "CDN 在链上时左移到真实客户端 IP（$(grep -c '^  - "' "$CDN_FRAG") 条网段可解析）"
		else
			no "既没记 CDN 节点也没记真实客户端，无法判定"
		fi
	fi
fi

echo
printf '\033[1mLinux 端到端结果\033[0m  通过 %d，失败 %d\n' "$PASS" "$FAIL"
[ "$FAIL" -eq 0 ]
