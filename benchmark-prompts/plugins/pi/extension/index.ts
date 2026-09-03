/**
 * bench —— 个人测试用 benchmark 提示词的 Pi 扩展。
 *
 * 形态：自定义工具（LLM 可调用）+ 斜杠命令（用户可直接触发），
 * 两者都通过 shell 调用 `bench` CLI，**本文件不含任何业务逻辑**。
 * 这是 docs/plugins.md 的核心约定：能力下沉 CLI，框架侧只做薄胶水，
 * 于是 DSH 适配可以复用同一套 bench 契约。
 *
 * 安装（任选其一）：
 *   pi -e ./plugins/pi/extension/index.ts
 *   cp -r plugins/pi/extension ~/.pi/agent/extensions/bench
 *
 * 前置：`make build-cli`，然后 `bench config init --endpoint <url>`
 * （或设 BENCH_BIN 指向 dist/bench）。
 *
 * 关于导入来源：pi 文档表格写 `typebox`，而示例里两种都有。这里统一从
 * `@earendil-works/pi-ai` 取 `Type` 与 `StringEnum`（`StringEnum` 只能从它取，
 * Google API 兼容要求），少一个模块解析假设。
 */

import { StringEnum, Type } from "@earendil-works/pi-ai";
import type { ExtensionAPI } from "@earendil-works/pi-coding-agent";
import { Text } from "@earendil-works/pi-tui";

import {
	buildArgs,
	decodeBench,
	type Exec,
	type ExecResult,
	ensureBench,
	formatStatus,
	resolveBinary,
	truncate,
} from "./bench-core.ts";

/** bench 单次调用上限。同步可能翻多页，给足但不无限。 */
const BENCH_TIMEOUT_MS = 60_000;

/**
 * 命令 handler 用到的 ctx 子集。
 *
 * 用**方法式**声明（而不是 `notify: (m: string) => void` 这种属性式函数类型）：
 * 方法参数在 TS 里是双向协变的，因此 pi 那个 kind 更窄的 notify 仍然可赋值进来；
 * 若写成属性式，strictFunctionTypes 下会因参数逆变而报不兼容。
 */
interface UICtx {
	hasUI: boolean;
	ui: {
		notify(message: string, kind?: unknown): void;
		setEditorText(text: string): void;
	};
}

/** bench 返回的提示词线格式（docs/api.md §2.1 的短字段名）。 */
interface BenchPrompt {
	id: string;
	p: string;
	t?: string[];
	v?: number;
	s?: string;
	h?: string;
}

