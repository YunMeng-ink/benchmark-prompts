/**
 * bench-core 的单元测试 + 与**真实 bench 二进制**的集成测试。
 *
 * 跑法：
 *   node --test plugins/pi/extension/
 * 或在构建 CLI 后带上真实二进制：
 *   make build-cli && BENCH_BIN=dist/bench.exe node --test plugins/pi/extension/
 */

import assert from "node:assert/strict";
import { execFile } from "node:child_process";
import { existsSync, mkdtempSync, readFileSync } from "node:fs";
import { tmpdir } from "node:os";
import { dirname, join } from "node:path";
import { test } from "node:test";
import { fileURLToPath } from "node:url";
import { promisify } from "node:util";

import {
	assertId,
	assertScoreValue,
	BenchError,
	buildArgs,
	decodeBench,
	type ExecResult,
	ensureBench,
	exitCodeLabel,
	formatStatus,
	resolveBinary,
	truncate,
} from "./bench-core.ts";

const run = promisify(execFile);
const here = dirname(fileURLToPath(import.meta.url));

const ok = (stdout: string, code = 0): ExecResult => ({ stdout, stderr: "", code });
const envelope = (data: unknown) => JSON.stringify({ ok: true, data, error: null, cursor: null, v: 1 });

// ---------- argv 构造 ----------

test("random 构造精确 argv", () => {
	assert.deepEqual(buildArgs("random", { tag: "coding" }), ["random", "--json", "--tag", "coding"]);
	assert.deepEqual(buildArgs("random", { fresh: true }), ["random", "--json", "--fresh"]);
	assert.deepEqual(buildArgs("random", { exclude: ["p_1", "p_2"] }), ["random", "--json", "--exclude", "p_1,p_2"]);
});

test("空值与 false 不产生残缺参数", () => {
	// 关键：--tag "" 会让服务端按空标签过滤，--exclude "" 会传一个空串
	assert.deepEqual(buildArgs("random", { tag: "", exclude: [], fresh: false }), ["random", "--json"]);
});

test("get / score 的选项在前、位置参数在后", () => {
	assert.deepEqual(buildArgs("get", { id: "p_1" }), ["get", "--json", "p_1"]);
	assert.deepEqual(buildArgs("get", { id: " p_1 ", local: true }), ["get", "--json", "--local", "p_1"]);
	assert.deepEqual(buildArgs("score", { id: "p_1", value: 5 }), ["score", "--json", "p_1", "5"]);
});

test("buildArgs 在拼命令时就拦下非法入参", () => {
	// 不把畸形命令（缺位置参数）交给 bench，能省一次进程启动与一段难懂的报错
	assert.throws(
		() => buildArgs("get", {}),
		(e: BenchError) => e.code === "bad_request",
	);
	assert.throws(
		() => buildArgs("get", { id: "  " }),
		(e: BenchError) => e.code === "bad_request",
	);
	assert.throws(
		() => buildArgs("score", { id: "p_1", value: 9 }),
		(e: BenchError) => e.code === "validation_failed",
	);
	assert.throws(
		() => buildArgs("score", { id: "p_1" }),
		(e: BenchError) => e.code === "validation_failed",
	);
});

test("list / upload / sync / meta 的参数映射", () => {
	assert.deepEqual(buildArgs("list", { tag: "a", limit: 10, all: true }), [
		"list",
		"--json",
		"--tag",
		"a",
		"--limit",
		"10",
		"--all",
	]);
	assert.deepEqual(buildArgs("upload", { content: "文本", tags: ["a", "b"], clientId: "c1" }), [
		"upload",
		"--json",
		"-c",
		"文本",
		"-t",
		"a,b",
		"--client-id",
		"c1",
	]);
	assert.deepEqual(buildArgs("upload", { file: "p.md" }), ["upload", "--json", "-f", "p.md"]);
	assert.deepEqual(buildArgs("sync"), ["sync", "--json"]);
	assert.deepEqual(buildArgs("meta"), ["meta", "--json"]);
});

test("home 追加在末尾且不覆盖 --json", () => {
	assert.deepEqual(buildArgs("meta", { home: "/tmp/h" }), ["meta", "--json", "--home", "/tmp/h"]);
	assert.deepEqual(buildArgs("meta", { home: "  " }), ["meta", "--json"]);
});

test("未知子命令立即报错，不去拼畸形命令", () => {
	assert.throws(
		() => buildArgs("frobnicate"),
		(e: unknown) => e instanceof BenchError && e.code === "bad_command",
	);
});

// ---------- 入参校验 ----------

test("assertScoreValue 与 bench 的 1-5 校验一致", () => {
	assert.equal(assertScoreValue(1), 1);
	assert.equal(assertScoreValue("5"), 5);
	for (const bad of [0, 6, -1, NaN, "abc", null, undefined, "3.5"]) {
		assert.throws(() => assertScoreValue(bad), BenchError, `应拒绝 ${String(bad)}`);
	}
});

