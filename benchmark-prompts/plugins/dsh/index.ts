/**
 * bench —— 个人测试用 benchmark 提示词的 DSH（DeepSeek Harness）适配插件。
 *
 * 形态与 Pi 侧同构：自定义工具（LLM 可调用）+ 斜杠命令（用户直接触发），
 * 全部通过 ctx.subprocess 调用 `bench` CLI，**本文件不含任何业务逻辑**
 * （网络/缓存/同步都在 Go 侧，见 docs/architecture.md ADR-6）。
 * 易错纯逻辑在 bench-core.ts（与 plugins/pi/extension/bench-core.ts 同一份代码）。
 *
 * 与 Pi 方言的三个关键差异（交接文档 §4）：
 *  1. 参数 schema 是 schemastery 声明式对象（required: true 写在属性里），不是 TypeBox；
 *  2. `execute` 只返回符合 output.schema 的 JSON 值，模型所见文本由 output.render 投影；
 *  3. 取消信号在第二入参 `exec.signal` 上，必须转发给子进程。
 *
 * 装载（详见同目录 README.md）：`dsh plugin --profile <name> add file:<此目录>`，
 * 或把本目录复制进 profile 树后在 cordis.patch.yml / --patch 里按路径引用。
 * 前置：`make build-cli` 产出 bench 可执行文件，`bench config init --endpoint <url>`
 * 完成配置（或在本插件 config 里显式给 endpoint/bin）。
 */

import type { Context } from "@deepseek-ai/cordis";
// 类型侧载：ctx.commands / ctx.subprocess 的服务声明来自这两个包的 declare module。
import type {} from "@deepseek-ai/dsh-commands";
import type {} from "@deepseek-ai/dsh-subprocess";
import { defineTool } from "@deepseek-ai/dsh-tools";
import z from "@deepseek-ai/schemastery";

import {
	BenchError,
	buildArgs,
	decodeBench,
	type Exec,
	type ExecResult,
	ensureBench,
	formatStatus,
	resolveBinary,
	truncate,
} from "./bench-core.ts";

export const name = "dsh-bench";
// dsh-base 同时提供这三个服务（web 与 headless 皆然）；缺任一服务本插件不激活。
export const inject = ["tools", "commands", "subprocess"];

/** 用户可配项。全部留空即"完全交给 bench 自己的配置"。 */
export interface Config {
	/** bench 可执行文件绝对路径；空 = BENCH_BIN 环境变量 > PATH 上的 bench。 */
	bin: string;
	/** 传给 bench 的 --home（配置与缓存目录）；空 = bench 默认。 */
	home: string;
	/** 传给 bench 的 --endpoint；空 = bench 用自己的配置文件。 */
	endpoint: string;
	/** 单次 bench 调用的子进程超时预算（毫秒）。 */
	timeoutMs: number;
	/** terminate 升级与管道排空的宽限期（毫秒）。 */
	graceMs: number;
}

export const Config: z<Config> = z.object({
	bin: z.string().default(""),
	home: z.string().default(""),
	endpoint: z.string().default(""),
	timeoutMs: z.number().default(60_000),
	graceMs: z.number().default(3_000),
});

/** bench 返回的提示词线格式（docs/api.md §2.1 的短字段名）。 */
interface BenchPrompt {
	id: string;
	p: string;
	t?: string[];
	v?: number;
	s?: string;
	h?: string;
}

/** bench_random / bench_get 的规范化输出（output.schema 同形）。 */
interface PromptValue {
	id: string;
	body: string;
	tags: string[];
	version: number;
	truncated: boolean;
	totalLines: number;
	omittedLines: number;
}

/**
 * 子进程桥：把 ctx.subprocess 包成 bench-core 期望的 Exec 签名。
 * argv 不经 shell 解释，正文里的引号/换行/$ 都是安全的（与 Pi 的差异点）。
 */
