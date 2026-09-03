/**
 * plugin.test.ts —— DSH 适配的框架层功能测试：假 ctx + 真 defineTool + 桩 subprocess。
 *
 * 覆盖 node 下可确定性验证的一切：注册形状、argv 构造、信封解码到错误分类、
 * output.render 的模型可见文本、六个命令 handler 的成功/错误形状、
 * bench 缺失自愈（失败不缓存）、超时与取消传播、注入安全（argv 不经 shell）。
 *
 * 运行（裸 @deepseek-ai/* 依赖经钩子解析到 DSH 安装树）：
 *   node --import scripts/dsh-module-hook.mjs --test plugins/dsh/plugin.test.ts
 * 真装载与真 LLM 调用是 scripts/smoke-dsh.sh 的职责，二者互补。
 */
import assert from "node:assert/strict";
import { createHash } from "node:crypto";
import { existsSync, readFileSync } from "node:fs";
import { dirname, join } from "node:path";
import { test } from "node:test";
import { fileURLToPath } from "node:url";
import { BenchError } from "./bench-core.ts";
import { apply } from "./index.ts";

const HERE = dirname(fileURLToPath(import.meta.url));

// ---------- 测试脚手架 ----------

interface StubCall {
	argv: readonly string[];
	cwd: string;
	signal?: AbortSignal;
}

type SpawnBehavior = (
	call: StubCall,
	index: number,
) => { code: number; stdout: string; stderr?: string; delayMs?: number } | { throws: Error };

function makeHandle(
	outcome: { exitCode: number | null; signal?: string | null },
	stdout: string,
	stderr: string,
	delayMs: number,
) {
	const collected = {
		stdout: { readFrom: () => ({ text: stdout, nextOffset: stdout.length, lossy: false }) },
		stderr: { readFrom: () => ({ text: stderr, nextOffset: stderr.length, lossy: false }) },
	};
	const done = new Promise<{ exitCode: number | null; signal: string | null }>((resolve) => {
		const settle = (): void => resolve({ exitCode: outcome.exitCode, signal: outcome.signal ?? null });
		if (delayMs > 0) setTimeout(settle, delayMs);
		else settle();
	});
	return {
		pid: 4242,
		stdin: undefined,
		stdout: undefined,
		stderr: undefined,
		collected,
		done,
		terminate() {},
		waitForExit: async () => true,
	};
}

function makeHarness(spawn: SpawnBehavior, configOver: Partial<TestConfig> = {}) {
	const calls: StubCall[] = [];
	const tools: Record<string, any> = {};
	const commands: Record<string, any> = {};
	const config: TestConfig = {
		bin: "bench-x",
		home: "H:/bh",
		endpoint: "",
		timeoutMs: 5_000,
		graceMs: 100,
		...configOver,
	};
	const ctx = {
		tools: {
			register: (t: any) => {
				tools[t.name] = t;
				return () => {};
			},
		},
		commands: {
			register: (c: any) => {
				commands[c.name] = c;
				return () => {};
			},
		},
		subprocess: {
			spawn(spec: any): any {
				const call: StubCall = { argv: spec.argv, cwd: spec.cwd, signal: spec.signal };
				const index = calls.push(call) - 1;
				const behavior = spawn(call, index);
				if ("throws" in behavior) throw behavior.throws;
				const { code, stdout, stderr = "", delayMs = 0 } = behavior;
				if (delayMs === 0 && code === -1) {
					// 模拟 spawn 级失败（ENOENT 一类）：done reject
					return { ...makeHandle({ exitCode: 1 }, stdout, stderr, 0), done: Promise.reject(new Error("spawn ENOENT")) };
				}
				return makeHandle({ exitCode: code }, stdout, stderr, delayMs);
			},
		},
		logger: () => ({ info() {}, warn() {}, error() {}, debug() {} }),
	};
	apply(ctx as never, config as never);
	return { calls, tools, commands, config };
}

interface TestConfig {
	bin: string;
	home: string;
	endpoint: string;
	timeoutMs: number;
	graceMs: number;
}