test("assertId 拒绝空白 id", () => {
	assert.equal(assertId(" p_1 ", "bench_get"), "p_1");
	for (const bad of ["", "   ", null, undefined, 42]) {
		assert.throws(
			() => assertId(bad, "bench_get"),
			(e: BenchError) => e.code === "bad_request",
		);
	}
});

// ---------- 退出码与错误映射 ----------

test("退出码标签覆盖全部约定值", () => {
	assert.equal(exitCodeLabel(0), "bench 退出码 0");
	for (const [code, label] of [
		[1, "网络或服务端故障"],
		[2, "被限流，稍后重试"],
		[3, "鉴权失败，需要配置 API Key"],
		[4, "资源不存在"],
		[5, "参数或校验错误"],
	] as const) {
		assert.equal(exitCodeLabel(code), label);
	}
});

test("decodeBench 成功路径取出 data", () => {
	const data = decodeBench(ok(envelope({ id: "p_1", p: "正文" })), "bench", ["get"]);
	assert.deepEqual(data, { id: "p_1", p: "正文" });
});

test("decodeBench 把非零退出码翻译成带原因的 BenchError", () => {
	const err = {
		ok: false,
		data: null,
		error: { code: "rate_limited", message: "慢一点" },
		cursor: null,
		v: 1,
	};
	let thrown: BenchError | undefined;
	try {
		decodeBench({ stdout: JSON.stringify(err), stderr: "", code: 2 }, "bench", ["score"]);
	} catch (e) {
		thrown = e as BenchError;
	}
	assert.ok(thrown instanceof BenchError);
	assert.equal(thrown.code, "rate_limited");
	assert.equal(thrown.exitCode, 2);
	assert.equal(thrown.retryable, true, "限流应可重试");
	assert.match(thrown.message, /被限流/);
	assert.match(thrown.message, /慢一点/);
});

test("decodeBench 在无 JSON 时回退到 stderr 末行", () => {
	assert.throws(
		() => decodeBench({ stdout: "", stderr: "line1\n真正的原因\n", code: 1 }, "bench", ["sync"]),
		(e: BenchError) => e.code === "unknown" && /真正的原因/.test(e.message),
	);
});

test("decodeBench 拒绝无法解析的成功输出", () => {
	assert.throws(
		() => decodeBench(ok("<html>网关错误页</html>"), "bench", ["meta"]),
		(e: BenchError) => e.code === "bad_response" && /--json/.test(e.message),
	);
});

test("decodeBench 抓住 code=0 但 ok=false 的不一致响应", () => {
	const weird = JSON.stringify({ ok: false, error: { code: "internal", message: "x" } });
	assert.throws(
		() => decodeBench(ok(weird), "bench", ["meta"]),
		(e: BenchError) => e.code === "internal" && e.exitCode === 5,
	);
});

test("killed 视为取消而非数据错误", () => {
	assert.throws(
		() => decodeBench({ stdout: "", stderr: "", code: 0, killed: true }, "bench", ["sync"]),
		(e: BenchError) => e.code === "canceled" && e.retryable,
	);
});

// ---------- 二进制探测 ----------

test("ensureBench 用 version 而不是裸 --json 做探测", async () => {
	// 回归：曾经探测命令写成 `bench --json`，bench 把 --json 当未知命令退 5，
	// 导致完全正常的二进制被误判为版本过旧。这里钉住探测参数。
	const seen: string[][] = [];
	await ensureBench(async (_bin, args) => {
		seen.push(args);
		return ok(envelope({ version: "dev" }));
	}, "bench");
	assert.deepEqual(seen, [["version", "--json"]]);
});

test("ensureBench 能识别 PATH 上的同名不相干命令", async () => {
	// 比如 PATH 上先命中了另一个叫 bench 的工具，它退了 0 但不输出 bench 信封
	await assert.rejects(
		() => ensureBench(async () => ({ stdout: "some other bench help", stderr: "", code: 0 }), "bench"),
		(e: BenchError) => e.code === "bench_wrong_binary" && /BENCH_BIN/.test(e.message),
	);
});

test("ensureBench 在缺二进制时给出可操作的指引", async () => {
	let thrown: BenchError | undefined;
	try {
		await ensureBench(async () => {
			throw new Error("ENOENT bench");
		}, "bench");
	} catch (e) {
		thrown = e as BenchError;
	}
	assert.ok(thrown instanceof BenchError);
	assert.equal(thrown.code, "bench_missing");
	assert.match(thrown.message, /make build-cli/);
	assert.match(thrown.message, /BENCH_BIN/);
});

