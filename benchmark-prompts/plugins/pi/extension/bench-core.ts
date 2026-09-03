/**
 * bench-core.ts —— Pi 扩展的纯逻辑层。
 *
 * 刻意**不 import 任何 pi / typebox 模块**，因此可以被 `node --test` 直接驱动
 * （Node 22.7+ 原生剥离类型）。这是本文件存在的全部理由：
 * 最容易出错的是 argv 构造、退出码→错误语义映射、以及输出截断，
 * 而这些恰好都不需要 pi 运行时就能验证。
 *
 * 扩展入口 index.ts 只负责把这里的函数接到 pi.registerTool / registerCommand。
 */

/** 一次 bench 调用的原始结果（与 pi.exec 的返回结构一致）。 */
export interface ExecResult {
	stdout: string;
	stderr: string;
	code: number;
	killed?: boolean;
}

export type Exec = (
	bin: string,
	args: string[],
	opts?: { signal?: AbortSignal; timeout?: number },
) => Promise<ExecResult>;

/** bench 的退出码约定，见 docs/client.md §11。必须与服务端保持一致。 */
export const ExitCode = {
	OK: 0,
	NETWORK: 1,
	RATE_LIMITED: 2,
	AUTH: 3,
	NOT_FOUND: 4,
	BAD_INPUT: 5,
} as const;

/** 退出码 → 人类可读归类。用于把 CLI 失败翻译成对 LLM 有用的话。 */
export function exitCodeLabel(code: number): string {
	switch (code) {
		case ExitCode.NETWORK:
			return "网络或服务端故障";
		case ExitCode.RATE_LIMITED:
			return "被限流，稍后重试";
		case ExitCode.AUTH:
			return "鉴权失败，需要配置 API Key";
		case ExitCode.NOT_FOUND:
			return "资源不存在";
		case ExitCode.BAD_INPUT:
			return "参数或校验错误";
		default:
			return `bench 退出码 ${code}`;
	}
}

/** bench 失败时抛出的错误。message 会被 pi 报告给 LLM。 */
export class BenchError extends Error {
	readonly code: string;
	readonly exitCode: number;
	/** 是否值得让模型换个参数再试一次（区别于"服务挂了"）。 */
	readonly retryable: boolean;

	constructor(message: string, code: string, exitCode: number) {
		super(message);
		this.name = "BenchError";
		this.code = code;
		this.exitCode = exitCode;
		this.retryable = exitCode === ExitCode.NETWORK || exitCode === ExitCode.RATE_LIMITED;
	}
}

/**
 * 解析 bench 二进制路径。
 *
 * 优先级：显式配置 > BENCH_BIN > BENCH_DIST 目录下的平台产物 > PATH 上的 bench。
 * 不在这里做存在性检查——交给 probeBench()，因为 fs 访问在这个模块里不好测。
 */
export function resolveBinary(env: NodeJS.ProcessEnv = process.env): { bin: string; source: string } {
	if (env.BENCH_BIN && env.BENCH_BIN.trim() !== "") {
		return { bin: env.BENCH_BIN.trim(), source: "BENCH_BIN" };
	}
	// PATH 上的 bench；pi 自己就是 bench 的同源项目产物，正常安装后直接可用
	return { bin: "bench", source: "PATH" };
}

/**
 * 构造 bench 的参数数组。
 *
 * 规则：**选项在前、位置参数在后**（`bench get --json --local p_1`），
 * 与 bench 自己接受的写法一致（bench 会先把选项与位置参数分拢）。
 *
 * 单独成函数是因为这是最容易静默出错的地方：参数拼错时 bench 会返回 5 号退出码，
 * 而 LLM 只看到一句“参数错误”，很难反推。测试直接钉住确切 argv。
 */
