import { useEffect, useState } from "preact/hooks";

import { api, describeError, noteRecent, type Prompt } from "../lib/api";
import ScorePanel from "./ScorePanel";

/** 详情页：完整正文 + 打分区（§3.3 + §3.8）。 */
export default function DetailView({ id }: { id: string }) {
	const [prompt, setPrompt] = useState<Prompt | null>(null);
	const [err, setErr] = useState("");
	const [copied, setCopied] = useState(false);

	useEffect(() => {
		let alive = true;
		setPrompt(null);
		setErr("");
		setCopied(false);
		api
			.get(id)
			.then((e) => {
				if (!alive) return;
				const p = e.data;
				setPrompt(p);
				if (p) noteRecent(p.id);
			})
			.catch((x: unknown) => {
				if (alive) setErr(describeError(x));
			});
		return () => {
			alive = false;
		};
	}, [id]);

	const copy = () => {
		if (!prompt) return;
		globalThis.navigator?.clipboard
			?.writeText(prompt.p)
			.then(() => setCopied(true))
			.catch(() => setCopied(false));
	};

	return (
		<section>
			{err && (
				<p class="error">
					{err} <a href="#/">返回列表</a>
				</p>
			)}
			{!prompt && !err && <p class="hint">加载中…</p>}
			{prompt && (
				<article>
					<header class="detail__head">
						<h2>{prompt.id}</h2>
						<span class="badges">
							{prompt.t.map((t) => (
								<code key={t}>{t}</code>
							))}
							<code>v{prompt.v}</code>
							<code>{prompt.h}</code>
						</span>
					</header>
					<pre class="prompt">{prompt.p}</pre>
					<p class="row">
						<button type="button" onClick={copy}>
							{copied ? "已复制" : "复制正文"}
						</button>
						<span class="hint">复制后原样粘给被测模型</span>
					</p>
					<ScorePanel id={prompt.id} />
				</article>
			)}
		</section>
	);
}
