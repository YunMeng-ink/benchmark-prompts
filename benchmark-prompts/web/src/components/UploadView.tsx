import { useState } from "preact/hooks";

import { api, describeError, type UploadData } from "../lib/api";

const MAX_LEN = 8192;

/** 上传页：进审核队列（202 + pending），不是即时公开（§3.7）。 */
export default function UploadView() {
	const [content, setContent] = useState("");
	const [tagsInput, setTagsInput] = useState("");
	const [busy, setBusy] = useState(false);
	const [err, setErr] = useState("");
	const [done, setDone] = useState<UploadData | null>(null);

	const tags = tagsInput
		.split(",")
		.map((s) => s.trim())
		.filter((s) => s !== "")
		.slice(0, 10);
	const len = [...content.trim()].length;
	const canSubmit = !busy && len > 0 && len <= MAX_LEN;

	const submit = () => {
		setBusy(true);
		setErr("");
		setDone(null);
		api
			.upload(content.trim(), tags)
			.then(setDone)
			.catch((x: unknown) => setErr(describeError(x)))
			.finally(() => setBusy(false));
	};

	return (
		<section>
			<h2>上传测试题</h2>
			<p class="hint">
				正文 {len} / {MAX_LEN} 字符 · 提交后进审核队列
			</p>
			<label class="block">
				提示词正文
				<textarea name="upload_content" rows={10} value={content} onInput={(e) => setContent(e.currentTarget.value)} />
			</label>
			<label class="block">
				标签
				<input
					name="upload_tags"
					value={tagsInput}
					onInput={(e) => setTagsInput(e.currentTarget.value)}
					placeholder="coding, reasoning"
				/>
			</label>
			<p class="row">
				<button type="button" disabled={!canSubmit} onClick={submit}>
					{busy ? "提交中…" : "提交"}
				</button>
				<span class="hint">需要 API Key</span>
			</p>
			{len > MAX_LEN && <p class="error">正文超出上限，请精简后再提交。</p>}
			{err && <p class="error">{err}</p>}
			{done && (
				<p class="ok">
					已收到 <code>{done.id}</code>，等待审核。
				</p>
			)}
		</section>
	);
}