export default function (pi: ExtensionAPI) {
	const { bin } = resolveBinary();
	const exec = pi.exec as unknown as Exec;

	// 不在工厂函数里探测二进制：pi 明确警告工厂可能在“永不开会话”的调用里执行，
	// 那时不该有进程/IO 副作用。首次用到时懒初始化，且失败不缓存（用户装好后能自愈）。
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

	/** 跑一条 bench 命令并返回解包后的 data；失败抛 BenchError（pi 转成 isError）。 */
	async function bench(command: string, opts: Record<string, unknown> = {}, signal?: AbortSignal): Promise<unknown> {
		await ensureReady();
		const args = buildArgs(command, opts as never);
		const res: ExecResult = await exec(bin, args, { signal, timeout: BENCH_TIMEOUT_MS });
		return decodeBench(res, bin, args);
	}

	// ---------- UI 辅助 ----------

	const notify = (ctx: UICtx, message: string, kind = "info") => {
		if (ctx.hasUI) ctx.ui.notify(message, kind);
	};

	/** 把整段提示词放进输入框：用户的下一步动作是复制去喂给本地待测模型。 */
	const placeInEditor = (ctx: UICtx, data: BenchPrompt) => {
		if (!ctx.hasUI) return;
		ctx.ui.setEditorText(renderPromptBlock(data));
		notify(ctx, `${data.id} 已放入输入框（${summaryOf(data)}）`, "info");
	};

	/** 统一包一层错误上报：命令路径不能抛未处理 rejection。 */
	const guarded = async (ctx: UICtx, fn: () => Promise<void>) => {
		try {
			await fn();
		} catch (err) {
			notify(ctx, err instanceof Error ? err.message : String(err), "error");
		}
	};

	// ---------- 工具 ----------

	pi.registerTool({
		name: "bench_random",
		label: "Bench 随机测试",
		description:
			"从个人 benchmark 提示词库随机取一条用于本地 LLM 测试。可按标签过滤，" + "并可排除最近已抽过的条目以避免重复。",
		promptSnippet: "随机取一条 benchmark 提示词做本地 LLM 测试",
		promptGuidelines: [
			"Use bench_random when the user asks for 随机测试 / 随机来一道 benchmark 提示词 / random prompt.",
			"Use bench_random with fresh=true when the user says they already tried the last one or wants something new.",
		],
		parameters: Type.Object({
			tag: Type.Optional(Type.String({ description: "按标签过滤，例如 coding、reasoning、systems" })),
			fresh: Type.Optional(Type.Boolean({ description: "true=排除最近抽过的条目" })),
		}),

		async execute(_id, params, signal) {
			const data = (await bench("random", { tag: params.tag, fresh: params.fresh }, signal)) as BenchPrompt;
			return promptResult(data);
		},

		renderCall(args, theme) {
			const tag = args?.tag ? theme.fg("accent", ` #${args.tag}`) : "";
			const fresh = args?.fresh ? theme.fg("dim", " fresh") : "";
			return new Text(`${theme.fg("toolTitle", theme.bold("bench_random"))}${tag}${fresh}`, 0, 0);
		},

		renderResult(result, { isPartial }, theme) {
			if (isPartial) return new Text(theme.fg("muted", "取提示词中…"), 0, 0);
			const d = (result?.details ?? {}) as { id?: string; truncated?: boolean };
			const tail = d.truncated ? theme.fg("warning", "（已截断）") : "";
			return new Text(`${theme.fg("success", `已取出 ${d.id ?? "?"}`)}${tail}`, 0, 0);
		},
	});

	pi.registerTool({
		name: "bench_get",
		label: "Bench 一键测试",
		description:
			"按 id 取一条 benchmark 提示词（一键测试）。id 可来自 bench_random、bench_catalog list 或用户直接给出。" +
			"设 local=true 只读本地缓存、不访问网络。",
		promptSnippet: "按 id 取一条指定的 benchmark 提示词",
		promptGuidelines: [
			"Use bench_get when the user gives a specific prompt id to test, or after bench_catalog list shows ids.",
			"Use bench_get with local=true when the network is down; it reads the previously synced cache.",
		],
		parameters: Type.Object({
			id: Type.String({ description: "提示词 id，形如 p_9f3a2c1b" }),
			local: Type.Optional(Type.Boolean({ description: "true=只读本地缓存，不联网" })),
		}),

		async execute(_id, params, signal) {
			const data = (await bench("get", { id: params.id, local: params.local }, signal)) as BenchPrompt;
			return promptResult(data);
		},

		renderCall(args, theme) {
			return new Text(`${theme.fg("toolTitle", theme.bold("bench_get"))} ${theme.fg("accent", args?.id ?? "?")}`, 0, 0);
		},

		renderResult(_result, { isPartial }, theme) {
			if (isPartial) return new Text(theme.fg("muted", "读取中…"), 0, 0);
			return new Text(theme.fg("success", "已读取"), 0, 0);
		},
	});

	pi.registerTool({
		name: "bench_score",
		label: "Bench 打分",
		description: "为一条 benchmark 提示词打 1-5 分。同一设备重复打分会覆盖旧分值，并返回当前均分与人数。",
		promptSnippet: "给刚测过的 benchmark 提示词打 1-5 分",
		promptGuidelines: ["Use bench_score right after the user finishes answering a bench_random or bench_get prompt."],
		parameters: Type.Object({
			id: Type.String({ description: "提示词 id" }),
			value: Type.Number({ description: "1-5 的整数评分" }),
		}),

		async execute(_id, params, signal) {
			const data = (await bench("score", { id: params.id, value: params.value }, signal)) as {
				avg: number;
				count: number;
			};
			return {
				content: [
					{
						type: "text",
						text: `已记录 ${params.value} 分；该提示词当前均分 ${data.avg}（${data.count} 人评分）。`,
					},
				],
				details: { id: params.id, value: params.value, avg: data.avg, count: data.count },
			};
		},

		renderCall(args, theme) {
			return new Text(
				`${theme.fg("toolTitle", theme.bold("bench_score"))} ${theme.fg("accent", args?.id ?? "?")} = ${String(args?.value ?? "?")}`,
				0,
				0,
			);
		},

		renderResult(result, _options, theme) {
			const d = (result?.details ?? {}) as { avg?: number; count?: number };
			return new Text(theme.fg("success", `均分 ${d.avg ?? "?"}（${d.count ?? "?"} 人）`), 0, 0);
		},
	});

	pi.registerTool({
		name: "bench_catalog",
		label: "Bench 目录",
		description:
			"浏览与维护本地提示词库。action=list 按标签列目录（只给 id 与标签，不含正文）；" +
			"action=sync 增量同步服务端新增内容；action=status 查看本地与服务端是否一致。",
		promptSnippet: "列出/搜索提示词、增量同步、查看同步状态",
		promptGuidelines: [
			"Use bench_catalog with action=status when the user asks whether the local prompt library is up to date.",
			"Use bench_catalog with action=sync before browsing so the local cache is fresh.",
		],
		parameters: Type.Object({
			action: StringEnum(["list", "sync", "status"] as const),
			tag: Type.Optional(Type.String({ description: "list 时按标签过滤" })),
			limit: Type.Optional(Type.Number({ description: "list 每页条数，默认 20，上限 100" })),
		}),

		async execute(_id, params, signal) {
			const action = String(params.action ?? "list");

			if (action === "status") {
				const data = await bench("meta", {}, signal);
				return {
					content: [{ type: "text", text: formatStatus(data) }],
					details: { action, status: data },
				};
			}

			if (action === "sync") {
				const data = await bench("sync", {}, signal);
				const wrap = data as { report?: { upserted?: number; deleted?: number }; local_count?: number };
				const rep = wrap?.report ?? {};
				return {
					content: [
						{
							type: "text",
							text: `同步完成：新增/变更 ${rep.upserted ?? 0}，删除 ${rep.deleted ?? 0}，本地共 ${wrap?.local_count ?? 0} 条。`,
						},
					],
					details: { action, sync: data },
				};
			}

			const data = (await bench("list", { tag: params.tag, limit: params.limit }, signal)) as {
				items?: Array<{ id: string; t?: string[]; v?: number }>;
				count?: number;
			};
			const lines = (data.items ?? []).map((it) => `${it.id}\tv${it.v ?? 1}\t${(it.t ?? []).join(",")}`);
			const body = truncate(lines.join("\n"), 200, 8000);
			let text = body.text;
			if (body.truncated) {
				text += `\n[仅显示部分：共 ${data.count ?? lines.length} 条，省略 ${body.omittedLines} 行；用 tag 过滤缩小范围]`;
			}
			if (text === "") text = "（没有匹配的提示词；先跑 bench_catalog action=sync 同步）";

			return {
				content: [{ type: "text", text }],
				details: { action, count: data.count ?? lines.length, truncated: body.truncated },
			};
		},

		renderCall(args, theme) {
			return new Text(
				`${theme.fg("toolTitle", theme.bold("bench_catalog"))} ${theme.fg("accent", String(args?.action ?? "?"))}`,
				0,
				0,
			);
		},

		renderResult(result, _options, theme) {
			const d = (result?.details ?? {}) as { action?: string; count?: number };
			if (d.action === "list") return new Text(theme.fg("success", `${d.count ?? 0} 条`), 0, 0);
			return new Text(theme.fg("success", "完成"), 0, 0);
		},
	});

	pi.registerTool({
		name: "bench_upload",
		label: "Bench 上传",
		description:
			"上传一条新的 benchmark 提示词。内容先进入服务端审核队列（pending），通过后才对外可见。" +
			"传 client_id 可在超时后安全重放同一份上传。",
		promptSnippet: "上传新的 benchmark 提示词（进审核队列）",
		promptGuidelines: ["Use bench_upload when the user wants to contribute a new benchmark prompt to their library."],
		parameters: Type.Object({
			content: Type.String({ description: "提示词正文，1-8192 字符" }),
			tags: Type.Optional(Type.Array(Type.String({ description: "标签，小写字母数字与 -_" }))),
			client_id: Type.Optional(Type.String({ description: "幂等键；重放同一份上传时复用" })),
		}),

		async execute(_id, params, signal) {
			const data = (await bench(
				"upload",
				{ content: params.content, tags: params.tags, clientId: params.client_id },
				signal,
			)) as { id: string; s: string };
			return {
				content: [
					{
						type: "text",
						text: `已提交 id=${data.id}，状态=${data.s}（审核通过后才公开）。`,
					},
				],
				details: data,
			};
		},

		renderCall(_args, theme) {
			return new Text(theme.fg("toolTitle", theme.bold("bench_upload")), 0, 0);
		},

		renderResult(result, _options, theme) {
			const d = (result?.details ?? {}) as { id?: string };
			return new Text(theme.fg("success", `已提交 ${d.id ?? "?"}`), 0, 0);
		},
	});

	// ---------- 斜杠命令 ----------
	//
	// 每个 handler 入口先过一次 asCtx 窄化：命令类命令需要的是 hasUI + 两个 ui 方法，
	// 把转换集中在一处，而不是每个调用点重复写双重断言。

	const asCtx = (ctx: unknown): UICtx => ctx as UICtx;

	pi.registerCommand("bench-random", {
		description: "随机取一条提示词放进输入框（可跟标签：/bench-random coding）",
		handler: async (args, ctx) => {
			const c = asCtx(ctx);
			await guarded(c, async () => {
				const tag = String(args ?? "").trim();
				const data = (await bench("random", tag === "" ? {} : { tag })) as BenchPrompt;
				placeInEditor(c, data);
			});
		},
	});

	pi.registerCommand("bench-get", {
		description: "按 id 取一条提示词放进输入框：/bench-get p_9f3a2c1b",
		handler: async (args, ctx) => {
			const c = asCtx(ctx);
			await guarded(c, async () => {
				const data = (await bench("get", { id: String(args ?? "").trim() })) as BenchPrompt;
				placeInEditor(c, data);
			});
		},
	});

	pi.registerCommand("bench-score", {
		description: "打分：/bench-score p_9f3a2c1b 5",
		handler: async (args, ctx) => {
			const c = asCtx(ctx);
			await guarded(c, async () => {
				const parts = String(args ?? "")
					.trim()
					.split(/\s+/);
				if (parts.length < 2) throw new Error("用法: /bench-score <id> <1-5>");
				const data = (await bench("score", { id: parts[0], value: Number(parts[1]) })) as {
					avg: number;
					count: number;
				};
				notify(c, `已记录；当前均分 ${data.avg}（${data.count} 人）`);
			});
		},
	});

	pi.registerCommand("bench-sync", {
		description: "增量同步本地提示词库",
		handler: async (_args, ctx) => {
			const c = asCtx(ctx);
			await guarded(c, async () => {
				const data = (await bench("sync", {})) as {
					report?: { upserted?: number; deleted?: number };
					local_count?: number;
				};
				notify(
					c,
					`同步完成：变更 ${data?.report?.upserted ?? 0}，删除 ${data?.report?.deleted ?? 0}，本地 ${data?.local_count ?? 0} 条`,
				);
			});
		},
	});

	pi.registerCommand("bench-status", {
		description: "查看本地与服务端是否一致",
		handler: async (_args, ctx) => {
			const c = asCtx(ctx);
			await guarded(c, async () => {
				notify(c, formatStatus(await bench("meta", {})));
			});
		},
	});

	pi.registerCommand("bench-list", {
		description: "列出提示词摘要（可跟标签）",
		handler: async (args, ctx) => {
			const c = asCtx(ctx);
			await guarded(c, async () => {
				const tag = String(args ?? "").trim();
				const opts = tag === "" ? { limit: 30 } : { tag, limit: 30 };
				const data = (await bench("list", opts)) as { items?: Array<{ id: string; t?: string[] }> };
				const lines = (data.items ?? []).map((it) => `${it.id}  ${(it.t ?? []).join(",")}`);
				const body = truncate(lines.join("\n"), 40, 4000);
				notify(c, body.text === "" ? "（无匹配条目）" : `${data.items?.length ?? 0} 条：${clampForNotice(body.text)}`);
			});
		},
	});

	// ---------- 启动提示 ----------

	pi.on("session_start", async (_event, ctx) => {
		if (!ctx.hasUI) return;
		try {
			await ensureReady();
		} catch (err) {
			ctx.ui.notify(`bench 扩展不可用：${err instanceof Error ? err.message : String(err)}`, "warning");
		}
	});
}

