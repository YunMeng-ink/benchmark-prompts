import { useState } from "preact/hooks";

import { api, describeError, noteRecent, type Prompt, recentIds } from "../lib/api";
import ScorePanel from "./ScorePanel";

/** 随机页：抽一条（可选排除最近看过的）。正文直接返回，不用再请求 get（§3.4）。 */
export default function RandomView() {
	const [tagInput, setTagInput] = useState("");
	const [exclude, setExclude] = useState(true);
	const [prompt, setPrompt] = useState<Prompt | null>(null);
	const [loading, setLoading] = useState(false);
	const [err, setErr] = useState("");

	const draw = () => {
		setLoading(true);
		setErr("");
		const tag = tagInput.trim();
		api
			.random({ tag: tag || undefined, exclude: exclude ? recentIds(20).join(",") : undefined })
			.then((e) => {
				const p = e.data;
				setPrompt(p);
				if (p) noteRecent(p.id);
			})
			.catch((x: unknown) => setErr(describeError(x)))
			.finally(() => setLoading(false));
	};

	return (
		<section>
			<header class="row">
				<form
					onSubmit={(ev) => {
						ev.preventDefault();
						draw();
					}}
				>
					<label>
						标签
						<input
							name="random_tag"
							value={tagInput}
							onInput={(e) => setTagInput(e.currentTarget.value)}
							placeholder="留空为全库"
						/>
					</label>
					<label class="check">
						<input
							name="exclude_recent"
							type="checkbox"
							checked={exclude}
							onChange={(e) => setExclude(e.currentTarget.checked)}
						/>
						排除最近看过的
					</label>
					<button type="submit" disabled={loading}>
						{loading ? "抽取中…" : prompt ? "再来一条" : "抽一条"}
					</button>
				</form>
			</header>

			{err && <p class="error">{err}</p>}
			{!prompt && !err && !loading && (
				<p class="hint">
					库里没有可用条目时会报 <code>not_found</code>，这不是故障——换标签或等审核通过。
				</p>
			)}
			{prompt && (
				<article>
					<header class="detail__head">
						<h2>
							<a href={`#/detail/${encodeURIComponent(prompt.id)}`}>{prompt.id}</a>
						</h2>
						<span class="badges">
							{prompt.t.map((t) => (
								<code key={t}>{t}</code>
							))}
							<code>v{prompt.v}</code>
						</span>
					</header>
					<pre class="prompt">{prompt.p}</pre>
					<ScorePanel id={prompt.id} />
				</article>
			)}
		</section>
	);
}
