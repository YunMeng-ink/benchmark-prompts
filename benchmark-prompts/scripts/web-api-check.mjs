/**
 * 直接对真源站跑「浏览器实际执行的那份 api.ts」（web/src/lib/api.ts）。
 *
 * 为什么要跑它而不是复刻一份断言：api.ts 是前端唯一的取数入口，复刻就等于
 * 测了一个可能已经漂移的副本。Node 26 原生执行 .ts，所以可以真 import。
 *
 * 由 scripts/smoke-web.sh 调用，参数走环境变量：
 *   WEB_BASE   源站根地址（故意带 /v1 也行，用来验证归一）
 *   WEB_KEY    一个可写的 API Key
 *   WEB_SEED   已审核提示词的 id
 *   WEB_MARK  那条种子里的唯一标记串（用来证明数据真的来自源站）
 *   WEB_TAG    种子使用的标签
 * 输出每行 `OK <说明>` 或 `ERR <说明>`，退出码非 0 表示有失败。
 */
import { ApiError, api, describeError, setApiKey } from "../web/src/lib/api.ts";

const base = process.env.WEB_BASE ?? "";
const key = process.env.WEB_KEY ?? "";
const seedId = process.env.WEB_SEED ?? "";
const mark = process.env.WEB_MARK ?? "";
const tag = process.env.WEB_TAG ?? "smoke-web";

let failures = 0;
const ok = (m) => console.log(`OK ${m}`);
const err = (m) => {
	failures += 1;
	console.log(`ERR ${m}`);
};

// —— 浏览器环境的最小 shim ——
const store = new Map();
globalThis.window = {
	localStorage: {
		getItem: (k) => (store.has(k) ? store.get(k) : null),
		setItem: (k, v) => store.set(k, String(v)),
		removeItem: (k) => store.delete(k),
	},
	// 故意写成带 /v1 的形式：docs/api.md 的 Base URL 就是这么写的，
	// 前端必须像 Go SDK 的 NormalizeEndpoint 一样把它剔掉。
	__BENCH_WEB__: { apiBase: base },
};

// 记录真实请求 URL，用来断言没有出现 /v1/v1 这种双重前缀。
let lastUrl = "";
const realFetch = globalThis.fetch;
globalThis.fetch = (u, init) => {
	lastUrl = String(u);
	return realFetch(u, init);
};

if (!base || !seedId) {
	err("缺少 WEB_BASE / WEB_SEED 环境变量");
	process.exit(1);
}

// —— 1. 只读列表 ——
const list = await api.list({ limit: 20 });
if (lastUrl.includes("/v1/v1")) err(`地址归一失败，请求打到了 ${lastUrl}`);
else if (!lastUrl.endsWith("/v1/prompts?limit=20")) err(`请求 URL 不符预期：${lastUrl}`);
else ok("endpoint 归一正确（粘进来的 /v1 被剔掉，路径仍含 /v1）");

const items = list.data?.items ?? [];
if (items.length === 0) err("列表为空，种子数据没生效");
else ok(`列表返回 ${items.length} 条摘要`);
if (items[0] && "p" in items[0]) err("摘要里泄漏了正文 p 字段");
else ok("摘要不含正文（与 §2.2 一致）");
if (list.cursor === null || typeof list.cursor === "string") ok("游标是原样透传的不透明字符串");
else err(`游标形态异常：${typeof list.cursor}`);

// —— 2. 详情必须带源站唯一标记，证明数据不是本地编造 ——
const one = await api.get(seedId);
if (one.data?.p?.includes(mark)) ok("详情正文来自源站（含唯一标记）");
else err("详情正文缺少唯一标记 → 数据链路可疑");
if (one.data?.id !== seedId) err("详情 id 与请求不一致");

// —— 3. 按标签随机 ——
const rnd = await api.random({ tag });
if (rnd.data?.id === seedId && rnd.data?.p?.includes(mark)) ok("随机端点返回完整正文");
else err(`按标签随机拿到的不是那条：${rnd.data?.id}`);

// —— 4. 打分统计（§3.8 存在的理由：没打过分也读得到） ——
const st0 = await api.stats(seedId);
if (st0.data?.count === 0 && st0.data?.avg === 0) ok("无人打分时统计是 0/0，且匿名可读");
else err(`初始统计应为 0/0，得到 ${JSON.stringify(st0.data)}`);

// —— 5. 写入需要 Key ——
setApiKey("");
try {
	await api.score(seedId, 5);
	err("未带 Key 的打分竟然成功");
} catch (e) {
	if (e instanceof ApiError && (e.code === "unauthorized" || e.code === "forbidden"))
		ok("匿名打分被拒（401/403）");
	else err(`匿名打分应被拒，实际 ${e?.code ?? e}`);
	if (describeError(e).includes("Key")) ok("错误码翻译成了指向「填 Key」的中文提示");
	else err(`降级提示没提到 Key：${describeError(e)}`);
}