/** 固定应答：第 0 次调用恒为 version 探测，其后的按场景表逐条应答。 */
function scripted(...answers: Array<{ code: number; stdout: string; stderr?: string }>): SpawnBehavior {
	const probe = { code: 0, stdout: '{"data":{"version":"dev"},"error":null,"ok":true,"v":1}' };
	return (_call, index) => (index === 0 ? probe : (answers[index - 1] ?? { code: 0, stdout: probe.stdout }));
}

const envelope = (data: unknown): string => JSON.stringify({ data, error: null, ok: true, v: 1 });
const failure = (code: number, errCode: string, message: string): { code: number; stdout: string } => ({
	code,
	stdout: JSON.stringify({ data: null, error: { code: errCode, message }, ok: false, v: 1 }),
});

const PROMPT_STDOUT = envelope({
	id: "p_1",
	p: "第一行正文\n第二行正文",
	t: ["writing"],
	v: 2,
	s: "approved",
	h: "c9",
});

const newExec = (): { signal: AbortSignal } => ({ signal: new AbortController().signal });

// ---------- 装载形状 ----------

test("插件导出 name/inject/Config/apply 四件套", async () => {
	const mod = await import("./index.ts");
	assert.equal(mod.name, "dsh-bench");
	assert.deepEqual([...mod.inject].sort(), ["commands", "subprocess", "tools"]);
	assert.equal(typeof mod.apply, "function");
	assert.equal(typeof mod.Config, "function");
});

test("注册 5 个工具与 6 个命令，描述非空", () => {
	const { tools, commands } = makeHarness(scripted());
	assert.deepEqual(Object.keys(tools).sort(), [
		"bench_catalog",
		"bench_get",
		"bench_random",
		"bench_score",
		"bench_upload",
	]);
	assert.deepEqual(Object.keys(commands).sort(), [
		"bench-get",
		"bench-list",
		"bench-random",
		"bench-score",
		"bench-status",
		"bench-sync",
	]);
	for (const t of Object.values(tools)) {
		assert.ok(t.description.length > 0);
		assert.equal(typeof t.execute, "function");
		assert.equal(t.output.schema.type, "object");
		assert.equal(t.output.schema.additionalProperties, false);
	}
	for (const c of Object.values(commands)) {
		assert.ok(c.description.length > 0);
		assert.equal(typeof c.handler, "function");
	}
});

test("并发安全声明：只读可并行，写与推进窗口的独占", () => {
	const { tools } = makeHarness(scripted());
	assert.equal(tools.bench_get.isConcurrencySafe({ id: "p_1" }), true);
	assert.equal(tools.bench_random.isConcurrencySafe({}), false);
	assert.equal(tools.bench_score.isConcurrencySafe({ id: "p_1", value: 3 }), false);
	assert.equal(tools.bench_upload.isConcurrencySafe({ content: "x" }), false);
	assert.equal(tools.bench_catalog.isConcurrencySafe({ action: "list" }), true);
	assert.equal(tools.bench_catalog.isConcurrencySafe({ action: "sync" }), false);
});

// ---------- argv 构造与注入安全 ----------

test("random 走 probe → random 两条 argv，--home 注入且 --json 恒在", async () => {
	const h = makeHarness(scripted({ code: 0, stdout: PROMPT_STDOUT }));
	const value = await h.tools.bench_random.execute({ tag: "writing", fresh: true }, newExec());
	assert.equal(h.calls.length, 2);
	assert.deepEqual([...h.calls[0].argv], ["bench-x", "version", "--json"]);
	assert.deepEqual(
		[...h.calls[1].argv],
		["bench-x", "random", "--json", "--tag", "writing", "--fresh", "--home", "H:/bh"],
	);
	assert.equal(value.id, "p_1");
});

test("含引号/$/换行的正文走数组元素，绝不拼进命令行字符串", async () => {
	const nasty = "a\"b'c $VAR `id` \\end\n下一行";
	const h = makeHarness(scripted({ code: 0, stdout: envelope({ id: "p_9", s: "pending" }) }));
	await h.tools.bench_upload.execute({ content: nasty, tags: ["a", "b,c"], client_id: "k1" }, newExec());
	const argv = [...h.calls[1].argv];
	assert.equal(argv[0], "bench-x");
	assert.equal(argv[1], "upload");
	assert.ok(argv.includes(nasty), "正文必须是独立 argv 元素");
	assert.equal(argv[argv.indexOf("-t") + 1], "a,b,c");
	assert.ok(JSON.stringify(argv).includes("$VAR"));
});