function makeExec(ctx: Context, config: Config): Exec {
	return async (bin, args, opts = {}): Promise<ExecResult> => {
		if (opts.signal?.aborted) throw new Error("bench 调用已取消");
		const controller = new AbortController();
		let timedOut = false;
		const timer = setTimeout(() => {
			timedOut = true;
			controller.abort(new Error(`bench 调用超过 ${String(opts.timeout ?? config.timeoutMs)}ms`));
		}, opts.timeout ?? config.timeoutMs);
		const onAbort = (): void => controller.abort(opts.signal?.reason);
		opts.signal?.addEventListener("abort", onAbort, { once: true });
		try {
			const handle = ctx.subprocess.spawn({
				argv: [bin, ...args],
				cwd: process.cwd(),
				stdio: {
					stdin: "ignore",
					stdout: { maxBytes: 2_000_000 },
					stderr: { maxBytes: 400_000 },
				},
				graceMs: config.graceMs,
				signal: controller.signal,
				// 刻意不塞 spec.env：显式 env 会"覆盖净化"泄漏宿主凭证。
				// 配置一律走 --home/--endpoint 命令行参数（交接文档 §7 的结论）。
			});
			const outcome = await handle.done;
			const stdout = handle.collected.stdout?.readFrom(0).text ?? "";
			const stderr = handle.collected.stderr?.readFrom(0).text ?? "";
			return {
				stdout,
				stderr,
				// 被信号杀死（Windows 强杀也走这里）归 1 号"网络/故障"类，配合 killed 判 canceled。
				code: outcome.exitCode ?? 1,
				killed: timedOut || opts.signal?.aborted === true,
			};
		} finally {
			clearTimeout(timer);
			opts.signal?.removeEventListener("abort", onAbort);
		}
	};
}

// ---------- 纯渲染辅助（与 Pi 侧逐字对齐，保证两框架给用户一致排版） ----------

/** 提示词块：人复制与 LLM 转交用的是同一份文本（docs/plugins.md §5）。 */
function renderPromptBlock(value: PromptValue): string {
	const header = `【Benchmark 提示词 ${value.id}】标签: ${value.tags.join(", ") || "-"}  v${String(value.version)}`;
	return `${header}\n----------------------------------\n${value.body}\n----------------------------------`;
}

function promptValue(data: BenchPrompt): PromptValue {
	const body = truncate(data.p ?? "", 120, 12_000);
	return {
		id: data.id,
		body: body.text,
		tags: data.t ?? [],
		version: data.v ?? 1,
		truncated: body.truncated,
		totalLines: body.totalLines,
		omittedLines: body.omittedLines,
	};
}

/** 模型可见文本 = 正文块 + 截断说明 + 下一步指示（Pi 的 promptResult 同文案）。 */
function renderPromptResult(_args: unknown, v: PromptValue): { type: "text"; text: string }[] {
	let text = renderPromptBlock(v);
	if (v.truncated) text += `\n[正文过长已截断：共 ${String(v.totalLines)} 行，省略 ${String(v.omittedLines)} 行]`;
	text += `\n把上面这段原样发给被测模型；回答完成后调用 bench_score(id="${v.id}", value=1-5) 记录评分。`;
	return [{ type: "text", text }];
}

function toMessage(err: unknown): string {
	return err instanceof Error ? err.message : String(err);
}

const PROMPT_OUTPUT = {
	type: "object",
	additionalProperties: false,
	properties: {
		id: { type: "string", required: true, description: "提示词 id" },
		body: { type: "string", required: true, description: "提示词正文（超长已截断）" },
		tags: { type: "array", required: true, items: { type: "string" } },
		version: { type: "integer", required: true },
		truncated: { type: "boolean", required: true },
		totalLines: { type: "integer", required: true },
		omittedLines: { type: "integer", required: true },
	},
} as const;

