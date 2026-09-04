import { useEffect, useState } from "preact/hooks";

import { api, apiBase, apiKey, describeError, hasApiBase, type KeyIssue, setApiBase, setApiKey } from "../lib/api";

/**
 * 连接设置：源站地址 + API Key，存本浏览器 localStorage。
 * 不做前端 HMAC——源站 auth.go 的优先级是 Bearer > 签名，前端只带 Key。
 */
export default function Credentials() {
	const initialBase = apiBase();
	const [base, setBase] = useState("");
	const [key, setKey] = useState("");
	const [saved, setSaved] = useState(false);
	const [invite, setInvite] = useState("");
	const [issued, setIssued] = useState<KeyIssue | null>(null);
	const [busy, setBusy] = useState(false);
	const [keyErr, setKeyErr] = useState("");

	// 用邀请码换一把 Key：deviceId 沿用打分那套稳定指纹，服务端按它做一设备一 Key。
	const register = () => {
		setBusy(true);
		setKeyErr("");
		api
			.registerKey(invite.trim())
			.then((res) => {
				setApiKey(res.key);
				setKey(res.key);
				setIssued(res);
				setInvite("");
			})
			.catch((e: unknown) => setKeyErr(describeError(e)))
			.finally(() => setBusy(false));
	};

	const revoke = () => {
		setBusy(true);
		setKeyErr("");
		api
			.revokeSelfKey()
			.then(() => {
				setApiKey("");
				setKey("");
				setIssued(null);
				setSaved(false);
			})
			.catch((e: unknown) => setKeyErr(describeError(e)))
			.finally(() => setBusy(false));
	};
	// 预渲染时读不到 localStorage（SSR 环境里没有），所以不能用它直接初始化 DOM 值：
	// Preact 水合不会把 HTML 里的旧值补成客户端状态，导致刷新后地址框显示空白、
	// 面板也不自动收起（真浏览器实测抓到）。挂载后再一次性同步。
	const [mounted, setMounted] = useState(false);
	const open = mounted ? !hasApiBase() : undefined;

	useEffect(() => {
		setBase(apiBase());
		setKey(apiKey());
		setMounted(true);
	}, []);

	// 存完必须让页面重取数据：各视图只在挂载时读过一次配置，不重载就会停在
	// “请先填地址”的旧状态（真浏览器点按实测抓到的缺陷）。
	const apply = () => {
		setApiBase(base);
		setApiKey(key);
		if (apiBase() !== initialBase) globalThis.location?.reload();
		else setSaved(true);
	};

	const clear = () => {
		const had = initialBase !== "" || apiKey() !== "";
		setApiBase("");
		setApiKey("");
		setBase("");
		setKey("");
		setSaved(false);
		if (had) globalThis.location?.reload();
	};

	return (
		<details class="creds" open={open}>
			<summary>连接设置</summary>
			<form
				onSubmit={(ev) => {
					ev.preventDefault();
					apply();
				}}
			>
				<label class="block">
					源站地址
					<input
						name="bench_api_base"
						autocomplete="off"
						value={base}
						onInput={(e) => {
							setSaved(false);
							setBase(e.currentTarget.value);
						}}
						placeholder="https://bench.example.com"
					/>
				</label>
				<label class="block">
					API Key
					<input
						name="bench_api_key"
						autocomplete="off"
						type="password"
						value={key}
						onInput={(e) => {
							setSaved(false);
							setKey(e.currentTarget.value);
						}}
						placeholder="留空则只能读"
					/>
				</label>
				<p class="row">
					<button type="submit">保存</button>
					<button type="button" class="ghost" onClick={clear}>
						清除
					</button>
				</p>
			</form>
			{saved && <p class="ok">已保存到本浏览器。</p>}

			<hr class="sep" />
			<p class="row">
				<label class="grow">
					邀请码
					<input
						name="invite_code"
						value={invite}
						onInput={(e) => {
							setInvite(e.currentTarget.value);
							setKeyErr("");
						}}
						placeholder="形如 ABCDE-FGHIJ"
					/>
				</label>
				<button type="button" disabled={busy || invite.trim() === ""} onClick={register}>
					{busy ? "处理中…" : "申请 Key"}
				</button>
				{apiKey() !== "" && (
					<button type="button" class="ghost" disabled={busy} onClick={revoke}>
						作废当前 Key
					</button>
				)}
			</p>

			{issued && (
				<div class="issued">
					<p class="warn">
						明文 Key <strong>只显示这一次</strong>，请立即保存：
					</p>
					<code class="issued__key">{issued.key}</code>
					<p class="hint">已自动填入并保存 · 句柄 {issued.ref}</p>
				</div>
			)}
			{keyErr && <p class="error">{keyErr}</p>}
			<p class="hint">
				⚠️ 这把 Key <strong>能写入</strong>，公共机器请留空。
			</p>
		</details>
	);
}