// ---------- 纯渲染辅助（不依赖 pi 运行时，便于单测） ----------

/** 给 LLM 看的工具结果：正文 + 明确的下一步指示。 */
function promptResult(data: BenchPrompt) {
	const body = truncate(data.p ?? "", 120, 12_000);
	let text = renderPromptBlock(data);
	if (body.truncated) {
		text += `\n[正文过长已截断：共 ${body.totalLines} 行，省略 ${body.omittedLines} 行]`;
	}
	text += `\n把上面这段原样发给被测模型；回答完成后调用 bench_score(id="${data.id}", value=1-5) 记录评分。`;

	return {
		content: [{ type: "text", text }],
		details: { id: data.id, tags: data.t ?? [], version: data.v ?? 1, truncated: body.truncated },
	};
}

/** 提示词块：人复制与 LLM 转交用的是同一份文本。 */
function renderPromptBlock(data: BenchPrompt): string {
	const body = truncate(data.p ?? "", 120, 12_000);
	const header = `【Benchmark 提示词 ${data.id}】标签: ${(data.t ?? []).join(", ") || "-"}  v${data.v ?? 1}`;
	return `${header}\n----------------------------------\n${body.text}\n----------------------------------`;
}

function summaryOf(data: BenchPrompt): string {
	return `${(data.t ?? []).join(",") || "无标签"} v${data.v ?? 1}`;
}

/** notify 是单行 UI，压成一行并限长。 */
function clampForNotice(s: string): string {
	const one = s.replace(/\s+/g, " ").trim();
	return one.length > 400 ? `${one.slice(0, 400)}…` : one;
}