test("ensureBench 拒绝过旧的二进制", async () => {
	await assert.rejects(
		() => ensureBench(async () => ({ stdout: "", stderr: "", code: 5 }), "bench"),
		(e: BenchError) => e.code === "bench_outdated",
	);
});

test("resolveBinary 优先使用 BENCH_BIN", () => {
	assert.deepEqual(resolveBinary({ BENCH_BIN: "/opt/bench" }), { bin: "/opt/bench", source: "BENCH_BIN" });
	assert.deepEqual(resolveBinary({ BENCH_BIN: "  " }), { bin: "bench", source: "PATH" });
});

// ---------- 截断 ----------

test("truncate 在限额内原样返回", () => {
	const text = "a\nb\nc";
	const r = truncate(text, 10, 1000);
	assert.equal(r.text, text);
	assert.equal(r.truncated, false);
	assert.equal(r.omittedLines, 0);
});

test("truncate 按行数截断并报告省略量", () => {
	const text = Array.from({ length: 500 }, (_, i) => `line ${i}`).join("\n");
	const r = truncate(text, 120, 12_000);
	assert.equal(r.truncated, true);
	assert.equal(r.totalLines, 500);
	assert.equal(r.text.split("\n").length, 120);
	assert.equal(r.omittedLines, 380);
});

test("truncate 同时受字节限额约束", () => {
	const text = "x".repeat(50_000);
	const r = truncate(text, 10_000, 1_000);
	assert.equal(r.truncated, true);
	assert.ok(r.text.length <= 1_000, `字节限额未被遵守：${r.text.length}`);
});

test("truncate 正确处理多字节字符", () => {
	const text = "提示词".repeat(1000); // 每字符 3 字节
	const r = truncate(text, 10_000, 300);
	assert.equal(r.truncated, true);
	assert.ok(r.text.length < text.length);
	assert.ok(!r.text.includes("\uFFFD"), "不应截出半个字符");
});

test("truncate 在单行超长时仍返回内容而不是空串", () => {
	// benchmark 提示词经常整段不带换行；按行边界裁剪会把内容全部丢掉。
	const oneLine = "x".repeat(40_000);
	const r = truncate(oneLine, 120, 1_000);
	assert.equal(r.truncated, true);
	assert.ok(r.text.length > 0, "单行长文不得被截成空串");
	assert.ok(r.text.length <= 1_000);
});

test("truncate 对空输入不产生意外", () => {
	const r = truncate("", 120, 100);
	assert.equal(r.text, "");
	assert.equal(r.truncated, false);
});

// ---------- 状态格式化 ----------

test("formatStatus 输出单行摘要", () => {
	assert.equal(
		formatStatus({ server_total: 42, local_count: 40, up_to_date: false, server_hash: "h" }),
		"服务端 42 条 / 本地 40 条 / 落后，需 sync",
	);
	assert.equal(formatStatus({ server_total: 1, local_count: 1, up_to_date: true }), "服务端 1 条 / 本地 1 条 / 已同步");
	assert.equal(formatStatus(null), "状态不可用");
});

// ---------- 与真实 bench 二进制的集成测试 ----------

function realBin(): string | undefined {
	const candidates = [
		process.env.BENCH_BIN,
		join(here, "..", "..", "dist", "bench.exe"),
		join(here, "..", "..", "dist", "bench"),
	].filter((p): p is string => Boolean(p));
	return candidates.find((p) => existsSync(p));
}

// 临时 home 必须落在系统临时目录，不能污染仓库
function tmpHome(label: string): string {
	return mkdtempSync(join(tmpdir(), `bench-itest-${label}-`));
}

const benchPath = realBin();

type Skipper = { skip(message?: string): void };

/**
 * 缺二进制时把测试标记为跳过，而不是用 `bench!` 非空断言骗过类型检查。
 * 断言会把"没测到"伪装成"测过了"，这正是集成测试最不该有的失败模式。
 */
function needBench(t: Skipper): string | undefined {
	if (benchPath) return benchPath;
	t.skip("未找到 bench 二进制；先 make build-cli，或设 BENCH_BIN 指向它");
	return undefined;
}

