/**
 * /v1 薄封装：信封解包、错误码归一、Bearer 注入。
 * 字段名严格照 docs/api.md（压缩过的 p/t/v/s/h 是线上格式，不是可读英文名）。
 */

export interface ErrorBody {
	code: string;
	message: string;
	retry_after?: number;
}

/** 信封的四个字段恒定出现（值可为 null），见 docs/api.md §1.1。 */
export interface Envelope<T> {
	ok: boolean;
	data: T | null;
	error: ErrorBody | null;
	cursor: string | null;
	v: number;
}

export interface PromptSummary {
	id: string;
	t: string[];
	v: number;
	h: string;
}

export interface Prompt extends PromptSummary {
	p: string;
	s: string;
}

export interface ListData {
	items: PromptSummary[];
	has_more: boolean;
}

export interface MetaData {
	total: number;
	catalog_hash: string;
	schema_version: number;
	server_time: number;
}

export interface ScoreStats {
	id: string;
	avg: number;
	count: number;
}

export interface UploadData {
	id: string;
	s: string;
}

/** 携带契约错误码的异常，调用方据 code 分支（§4.4 的降级提示靠它）。 */
export class ApiError extends Error {
	readonly code: string;
	readonly retryAfter: number | null;
	readonly httpStatus: number;

	constructor(body: ErrorBody | null, httpStatus: number) {
		super(body?.message ?? `请求失败（HTTP ${httpStatus}）`);
		this.name = "ApiError";
		this.code = body?.code ?? "unavailable";
		this.retryAfter = typeof body?.retry_after === "number" ? body.retry_after : null;
		this.httpStatus = httpStatus;
	}
}

const LS_BASE = "bench_api_base";
const LS_KEY = "bench_api_key";
const LS_DEVICE = "bench_device_id";
const LS_CLIENT = "bench_client_id";

function lsGet(k: string): string {
	// 岛在构建期也会被服务端渲染一次（Astro prerender），那里没有 localStorage。
	if (typeof globalThis.window === "undefined") return "";
	try {
		return globalThis.window.localStorage.getItem(k) ?? "";
	} catch {
		return "";
	}
}

function lsSet(k: string, v: string): void {
	if (typeof globalThis.window === "undefined") return;
	try {
		globalThis.window.localStorage.setItem(k, v);
	} catch {
		// 隐私模式下 localStorage 会抛；忽略即可，功能退化为“本次会话内有效”。
	}
}

/** 与 Go SDK 的 NormalizeEndpoint 同构：endpoint 一律是源站根，路径负 /v1 由客户端拼。
 *  这样用户把 docs/api.md 里的 Base URL（含 /v1）直接粘进来也能用。 */
function normalizeBase(raw: string): string {
	let v = raw.trim();
	if (v === "") return "";
	if (!v.includes("://")) v = `https://${v}`;
	v = v.replace(/\/+$/, "");
	v = v.replace(/\/v1$/, "");
	return v.replace(/\/+$/, "");
}

/** 运行期覆盖优先，其次 public/runtime-config.js 的构建/部署期值。 */
export function apiBase(): string {
	return normalizeBase(lsGet(LS_BASE) || globalThis.window?.__BENCH_WEB__?.apiBase || "");
}

export function setApiBase(v: string): void {
	lsSet(LS_BASE, normalizeBase(v));
}

export function apiKey(): string {
	return lsGet(LS_KEY);
}

export function setApiKey(v: string): void {
	lsSet(LS_KEY, v.trim());
}

/** deviceId 用于打分去重、clientId 用于上传幂等：都要跨刷新稳定。 */
function stableId(key: string): string {
	let v = lsGet(key);
	if (!v) {
		v = globalThis.crypto?.randomUUID?.() ?? `w_${Date.now()}_${Math.random().toString(16).slice(2)}`;
		lsSet(key, v);
	}
	return v;
}

export function deviceId(): string {
	return stableId(LS_DEVICE);
}

export function clientId(): string {
	return stableId(LS_CLIENT);
}

const LS_RECENT = "bench_recent_ids";
const RECENT_MAX = 40;

/** 记下最近看过的 id，供 `random?exclude=` 避免重复抽到（§3.4 上限 100）。 */
export function noteRecent(id: string): void {
	const cur = recentIds(RECENT_MAX);
	const next = [id, ...cur.filter((x) => x !== id)].slice(0, RECENT_MAX);
	lsSet(LS_RECENT, next.join(","));
}

export function recentIds(limit = 20): string[] {
	return lsGet(LS_RECENT)
		.split(",")
		.map((s) => s.trim())
		.filter((s) => s !== "")
		.slice(0, limit);
}

export function hasApiBase(): boolean {
	return apiBase() !== "";
}

