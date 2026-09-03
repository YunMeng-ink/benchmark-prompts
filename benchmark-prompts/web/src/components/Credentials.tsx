import { useEffect, useState } from "preact/hooks";

import { apiBase, apiKey, hasApiBase, setApiBase, setApiKey } from "../lib/api";

/**
 * 连接设置：源站地址 + API Key，存本浏览器 localStorage。
 * 不做前端 HMAC——源站 auth.go 的优先级是 Bearer > 签名，前端只带 Key。
 */
export default function Credentials() {
	const initialBase = apiBase();
	const [base, setBase] = useState("");
	const [key, setKey] = useState("");
	const [saved, setSaved] = useState(false);
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
					API Key（可选；只浏览不需要）
					<input
						name="bench_api_key"
						autocomplete="off"
						type="password"
						value={key}
						onInput={(e) => {
							setSaved(false);
							setKey(e.currentTarget.value);
						}}
						placeholder="留空则只能读；浏览/随机/详情都不需要 Key"
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
			<p class="hint">
				⚠️ 这里存的 Key <strong>能写入</strong>（打分、上传）。只在自己的设备上填；公共机器请留空，只读浏览。
			</p>
		</details>
	);
}