// ---------- skill 结构校验（确定性，不依赖模型输出） ----------
//
// pi 对不合规范的 skill 只发 warning，坏 skill 会**静默不加载**；
// 而“问模型有哪些 skill”是非确定性的，不能当回归断言。所以自己校。
test("SKILL.md 的 frontmatter 符合 Agent Skills 规范", () => {
	const p = join(here, "..", "skill", "benchmark-testing", "SKILL.md");
	assert.ok(existsSync(p), `skill 文件必须存在：${p}`);

	const raw = readFileSync(p, "utf8");
	const m = /^---\n([\s\S]*?)\n---\n/.exec(raw);
	assert.ok(m, "SKILL.md 必须以 --- 开头并闭合 frontmatter");

	const fields = new Map<string, string>();
	for (const line of m[1].split("\n")) {
		const i = line.indexOf(":");
		if (i > 0) fields.set(line.slice(0, i).trim(), line.slice(i + 1).trim());
	}

	const name = fields.get("name") ?? "";
	assert.match(name, /^[a-z0-9][a-z0-9-]*[a-z0-9]$/, `name 需为小写字母/数字/连字符且首尾不是连字符：${name}`);
	assert.ok(!name.includes("--"), "name 不得含连续连字符");
	assert.ok(name.length <= 64, `name 超过 64 字符：${name.length}`);

	const desc = fields.get("description") ?? "";
	assert.ok(desc !== "", "缺 description 的 skill 不会被加载");
	assert.ok(desc.length <= 1024, `description 超过 1024 字符：${desc.length}`);
	assert.match(desc, /使用|时使用|Use/, "description 必须说明何时使用（它决定模型会不会加载这个 skill）");

	assert.ok(raw.slice(m[0].length).trim().length > 0, "frontmatter 之后必须有正文指令");
});

test("扩展目录结构与 pi 的发现规则一致", () => {
	assert.ok(existsSync(join(here, "index.ts")), "扩展入口必须是 index.ts（pi 按 */index.ts 发现）");
	assert.ok(existsSync(join(here, "bench-core.ts")), "纯逻辑核必须与入口同目录（jiti 相对导入）");
});

test("真实 bench：--version 输出可被 decodeBench 解析", async (t) => {
	const bin = needBench(t);
	if (!bin) return;
	const home = tmpHome("ver");
	const { stdout } = await run(bin, ["version", "--json", "--home", home], { encoding: "utf8" });
	const data = decodeBench({ stdout, stderr: "", code: 0 }, bin, ["version"]) as { version?: string };
	assert.equal(typeof data.version, "string");
});

test("真实 bench：错误确实以 JSON 信封走 stdout（扩展的解析前提）", async (t) => {
	const bin = needBench(t);
	if (!bin) return;
	// 用一个必定不可达的端口 + 临时 home，触发网络类失败
	const home = tmpHome("err");
	let res: ExecResult;
	try {
		const { stdout, stderr } = await run(bin, ["meta", "--json", "--endpoint", "http://127.0.0.1:1", "--home", home], {
			encoding: "utf8",
		});
		res = { stdout, stderr, code: 0 };
	} catch (e: unknown) {
		const err = e as { code?: number; stdout?: string; stderr?: string };
		res = { stdout: err.stdout ?? "", stderr: err.stderr ?? "", code: err.code ?? 1 };
	}

	// 关键断言：stdout 必须是可解析的信封，而不是只有 stderr
	assert.ok(res.stdout.trim() !== "", "错误响应必须走 stdout，否则扩展无法拿到 error.code");
	const parsed = JSON.parse(res.stdout) as { ok: boolean; error: { code: string } };
	assert.equal(parsed.ok, false);
	assert.equal(typeof parsed.error.code, "string");
	assert.notEqual(parsed.error.code, "");

	// 且被映射成可重试的 BenchError
	assert.throws(
		() => decodeBench({ ...res, code: res.code === 0 ? 1 : res.code }, bin, ["meta"]),
		(e: BenchError) => e.retryable === true,
	);
});

test("真实 bench：错误信封形状统一（ok/data/error/v）", async (t) => {
	const bin = needBench(t);
	if (!bin) return;
	const home = tmpHome("score");
	// 先配好一个不可达的 endpoint，使命令能过本地配置检查、去碰真实网络/校验路径
	await run(bin, ["config", "init", "--endpoint", "http://127.0.0.1:1", "--home", home], { encoding: "utf8" });

	let code = 0;
	let stdout = "";
	try {
		const r = await run(bin, ["score", "--json", "p_x", "9", "--home", home], { encoding: "utf8" });
		stdout = r.stdout;
	} catch (e: unknown) {
		const err = e as { code?: number; stdout?: string };
		code = err.code ?? 1;
		stdout = err.stdout ?? "";
	}
	assert.equal(code, 5, `应返回退出码 5，得到 ${code}：${stdout}`);

	const parsed = JSON.parse(stdout) as { ok: boolean; data: unknown; error: { code: string }; v: number };
	assert.equal(parsed.ok, false);
	assert.equal(parsed.data, null);
	assert.equal(parsed.v, 1);
	assert.equal(parsed.error.code, "validation_failed", "分值越界应在本地就被拦下，不消耗网络往返");

	// 统一形状：成功路径也必须带 ok/data
	const { stdout: okOut } = await run(bin, ["version", "--json", "--home", home], { encoding: "utf8" });
	const okParsed = JSON.parse(okOut) as { ok: boolean; data: { version: string } };
	assert.equal(okParsed.ok, true);
	assert.equal(typeof okParsed.data.version, "string");
});
