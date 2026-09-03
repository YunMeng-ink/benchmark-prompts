import { useEffect, useState } from "preact/hooks";

import { api, describeError, hasApiBase, type PromptSummary } from "../lib/api";
import { go } from "../lib/router";

const PAGE = 20;

/** 列表页：分页 + 标签筛选。游标原样透传，不解析（§3.2）。 */
export default function PromptList() {
	const [items, setItems] = useState<PromptSummary[]>([]);
	const [cursor, setCursor] = useState<string | null>(null);
	const [hasMore, setHasMore] = useState(false);
	const [tagInput, setTagInput] = useState("");
	const [tag, setTag] = useState("");
	const [total, setTotal] = useState<number | null>(null);
	const [loading, setLoading] = useState(false);
	const [err, setErr] = useState("");

	const load = (nextCursor: string | null, useTag: string) => {
		setLoading(true);
		setErr("");
		api
			.list({ cursor: nextCursor ?? undefined, limit: PAGE, tag: useTag || undefined })
			.then((e) => {
				const got = e.data?.items ?? [];
				setItems((prev) => (nextCursor ? prev.concat(got) : got));
				setCursor(e.cursor);
				setHasMore(e.data?.has_more ?? false);
			})
			.catch((x: unknown) => setErr(describeError(x)))
			.finally(() => setLoading(false));
	};

	const firstPage = (useTag: string) => {
		setTag(useTag);
		setItems([]);
		load(null, useTag);
	};

	// 打开页面就取第一页。列表为空时也是正常空集（不像 random 会报 not_found），
	// 而没配源站地址时不白打一个请求，直接把用户指向「连接设置」。
	useEffect(() => {
		if (hasApiBase()) firstPage("");
	}, []);

	const open = (id: string) => {
		go(`/detail/${encodeURIComponent(id)}`);
	};

	return (
		<section>
			<header class="row">
				<form
					onSubmit={(ev) => {
						ev.preventDefault();
						firstPage(tagInput.trim());
					}}
				>
					<label>
						标签
						<input value={tagInput} onInput={(e) => setTagInput(e.currentTarget.value)} placeholder="如 coding" />
					</label>
					<button type="submit">筛选</button>
					{tag !== "" && (
						<button
							type="button"
							onClick={() => {
								setTagInput("");
								firstPage("");
							}}
						>
							清除
						</button>
					)}
				</form>
				<button
					type="button"
					class="ghost"
					onClick={() =>
						api
							.meta()
							.then((e) => setTotal(e.data?.total ?? null))
							.catch((x: unknown) => setErr(describeError(x)))
					}
				>
					查库总量
				</button>
			</header>

			{total !== null && <p class="hint">已审核提示词共 {total} 条</p>}
			{tag !== "" && (
				<p class="hint">
					当前按标签 <code>{tag}</code> 过滤
				</p>
			)}

			{items.length === 0 && !loading && !err && (
				<p class="hint">
					{hasApiBase() ? (
						<>
							库里还没有已审核的提示词。
							<button type="button" class="link" onClick={() => firstPage("")}>
								重试
							</button>
						</>
					) : (
						"先在页面底部「连接设置」里填源站地址。"
					)}
				</p>
			)}

			<ul class="cards">
				{items.map((it) => (
					<li key={it.id}>
						<button type="button" class="card" onClick={() => open(it.id)}>
							<span class="card__id">{it.id}</span>
							<span class="card__tags">{it.t.length > 0 ? it.t.join(" · ") : "无标签"}</span>
							<span class="card__meta">
								v{it.v} · {it.h}
							</span>
						</button>
					</li>
				))}
			</ul>

			{loading && <p class="hint">加载中…</p>}
			{err && <p class="error">{err}</p>}
			{hasMore && (
				<button type="button" disabled={loading} onClick={() => load(cursor, tag)}>
					加载更多
				</button>
			)}
			{!hasMore && items.length > 0 && <p class="hint">已到末尾（正文要点进详情才拉取，列表只给摘要）</p>}
		</section>
	);
}