setApiKey(key);
const sc = await api.score(seedId, 4);
if (sc?.count === 1 && sc?.avg === 4) ok("带 Key 打分成功（avg=4 count=1）");
else err(`打分响应异常：${JSON.stringify(sc)}`);

const st1 = await api.stats(seedId);
if (st1.data?.count === 1 && st1.data?.avg === 4) ok("统计端点反映了刚提交的分数");
else err(`统计未更新：${JSON.stringify(st1.data)}`);

// —— 6. 上传进审核队列 ——
const up = await api.upload(`前端冒烟上传的一条，唯一标记 WEBUP-3f70-${Date.now()}`, [tag]);
if (up?.s === "pending" && up?.id) ok(`上传进入审核队列（${up.id} / pending）`);
else err(`上传结果异常：${JSON.stringify(up)}`);

// —— 7. 不存在的 id ——
try {
	await api.get("p_absent_zzz");
	err("不存在的 id 竟然返回成功");
} catch (e) {
	if (e instanceof ApiError && e.code === "not_found") ok("not_found 错误码正确");
	else err(`应报 not_found，实际 ${e?.code ?? e}`);
	if (!describeError(e).includes("不存在")) err(`错误文案不可行动：${describeError(e)}`);
	else ok("not_found 翻译成中文可行动提示");
}

// —— 8. 自助注册闭环（邀请码 → Key → 自视 → 作废）——
const invite = process.env.WEB_INVITE ?? "";
if (invite) {
	setApiKey(""); // 用匿名身份申请
	const issued = await api.registerKey(invite, "冒烟机");
	if (!issued?.key?.startsWith("bk_")) err(`注册没返回可用 Key：${JSON.stringify(issued)}`);
	else ok("邀请码换到自助 Key");
	if (issued?.scope !== "writer") err(`自助 Key 作用域应为 writer，得到 ${issued?.scope}`);
	else ok("自助 Key 作用域是 writer（不是 admin）");

	setApiKey(issued.key);
	const self = (await api.selfKey(issued.key)).data;
	if (!self?.ref || self.enabled !== true) err(`自视信息异常：${JSON.stringify(self)}`);
	else ok("自视端点报告 ref/enabled");
	if (JSON.stringify(self).includes(issued.key)) err("自视端点回显了明文 Key");
	else ok("自视端点不回显明文 Key");

	const st = await api.score(seedId, 3);
	if (st?.count >= 1) ok("writer Key 可以打分");
	else err(`writer Key 打分结果异常：${JSON.stringify(st)}`);

	const rev = (await api.revokeSelfKey()).data;
	if (rev?.revoked !== true) err(`作废未被确认：${JSON.stringify(rev)}`);
	else ok("可作废自己这把 Key");

	try {
		await api.selfKey();
		err("已作废的 Key 竟仍可用");
	} catch (e) {
		if (e instanceof ApiError && e.code === "unauthorized") ok("作废后请求被拒（401）");
		else err(`作废后应 401，实际 ${e?.code ?? e}`);
	}

	// 同设备重复申请必须被拒（一设备一 Key）
	setApiKey("");
	try {
		await api.registerKey(invite, "重复");
		err("同一设备重复申请竟然成功");
	} catch (e) {
		if (e instanceof ApiError && e.code === "conflict") ok("同设备重复申请 → conflict");
		else err(`应 conflict，实际 ${e?.code ?? e}`);
	}

	// 无效邀请码：forbidden，且文案不区分"不存在/过期/用尽"
	try {
		await api.registerKey("BOGUS-CODE", "x");
		err("无效邀请码竟然通过");
	} catch (e) {
		if (e instanceof ApiError && e.code === "forbidden") ok("无效邀请码 → forbidden");
		else err(`应 forbidden，实际 ${e?.code ?? e}`);
		if (!describeError(e).includes("Key")) err(`无效码的提示不可行动：${describeError(e)}`);
		else ok("无效码提示指向填 Key/邀请码");
	}
} else {
	console.log("SKIP 自助注册闭环（未提供 WEB_INVITE）");
}

// —— 9. 未配置地址 → local_error，而不是含糊的网络错误 ——
globalThis.window.__BENCH_WEB__.apiBase = "";
try {
	await api.meta();
	err("没配地址竟然成功");
} catch (e) {
	if (e instanceof ApiError && e.code === "local_error") ok("未配置地址时报 local_error");
	else err(`应报 local_error，实际 ${e?.code ?? e}`);
	if (!describeError(e).includes("连接设置")) err(`未配置时的提示没指向设置面板：${describeError(e)}`);
	else ok("未配置时提示指向「连接设置」面板");
}

process.exit(failures > 0 ? 1 : 0);