export function apply(ctx: Context, config: Config): void {
	const log = ctx.logger("bench");
	const { bin: pathBin } = resolveBinary();
	const bin = config.bin.trim() === "" ? pathBin : config.bin.trim();
	const exec = makeExec(ctx, config);

	// 懒探测、失败不缓存：用户装好 bench 后无需重启 DSH 即自愈（Pi 侧同一手法）。
	let readiness: Promise<void> | null = null;
	const ensureReady = (): Promise<void> => {
		if (readiness === null) {
			readiness = ensureBench(exec, bin).catch((err: unknown) => {
				readiness = null;
				throw err;
			});
		}
		return readiness;
	};

	/** 跑一条 bench 命令并返回解包后的 data；失败抛 BenchError（DSH 转成工具错误）。 */
	const run = async (command: string, opts: Record<string, unknown> = {}, signal?: AbortSignal): Promise<unknown> => {
		await ensureReady();
		// home 经由 buildArgs 统一注入（它从 opts 里读 home），endpoint 是全局覆盖项。
		const args = buildArgs(command, { ...opts, home: config.home } as never);
		if (config.endpoint.trim() !== "") args.push("--endpoint", config.endpoint.trim());
		const res: ExecResult = await exec(bin, args, { signal });
		return decodeBench(res, bin, args);
	};

	// ---------- 工具 ----------

	ctx.tools.register(
		defineTool({
			name: "bench_random",
			description:
				"从个人 benchmark 提示词库随机取一条用于本地 LLM 测试。可按标签过滤，" + "并可排除最近已抽过的条目以避免重复。",
			parameters: {
				tag: { type: "string", description: "按标签过滤，例如 coding、reasoning、systems" },
				fresh: { type: "boolean", description: "true=排除最近抽过的条目" },
			},
			output: {
				schema: PROMPT_OUTPUT,
				render: renderPromptResult,
				presentationMeta: (_a, v) => ({ id: v.id }),
			},
			timeoutMs: config.timeoutMs + 30_000,
			// random 会推进"最近抽过"滚动窗口，并发会互相吃掉排除集 → 独占（缺省即独占，显式写出）
			isConcurrencySafe: () => false,
			async execute(a, execCtx) {
				const data = (await run("random", { tag: a.tag, fresh: a.fresh }, execCtx.signal)) as BenchPrompt;
				return promptValue(data);
			},
			presentCall: (a) => ({ card: "generic", title: "Bench 随机取题", kind: "fetch", rawInput: a.tag ?? undefined }),
		}),
	);

	ctx.tools.register(
		defineTool({
			name: "bench_get",
			description:
				"按 id 取一条 benchmark 提示词（一键测试）。id 可来自 bench_random、bench_catalog list 或用户直接给出。" +
				"设 local=true 只读本地缓存、不访问网络。",
			parameters: {
				id: { type: "string", required: true, description: "提示词 id，形如 p_9f3a2c1b" },
				local: { type: "boolean", description: "true=只读本地缓存，不联网" },
			},
			output: {
				schema: PROMPT_OUTPUT,
				render: renderPromptResult,
				presentationMeta: (_a, v) => ({ id: v.id }),
			},
			timeoutMs: config.timeoutMs + 30_000,
			isConcurrencySafe: () => true,
			async execute(a, execCtx) {
				const data = (await run("get", { id: a.id, local: a.local }, execCtx.signal)) as BenchPrompt;
				return promptValue(data);
			},
			presentCall: (a) => ({ card: "generic", title: `Bench 取题 ${a.id}`, kind: "fetch", rawInput: a.id }),
		}),
	);

	ctx.tools.register(
		defineTool({
			name: "bench_score",
			description: "为一条 benchmark 提示词打 1-5 分。同一设备重复打分会覆盖旧分值，并返回当前均分与人数。",
			parameters: {
				id: { type: "string", required: true, description: "提示词 id" },
				value: { type: "integer", required: true, description: "1-5 的整数评分" },
			},
			output: {
				schema: {
					type: "object",
					additionalProperties: false,
					properties: {
						id: { type: "string", required: true },
						value: { type: "integer", required: true },
						avg: { type: "number", required: true },
						count: { type: "integer", required: true },
					},
				},
				render: (_a, v) => [
					{
						type: "text",
						text: `已记录 ${String(v.value)} 分；该提示词当前均分 ${String(v.avg)}（${String(v.count)} 人评分）。`,
					},
				],
				presentationMeta: (_a, v) => ({ id: v.id, avg: v.avg, count: v.count }),
			},
			timeoutMs: config.timeoutMs + 30_000,
			// 写请求不重试（docs/client.md ADR-12），并发写同一条会互相覆盖 → 独占
			isConcurrencySafe: () => false,
			async execute(a, execCtx) {
				const data = (await run("score", { id: a.id, value: a.value }, execCtx.signal)) as {
					avg: number;
					count: number;
				};
				return { id: a.id, value: a.value, avg: data.avg, count: data.count };
			},
			presentCall: (a) => ({ card: "generic", title: `Bench 打分 ${a.id} = ${String(a.value)}`, kind: "other" }),
		}),
	);

	ctx.tools.register(
		defineTool({
			name: "bench_catalog",
			description:
				"浏览与维护本地提示词库。action=list 按标签列目录（只给 id 与标签，不含正文）；" +
				"action=sync 增量同步服务端新增内容；action=status 查看本地与服务端是否一致。",
			parameters: {
				action: {
					type: "string",
					required: true,
					enum: ["list", "sync", "status"],
					description: "list | sync | status",
				},
				tag: { type: "string", description: "list 时按标签过滤" },
				limit: { type: "integer", description: "list 每页条数，默认 20，上限 100" },
			},
			output: {
				schema: {
					type: "object",
					additionalProperties: false,
					properties: {
						action: { type: "string", required: true },
						text: { type: "string", required: true },
						count: { type: "integer", required: true },
						truncated: { type: "boolean", required: true },
					},
				},
				render: (_a, v) => [{ type: "text", text: v.text }],
			},
			timeoutMs: config.timeoutMs + 30_000,
			// sync 会写本地缓存；list/status 纯读。
			isConcurrencySafe: (a) => a.action !== "sync",
			async execute(a, execCtx) {
				const action = String(a.action ?? "list");
				if (action === "status") {
					const data = await run("meta", {}, execCtx.signal);
					const s = data as { server_total?: number; local_count?: number };
					return { action, text: formatStatus(data), count: s.server_total ?? 0, truncated: false };
				}
				if (action === "sync") {
					const data = await run("sync", {}, execCtx.signal);
					const wrap = data as { report?: { upserted?: number; deleted?: number }; local_count?: number };
					const rep = wrap?.report ?? {};
					return {
						action,
						text:
							`同步完成：新增/变更 ${String(rep.upserted ?? 0)}，删除 ${String(rep.deleted ?? 0)}，` +
							`本地共 ${String(wrap?.local_count ?? 0)} 条。`,
						count: wrap?.local_count ?? 0,
						truncated: false,
					};
				}
				const data = (await run("list", { tag: a.tag, limit: a.limit }, execCtx.signal)) as {
					items?: Array<{ id: string; t?: string[]; v?: number }>;
					count?: number;
				};
				const lines = (data.items ?? []).map((it) => `${it.id}\tv${String(it.v ?? 1)}\t${(it.t ?? []).join(",")}`);
				const body = truncate(lines.join("\n"), 200, 8_000);
				let text = body.text;
				if (body.truncated) {
					text += `\n[仅显示部分：共 ${String(data.count ?? lines.length)} 条，省略 ${String(body.omittedLines)} 行；用 tag 过滤缩小范围]`;
				}
				if (text === "") text = "（没有匹配的提示词；先跑 bench_catalog action=sync 同步）";
				return { action, text, count: data.count ?? lines.length, truncated: body.truncated };
			},
			presentCall: (a) => ({ card: "generic", title: `Bench 目录 ${String(a.action ?? "?")}`, kind: "search" }),
		}),
	);

	ctx.tools.register(
		defineTool({
			name: "bench_upload",
			description:
				"上传一条新的 benchmark 提示词。内容先进入服务端审核队列（pending），通过后才对外可见。" +
				"传 client_id 可在超时后安全重放同一份上传。",
			parameters: {
				content: { type: "string", required: true, description: "提示词正文，1-8192 字符" },
				tags: { type: "array", description: "标签，小写字母数字与 -_", items: { type: "string" } },
				client_id: { type: "string", description: "幂等键；重放同一份上传时复用" },
			},
			output: {
				schema: {
					type: "object",
					additionalProperties: false,
					properties: {
						id: { type: "string", required: true },
						status: { type: "string", required: true },
					},
				},
				render: (_a, v) => [{ type: "text", text: `已提交 id=${v.id}，状态=${v.status}（审核通过后才公开）。` }],
				presentationMeta: (_a, v) => ({ id: v.id, status: v.status }),
			},
			timeoutMs: config.timeoutMs + 30_000,
			// 写请求不重试 → 独占
			isConcurrencySafe: () => false,
			async execute(a, execCtx) {
				const data = (await run(
					"upload",
					{ content: a.content, tags: a.tags, clientId: a.client_id },
					execCtx.signal,
				)) as { id: string; s: string };
				return { id: data.id, status: data.s };
			},
			presentCall: () => ({ card: "generic", title: "Bench 上传提示词", kind: "edit" }),
		}),
	);

	// ---------- 斜杠命令 ----------
	//
	// DSH 的 command handler 直接在 agent 上执行、不经过模型（交接文档 §5）；
	// 返回的 CommandResult.text 由派发它的 UI 原样渲染，这就是"把正文展示给用户"
	// 的 DSH 原语（Pi 侧是 setEditorText 回填输入框）。

	ctx.commands.register({
		name: "bench-random",
		description: "随机取一条提示词并展示（可跟标签：/bench-random coding）",
		input: { hint: "[tag]" },
		handler: async (inv) => {
			try {
				const tag = inv.rawInput.trim();
				const data = (await run("random", tag === "" ? {} : { tag }, inv.signal)) as BenchPrompt;
				return { kind: "success", text: renderPromptBlock(promptValue(data)) };
			} catch (err) {
				return { kind: "error", text: toMessage(err) };
			}
		},
	});

	ctx.commands.register({
		name: "bench-get",
		description: "按 id 取一条提示词并展示：/bench-get p_9f3a2c1b",
		input: { hint: "<id>" },
		handler: async (inv) => {
			try {
				const data = (await run("get", { id: inv.rawInput.trim() }, inv.signal)) as BenchPrompt;
				return { kind: "success", text: renderPromptBlock(promptValue(data)) };
			} catch (err) {
				return { kind: "error", text: toMessage(err) };
			}
		},
	});

	ctx.commands.register({
		name: "bench-score",
		description: "打分：/bench-score p_9f3a2c1b 5",
		input: { hint: "<id> <1-5>" },
		handler: async (inv) => {
			try {
				const parts = inv.rawInput.trim().split(/\s+/);
				if (parts.length < 2) throw new BenchError("用法: /bench-score <id> <1-5>", "bad_request", 5);
				const data = (await run("score", { id: parts[0], value: Number(parts[1]) }, inv.signal)) as {
					avg: number;
					count: number;
				};
				return { kind: "success", text: `已记录；当前均分 ${String(data.avg)}（${String(data.count)} 人）` };
			} catch (err) {
				return { kind: "error", text: toMessage(err) };
			}
		},
	});

	ctx.commands.register({
		name: "bench-sync",
		description: "增量同步本地提示词库",
		handler: async (inv) => {
			try {
				const data = (await run("sync", {}, inv.signal)) as {
					report?: { upserted?: number; deleted?: number };
					local_count?: number;
				};
				return {
					kind: "success",
					text:
						`同步完成：变更 ${String(data?.report?.upserted ?? 0)}，删除 ${String(data?.report?.deleted ?? 0)}，` +
						`本地 ${String(data?.local_count ?? 0)} 条`,
				};
			} catch (err) {
				return { kind: "error", text: toMessage(err) };
			}
		},
	});

	ctx.commands.register({
		name: "bench-status",
		description: "查看本地与服务端是否一致",
		handler: async (inv) => {
			try {
				return { kind: "success", text: formatStatus(await run("meta", {}, inv.signal)) };
			} catch (err) {
				return { kind: "error", text: toMessage(err) };
			}
		},
	});

	ctx.commands.register({
		name: "bench-list",
		description: "列出提示词摘要（可跟标签）：/bench-list coding",
		input: { hint: "[tag]" },
		handler: async (inv) => {
			try {
				const tag = inv.rawInput.trim();
				const opts = tag === "" ? { limit: 30 } : { tag, limit: 30 };
				const data = (await run("list", opts, inv.signal)) as { items?: Array<{ id: string; t?: string[] }> };
				const lines = (data.items ?? []).map((it) => `${it.id}  ${(it.t ?? []).join(",")}`);
				const body = truncate(lines.join("\n"), 40, 4_000);
				return {
					kind: "success",
					text: body.text === "" ? "（无匹配条目）" : `${String(data.items?.length ?? 0)} 条：\n${body.text}`,
				};
			} catch (err) {
				return { kind: "error", text: toMessage(err) };
			}
		},
	});

	log.info("bench 插件已装载（工具与命令在首次调用时懒探测 bench 二进制）");
}
