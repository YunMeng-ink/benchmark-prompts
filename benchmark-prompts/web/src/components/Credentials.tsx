import { useState } from "preact/hooks";

import { apiBase, apiKey, hasApiBase, setApiBase, setApiKey } from "../lib/api";

/**
 * 连接设置：源站地址 + API Key，存本浏览器 localStorage。
 * 不做前端 HMAC——源站 auth.go 的优先级是 Bearer > 签名，前端只带 Key。
 */
export default function Credentials() {
	const [base, setBase] = useState(apiBase());
	const [key, setKey] = useState(apiKey());
	const [saved, setSaved] = useState(false);
	const open = !hasApiBase();

	return (
		<details class="creds" open={open}>
			<summary>连接设置</summary>
			<form
				onSubmit={(ev) => {
					ev.preventDefault();
					setApiBase(base);
					setApiKey(key);
					setSaved(true);
				}}
			>
				<label class="block">
					源站地址
					<input
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
					<button
						type="button"
						class="ghost"
						onClick={() => {
							setApiBase("");
							setApiKey("");
							setBase("");
							setKey("");
							setSaved(false);
						}}
					>
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