async function request<T>(path: string, init: RequestInit = {}): Promise<Envelope<T>> {
	const base = apiBase();
	if (!base) throw new ApiError({ code: "local_error", message: "尚未配置源站地址" }, 0);

	const headers: Record<string, string> = { Accept: "application/json", ...(init.headers as Record<string, string>) };
	// 只带 Bearer：源站 auth.go 的优先级是 Bearer > HMAC 签名，前端不该持有 secret。
	const key = apiKey();
	if (key) headers.Authorization = `Bearer ${key}`;

	let res: Response;
	try {
		// 不要手写 Accept-Encoding：浏览器自动带且自动解压，手动设反而要自己解压。
		res = await fetch(`${base}/v1${path}`, { ...init, headers });
	} catch {
		throw new ApiError({ code: "network", message: "无法连接源站（可能在离线或跨域被拒）" }, 0);
	}

	let env: Envelope<T>;
	try {
		env = (await res.json()) as Envelope<T>;
	} catch {
		throw new ApiError({ code: "unavailable", message: `响应不是合法 JSON（HTTP ${res.status}）` }, res.status);
	}
	if (!env.ok) throw new ApiError(env.error, res.status);
	return env;
}

const qs = (params: Record<string, string | number | undefined>): string => {
	const q = new URLSearchParams();
	for (const [k, v] of Object.entries(params)) {
		if (v !== undefined && v !== "") q.set(k, String(v));
	}
	const s = q.toString();
	return s ? `?${s}` : "";
};

/** 自助注册的返回值：明文 Key 只在这里出现一次。 */
export interface KeyIssue {
	key: string;
	ref: string;
	name: string;
	scope: string;
	deviceId: string;
}

export interface KeySelf {
	ref: string;
	name: string;
	scope: string;
	deviceId: string;
	enabled: boolean;
	created_at: number;
}

export const api = {
	meta: () => request<MetaData>("/meta"),
	list: (opts: { cursor?: string; limit?: number; tag?: string }) => request<ListData>(`/prompts${qs(opts)}`),
	get: (id: string) => request<Prompt>(`/prompts/${encodeURIComponent(id)}`),
	random: (opts: { tag?: string; exclude?: string }) => request<Prompt>(`/prompts/random${qs(opts)}`),
	stats: (id: string) => request<ScoreStats>(`/prompts/${encodeURIComponent(id)}/score`),
	score: (id: string, value: number) =>
		request<ScoreStats>("/scores", {
			method: "POST",
			headers: { "Content-Type": "application/json" },
			body: JSON.stringify({ id, value, deviceId: deviceId() }),
		}).then((e) => e.data as { avg: number; count: number }),
	/** 用邀请码换一把绑定本设备的 writer Key；deviceId 沿用打分那套稳定指纹。 */
	registerKey: (inviteCode: string, label = "") =>
		request<KeyIssue>("/keys", {
			method: "POST",
			headers: { "Content-Type": "application/json" },
			body: JSON.stringify({ inviteCode, deviceId: deviceId(), label }),
		}).then((e) => e.data as KeyIssue),
	selfKey: () => request<KeySelf>("/keys/self"),
	revokeSelfKey: () => request<{ ref: string; revoked: boolean }>("/keys/self", { method: "DELETE" }),
	upload: (content: string, tags: string[]) =>
		request<UploadData>("/prompts", {
			method: "POST",
			headers: { "Content-Type": "application/json" },
			body: JSON.stringify({ p: content, t: tags, clientId: clientId() }),
		}).then((e) => e.data as UploadData),
};

/** 把错误码翻译成中文可行动提示（§4.4）。 */
export function describeError(err: unknown): string {
	if (!(err instanceof ApiError)) return err instanceof Error ? err.message : "未知错误";
	switch (err.code) {
		case "local_error":
			return "还没配置源站地址——在下方「连接设置」里填一次即可。";
		case "network":
			return "连不上源站：可能在离线、或该站点未把你当前的源加入 CORS 白名单。";
		case "unauthorized":
		case "forbidden":
			return "写入需要有效的 API Key（只读不必）。请在「连接设置」里填 Key。";
		case "not_found":
			return "这条提示词不存在或未通过审核。";
		case "validation_failed":
			return `输入未通过校验：${err.message}`;
		case "rate_limited":
			return err.retryAfter ? `操作太频繁，请 ${err.retryAfter} 秒后重试。` : "操作太频繁，稍后重试。";
		case "too_large":
			return "正文超出长度上限。";
		case "unavailable":
			return err.retryAfter
				? `源站正在降级限流，请 ${err.retryAfter} 秒后重试。`
				: "源站暂时不可用（可能在降级或维护）。";
		default:
			return `${err.code}：${err.message}`;
	}
}