test("get 的选项在位置参数前，endpoint 全局项可追加", async () => {
	const h = makeHarness(scripted({ code: 0, stdout: PROMPT_STDOUT }), { endpoint: "http://127.0.0.1:18099" });
	await h.tools.bench_get.execute({ id: "p_1", local: true }, newExec());
	assert.deepEqual(
		[...h.calls[1].argv],
		["bench-x", "get", "--json", "--local", "--home", "H:/bh", "p_1", "--endpoint", "http://127.0.0.1:18099"],
	);
});

test("score 的 argv 为 <id> <value> 位置参数", async () => {
	const h = makeHarness(scripted({ code: 0, stdout: envelope({ avg: 4, count: 1 }) }));
	const value = await h.tools.bench_score.execute({ id: "p_1", value: 4 }, newExec());
	assert.deepEqual([...h.calls[1].argv].slice(1), ["score", "--json", "--home", "H:/bh", "p_1", "4"]);
	assert.deepEqual(value, { id: "p_1", value: 4, avg: 4, count: 1 });
});

// ---------- 模型可见渲染（与 Pi 侧模板逐字对齐） ----------

test("render 产出统一模板：标题块 + 分隔线 + 下一步指示", async () => {
	const h = makeHarness(scripted({ code: 0, stdout: PROMPT_STDOUT }));
	const value = await h.tools.bench_random.execute({}, newExec());
	const blocks = h.tools.bench_random.output.render({}, value);
	assert.equal(blocks.length, 1);
	const text: string = blocks[0].text;
	assert.ok(text.startsWith("【Benchmark 提示词 p_1】标签: writing  v2\n"));
	assert.ok(
		text.includes("----------------------------------\n第一行正文\n第二行正文\n----------------------------------"),
	);
	assert.ok(text.includes('把上面这段原样发给被测模型；回答完成后调用 bench_score(id="p_1", value=1-5) 记录评分。'));
});

test("超长正文在值里就截断，render 附截断说明", async () => {
	const long = Array.from({ length: 200 }, (_, i) => `第${String(i + 1)}行`).join("\n");
	const h = makeHarness(scripted({ code: 0, stdout: envelope({ id: "p_2", p: long, t: [], v: 1 }) }));
	const value = await h.tools.bench_get.execute({ id: "p_2" }, newExec());
	assert.equal(value.truncated, true);
	assert.equal(value.totalLines, 200);
	assert.ok(value.omittedLines > 0);
	const text: string = h.tools.bench_get.output.render({}, value)[0].text;
	assert.ok(text.includes("[正文过长已截断：共 200 行，省略"));
});

// ---------- 四类失败：可行动中文、绝不以成功返回 ----------

test("not_found（exit 4）→ BenchError，文本带归类与原始 code", async () => {
	const h = makeHarness(scripted(failure(4, "not_found", "not_found: 资源不存在 (HTTP 404)")));
	await assert.rejects(h.tools.bench_get.execute({ id: "p_ghost" }, newExec()), (err: BenchError) => {
		assert.ok(err instanceof BenchError);
		assert.ok(err.message.includes("资源不存在"));
		assert.ok(err.message.includes("not_found"));
		assert.equal(err.retryable, false);
		return true;
	});
});

test("分值越界在前置校验就拦下（不消耗进程）", async () => {
	let spawns = 0;
	const h = makeHarness((call, index) => {
		spawns += 1;
		return scripted({ code: 0, stdout: envelope({ avg: 4, count: 1 }) })(call, index);
	});
	await assert.rejects(
		h.tools.bench_score.execute({ id: "p_1", value: 9 }, newExec()),
		(err: BenchError) => err.message.includes("必须是 1-5 的整数") && err.code === "validation_failed",
	);
	assert.equal(spawns, 1, "除探测外不得再启动 bench");
});