export function buildArgs(
	command: string,
	opts: Record<string, string | number | boolean | string[] | undefined> = {},
): string[] {
	// flags 与 positional 分开收集，最后才拼接，避免位置参数插在选项中间
	const flags: string[] = [];
	const positional: string[] = [];

	const push = (name: string, value: string | number | boolean | string[] | undefined, flag: string = `--${name}`) => {
		if (value === undefined || value === "" || value === false) return;
		if (value === true) {
			flags.push(flag);
			return;
		}
		if (Array.isArray(value)) {
			if (value.length > 0) flags.push(flag, value.join(","));
			return;
		}
		flags.push(flag, String(value));
	};

	// 所有子命令统一 --json，避免解析人类可读文本
	flags.push("--json");

	switch (command) {
		case "random":
			push("tag", opts.tag);
			push("exclude", opts.exclude);
			push("fresh", opts.fresh);
			break;
		case "get":
			push("local", opts.local);
			positional.push(assertId(opts.id, "bench get"));
			break;
		case "score":
			positional.push(assertId(opts.id, "bench score"));
			positional.push(String(assertScoreValue(opts.value)));
			break;
		case "list":
			push("tag", opts.tag);
			push("limit", opts.limit);
			push("all", opts.all);
			break;
		case "upload":
			push("c", opts.content, "-c");
			push("f", opts.file, "-f");
			push("t", opts.tags, "-t");
			push("client-id", opts.clientId);
			break;
		case "sync":
		case "meta":
		case "reset":
		case "version":
			break;
		default:
			throw new BenchError(`未知的 bench 子命令 ${command}`, "bad_command", ExitCode.BAD_INPUT);
	}

	const home = typeof opts.home === "string" ? opts.home.trim() : "";
	if (home !== "") flags.push("--home", home);

	return [command, ...flags, ...positional];
}

/**
 * 校验 score 入参。放在这里是因为它必须与 bench 自身的 1-5 校验一致，
 * 而在扩展层提前拦掉可以省一次进程启动。
 */
export function assertScoreValue(value: unknown): number {
	// 用 Number 而不是 parseInt：parseInt("3.5") === 3 会静默把无效入参变成有分的一票。
	const n = typeof value === "number" ? value : Number(String(value ?? "").trim());
	if (!Number.isInteger(n) || n < 1 || n > 5) {
		throw new BenchError("score 的 value 必须是 1-5 的整数", "validation_failed", ExitCode.BAD_INPUT);
	}
	return n;
}

/** 校验 id，避免把空串拼成 `bench get --json` 这种缺位置参数的畸形命令。 */
export function assertId(id: unknown, tool: string): string {
	const s = typeof id === "string" ? id.trim() : "";
	if (s === "") {
		throw new BenchError(`${tool} 需要非空的 id`, "bad_request", ExitCode.BAD_INPUT);
	}
	return s;
}

/**
 * 从 bench 的输出里解出 data，或在失败时抛 BenchError。
 *
 * 注意 bench 的 `--json` 约定：**出错时错误信封也走 stdout**（见 docs/client.md §5），
 * 所以不能只看 stderr。
 */
/** bench 信封的解析形状（tsc 下 `as typeof envelope` 会自指引出 never，命名类型才站得住）。 */
interface BenchEnvelope {
	ok?: boolean;
	data?: unknown;
	error?: { code?: string; message?: string } | null;
}

export function decodeBench(res: ExecResult, bin: string, args: string[]): unknown {
	const trimmed = res.stdout.trim();

	let envelope: BenchEnvelope | null = null;

	if (trimmed !== "") {
		try {
			envelope = JSON.parse(trimmed) as BenchEnvelope;
		} catch {
			envelope = null;
		}
	}

	if (res.killed) {
		throw new BenchError(`bench 调用被取消或超时：${bin} ${args.join(" ")}`, "canceled", ExitCode.NETWORK);
	}

	if (res.code !== ExitCode.OK) {
		const code = envelope?.error?.code ?? "unknown";
		const detail = envelope?.error?.message ?? lastLine(res.stderr) ?? "";
		const label = exitCodeLabel(res.code);
		throw new BenchError(
			`bench ${args[0]} 失败（${label}）${code ? ` [${code}]` : ""}${detail ? `：${detail}` : ""}`,
			code,
			res.code,
		);
	}

	if (envelope === null) {
		throw new BenchError(
			`bench ${args[0]} 未返回可解析的 JSON；请确认 bench 版本是否过旧（需支持 --json）`,
			"bad_response",
			ExitCode.BAD_INPUT,
		);
	}
	if (envelope.ok !== true) {
		const code = envelope.error?.code ?? "unknown";
		throw new BenchError(
			`bench ${args[0]} 返回失败信封：${envelope.error?.message ?? "无原因"}`,
			code,
			ExitCode.BAD_INPUT,
		);
	}
	return envelope.data;
}

function lastLine(s: string): string | undefined {
	const lines = s
		.split("\n")
		.map((l) => l.trim())
		.filter((l) => l !== "");
	return lines.length > 0 ? lines[lines.length - 1] : undefined;
}

/**
 * 探测 bench 是否可用；不可用时给出可直接照做的安装指引。
 *
 * 探测命令必须是 `bench version --json`，不能是 `bench --json`：
 * bench 把第一个位置当子命令名，`--json` 会被当成未知命令而返回退出码 5，
 * 于是把"完全正常的 bench"误判成"版本过旧"。
 */
