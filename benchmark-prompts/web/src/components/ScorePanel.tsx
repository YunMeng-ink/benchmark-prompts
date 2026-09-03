import { useEffect, useState } from "preact/hooks";

import { api, describeError, type ScoreStats } from "../lib/api";

/**
 * 打分区：先读 GET /v1/prompts/{id}/score（§3.8），再允许提交 POST /v1/scores。
 * 只读统计的存在意义就是「没打过分数也看得到别人打过多少」。
 */
export default function ScorePanel({ id }: { id: string }) {
	const [stats, setStats] = useState<ScoreStats | null>(null);
	const [busy, setBusy] = useState<number | null>(null);
	const [err, setErr] = useState<string>("");
	const [note, setNote] = useState<string>("");

	useEffect(() => {
		let alive = true;
		setStats(null);
		setErr("");
		setNote("");
		api
			.stats(id)
			.then((e) => {
				if (alive) setStats(e.data);
			})
			.catch((e: unknown) => {
				if (alive) setErr(describeError(e));
			});
		return () => {
			alive = false;
		};
	}, [id]);

	const submit = (value: number) => {
		setBusy(value);
		setErr("");
		api
			.score(id, value)
			.then((d) => {
				setStats({ id, avg: d.avg, count: d.count });
				setNote(`已记录 ${value} 分（同一浏览器重复打分会覆盖旧分，不重复计数）`);
			})
			.catch((e: unknown) => setErr(describeError(e)))
			.finally(() => setBusy(null));
	};

	return (
		<section class="score">
			<h3>打分</h3>
			{stats && (
				<p class="score__now">
					当前 <strong>{stats.avg.toFixed(2)}</strong> 分 · <strong>{stats.count}</strong> 人
					{stats.count === 0 ? "（还没有人打分）" : ""}
				</p>
			)}
			<div class="score__btns">
				{[1, 2, 3, 4, 5].map((n) => (
					<button type="button" onClick={() => submit(n)} disabled={busy !== null} aria-label={`打 ${n} 分`}>
						{busy === n ? "…" : n}
					</button>
				))}
			</div>
			{note && <p class="hint">{note}</p>}
			{err && <p class="error">{err}</p>}
		</section>
	);
}