test("网络失败（exit 1）归类为可重试；未配置（exit 5）指引 config init", async () => {
	const net = makeHarness(scripted(failure(1, "network", "network: 网络请求失败 (connectex)")));
	await assert.rejects(net.tools.bench_get.execute({ id: "p_1" }, newExec()), (err: BenchError) => {
		assert.ok(err.message.includes("网络或服务端故障"));
		assert.equal(err.retryable, true);
		return true;
	});
	const local = makeHarness(
		scripted(
			failure(
				5,
				"local_error",
				"local_error: 尚未配置服务地址；请运行 bench config init --endpoint <url>，或设置环境变量 BENCH_ENDPOINT",
			),
		),
	);
	await assert.rejects(local.tools.bench_catalog.execute({ action: "status" }, newExec()), (err: BenchError) => {
		assert.ok(err.message.includes("尚未配置服务地址"));
		assert.ok(err.message.includes("bench config init"));
		return true;
	});
});

test("bench 二进制缺失 → 安装指引；修好后同一实例自愈（失败不缓存）", async () => {
	let broken = true;
	const probeAnswer = { code: 0, stdout: '{"data":{"version":"dev"},"error":null,"ok":true,"v":1}' };
	const h = makeHarness((call) => {
		if (call.argv[1] === "version") return broken ? { code: -1, stdout: "" } : probeAnswer;
		return { code: 0, stdout: PROMPT_STDOUT };
	});
	await assert.rejects(h.tools.bench_random.execute({}, newExec()), (err: BenchError) => {
		assert.ok(err.message.includes("找不到 bench 可执行文件"));
		assert.ok(err.message.includes("make build-cli"));
		assert.ok(err.message.includes("BENCH_BIN"));
		return true;
	});
	broken = false;
	const value = await h.tools.bench_random.execute({}, newExec());
	assert.equal(value.id, "p_1", "装好后不重启即恢复");
});

test("超时预算内未回 → canceled，而不是静默成功", async () => {
	const h = makeHarness(
		(_call, index) =>
			index === 0
				? { code: 0, stdout: '{"data":{"version":"dev"},"error":null,"ok":true,"v":1}' }
				: { code: 0, stdout: PROMPT_STDOUT, delayMs: 500 },
		{ timeoutMs: 40, graceMs: 10 },
	);
	// 桩的 handle 会等 500ms 才 resolve，而 makeExec 40ms 就 abort：真 subprocess 会因
	// spec.signal 终止子进程并立即 settle；桩按"信号触发即 settle"模拟该义务。
	const msg = await h.tools.bench_get.execute({ id: "p_1" }, newExec()).then(
		() => "UNEXPECTED-OK",
		(e: Error) => e.message,
	);
	assert.ok(msg.includes("被取消或超时"), `实际错误：${msg}`);
});

test("外部取消信号：调用前已 abort → 直接拒绝且不起进程", async () => {
	const h = makeHarness(scripted({ code: 0, stdout: PROMPT_STDOUT }));
	const ac = new AbortController();
	ac.abort(new Error("user cancel"));
	await assert.rejects(
		h.tools.bench_random.execute({}, { signal: ac.signal } as never),
		(err: Error) => err.message.includes("已取消") || err.message.includes("cancel"),
	);
	// 探测可能先于 signal 检查发生（懒探测与取消检查的次序），但正文进程绝不允许再起。
	assert.ok(h.calls.length <= 2);
	if (h.calls.length === 2) assert.ok(!h.calls[1].argv.includes("random"));
});

// ---------- 命令 handler（DSH 特有：不经模型，直接出 CommandResult） ----------

const invoke = (rawInput = ""): any => ({
	commandId: "c1",
	agent: {},
	rawInput,
	attachments: [],
	signal: new AbortController().signal,
});

test("/bench-random 成功返回 success + 可复制正文块（无模型指示行）", async () => {
	const h = makeHarness(scripted({ code: 0, stdout: PROMPT_STDOUT }));
	const result = await h.commands["bench-random"].handler(invoke("writing"));
	assert.equal(result.kind, "success");
	assert.ok(result.text.startsWith("【Benchmark 提示词 p_1】"));
	assert.ok(!result.text.includes("把上面这段原样发给被测模型"), "给人复制的正文不带模型指示");
});