export async function ensureBench(exec: Exec, bin: string): Promise<void> {
	const probe = ["version", "--json"];
	let res: ExecResult;
	try {
		res = await exec(bin, probe, { timeout: 10_000 });
	} catch (err) {
		throw new BenchError(
			`找不到 bench 可执行文件（${bin}）。请先构建：在 benchmark-prompts 目录执行 make build-cli，` +
				`再把 dist/bench 加入 PATH，或设置环境变量 BENCH_BIN 指向它。原因：${describeErr(err)}`,
			"bench_missing",
			ExitCode.BAD_INPUT,
		);
	}
	if (res.code !== ExitCode.OK) {
		throw new BenchError(
			`执行 \`bench ${probe.join(" ")}\` 失败（${exitCodeLabel(res.code)}），请重新构建 bench。`,
			"bench_outdated",
			ExitCode.BAD_INPUT,
		);
	}
	// 进一步确认输出真的是 bench 的信封，而不是某个同名命令的帮助文本
	try {
		decodeBench(res, bin, probe);
	} catch (err) {
		if (err instanceof BenchError && err.code === "bad_response") {
			throw new BenchError(
				`\`${bin} ${probe.join(" ")}\` 没有返回 bench 的 JSON 信封，` +
					`怀疑命中的不是本项目的 bench（PATH 里有同名命令？）。请显式设 BENCH_BIN。`,
				"bench_wrong_binary",
				ExitCode.BAD_INPUT,
			);
		}
		throw err;
	}
}

function describeErr(err: unknown): string {
	if (err instanceof Error) return err.message;
	return String(err);
}

export interface Truncation {
	text: string;
	truncated: boolean;
	totalLines: number;
	omittedLines: number;
}

/**
 * 截断输出，避免把整个提示词库灌进 LLM 上下文。
 *
 * 这里自带实现而不用 pi 的 truncateHead，是为了让这段逻辑可单测
 * （pi 的截断工具需要 pi 运行时）。限制刻意保守：正文最长 120 行 / 12KB。
 *
 * 特别注意**单行超长**的情形：benchmark 提示词经常整段不带换行，
 * 若只按行边界裁剪，首行超字节限额会导致返回空串（丢光内容）。
 */
export function truncate(text: string, maxLines = 120, maxBytes = 12_000): Truncation {
	const allLines = text.split("\n");
	if (allLines.length <= maxLines && byteLen(text) <= maxBytes) {
		return { text, truncated: false, totalLines: allLines.length, omittedLines: 0 };
	}

	let out = "";
	let lines = 0;
	for (const line of allLines) {
		if (lines >= maxLines) break;
		const candidate = out === "" ? line : `${out}\n${line}`;
		if (byteLen(candidate) > maxBytes) break;
		out = candidate;
		lines++;
	}

	// 一行都没装下：按码点硬切首行，保证不截出半个字符、也不空手而归。
	if (out === "" && allLines.length > 0) {
		out = cutByBytes(allLines[0], maxBytes);
		lines = out === "" ? 0 : 1;
	}

	return {
		text: out,
		truncated: true,
		totalLines: allLines.length,
		omittedLines: Math.max(allLines.length - lines, 0),
	};
}

/** 按 UTF-8 字节上限安全切割（不拆代理对）。 */
function cutByBytes(s: string, maxBytes: number): string {
	if (maxBytes <= 0) return "";
	let out = "";
	let used = 0;
	for (const ch of s) {
		const size = charBytes(ch.codePointAt(0) ?? 0);
		if (used + size > maxBytes) break;
		out += ch;
		used += size;
	}
	return out;
}

/** 一个码点占多少 UTF-8 字节。 */
function charBytes(cp: number): number {
	if (cp < 0x80) return 1;
	if (cp < 0x800) return 2;
	if (cp < 0x10000) return 3;
	return 4;
}

function byteLen(s: string): number {
	// 无 Buffer 依赖的粗略 UTF-8 长度估算，足够用于限额
	let n = 0;
	for (const ch of s) {
		n += charBytes(ch.codePointAt(0) ?? 0);
	}
	return n;
}

/** 把 bench 的 meta 结果压成一行状态，用于工具结果与 /bench-status。 */
export function formatStatus(data: unknown): string {
	const s = data as {
		server_total?: number;
		local_count?: number;
		up_to_date?: boolean;
		server_hash?: string;
	};
	if (!s || typeof s.server_total !== "number") return "状态不可用";
	return `服务端 ${s.server_total} 条 / 本地 ${s.local_count ?? 0} 条 / ${s.up_to_date ? "已同步" : "落后，需 sync"}`;
}