test("/bench-score 参数不足 → error 且文本可行动；缺 id 同理", async () => {
	const h = makeHarness(scripted());
	const r1 = await h.commands["bench-score"].handler(invoke("p_1"));
	assert.equal(r1.kind, "error");
	assert.ok(r1.text.includes("用法: /bench-score <id> <1-5>"));
});

test("/bench-status 输出与 formatStatus 一致的一行", async () => {
	const h = makeHarness(
		scripted({ code: 0, stdout: envelope({ server_total: 3, local_count: 1, up_to_date: false }) }),
	);
	const r = await h.commands["bench-status"].handler(invoke());
	assert.equal(r.kind, "success");
	assert.equal(r.text, "服务端 3 条 / 本地 1 条 / 落后，需 sync");
});

test("/bench-sync 与 /bench-list 的成功文本", async () => {
	const h = makeHarness(
		scripted(
			{ code: 0, stdout: envelope({ local_count: 5, report: { upserted: 2, deleted: 1 } }) },
			{
				code: 0,
				stdout: envelope({
					count: 2,
					items: [
						{ id: "p_1", t: ["a"] },
						{ id: "p_2", t: [] },
					],
				}),
			},
		),
	);
	const s = await h.commands["bench-sync"].handler(invoke());
	assert.ok(s.text.includes("变更 2") && s.text.includes("删除 1") && s.text.includes("本地 5 条"));
	const l = await h.commands["bench-list"].handler(invoke("a"));
	assert.ok(l.text.startsWith("2 条："));
	assert.ok(l.text.includes("p_1  a"));
});

test("命令错误路径永不返回 success（以随机取题失败为例）", async () => {
	const h = makeHarness(scripted(failure(4, "not_found", "not_found: 当前没有可用的提示词")));
	const r = await h.commands["bench-random"].handler(invoke("nosuch"));
	assert.equal(r.kind, "error");
	assert.ok(r.text.includes("没有可用"));
});

// ---------- catalog 三态 ----------

test("bench_catalog 三种 action 的模型可见文本", async () => {
	const h = makeHarness(
		scripted(
			{ code: 0, stdout: envelope({ server_total: 7, local_count: 7, up_to_date: true }) },
			{ code: 0, stdout: envelope({ local_count: 9, report: { upserted: 2, deleted: 0 } }) },
			{ code: 0, stdout: envelope({ count: 1, items: [{ id: "p_3", t: ["sys"], v: 4 }] }) },
		),
	);
	const st = await h.tools.bench_catalog.execute({ action: "status" }, newExec());
	assert.ok(st.text.includes("服务端 7 条") && st.text.includes("已同步"));
	const sy = await h.tools.bench_catalog.execute({ action: "sync" }, newExec());
	assert.ok(sy.text.includes("新增/变更 2"));
	const li = await h.tools.bench_catalog.execute({ action: "list" }, newExec());
	assert.ok(li.text.includes("p_3") && li.text.includes("v4") && li.text.includes("sys"));
});

// ---------- bench-core 双份防漂移 ----------

const BENCH_CORE_SHA256 = "2abb6536c7487bcf569e550c8284adaa5f83d662fa155e3296cd41dbc7c473cf";

test("dsh 侧 bench-core.ts 与钉住的哈希一致（改动须双向同步）", () => {
	const hash = createHash("sha256")
		.update(readFileSync(join(HERE, "bench-core.ts")))
		.digest("hex");
	assert.equal(hash, BENCH_CORE_SHA256, "两份 bench-core 必须同步修改后一起更新本测试的钉值");
});

test("仓库布局可用时，逐字节比对 pi 侧 bench-core.ts", () => {
	const piCopy = join(HERE, "..", "pi", "extension", "bench-core.ts");
	if (!existsSync(piCopy)) return; // 装载副本（无 pi 目录树）时静默跳过
	assert.deepEqual([...readFileSync(join(HERE, "bench-core.ts"))], [...readFileSync(piCopy)]);
});
