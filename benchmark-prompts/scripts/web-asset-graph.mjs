/**
 * 前端产物资产图核对：从 index.html 出发，沿 import 逐层解析，
 * 断言每个被引用的文件都真实存在于 dist/。
 *
 * 为什么不能只看 src/href 属性：Astro 的岛把入口写在 JSON 字符串里，而真正的
 * 框架代码（preact/hooks/signals）是 chunk 之间的相对 import —— 少一个文件，
 * 页面在 CDN 上会白屏，但"属性核对"完全看不出来。
 *
 * 用法：node scripts/web-asset-graph.mjs [dist 目录]
 * 输出：每行 `OK …` / `ERR …` / `MISS …`，退出码非 0 表示有缺失。
 */
import { existsSync, readFileSync } from "node:fs";
import path from "node:path";

const root = path.resolve(process.argv[2] ?? "web/dist");
const index = path.join(root, "index.html");

if (!existsSync(index)) {
	console.log("ERR 产物不存在：index.html");
	process.exit(1);
}

const html = readFileSync(index, "utf8");

// src/href 属性 + 岛声明里以 JSON 字符串出现的绝对路径。
const refs = new Set();
for (const m of html.matchAll(/(?:src|href)="(\/[^"]*)"/g)) refs.add(m[1]);
for (const m of html.matchAll(/"(\/[\w./-]+\.(?:js|css|svg|json|html))"/g)) refs.add(m[1]);

const seen = new Set();
const missing = [];
const queue = [...refs];
let jsFiles = 0;

const absolutize = (spec, fromFile) => {
	if (spec.startsWith("/")) return spec;
	if (spec.startsWith("./") || spec.startsWith("../")) {
		const abs = path.resolve(path.dirname(path.join(root, fromFile)), spec);
		return `/${path.relative(root, abs).replaceAll("\\", "/")}`;
	}
	return null; // 裸导入（npm 包名）由打包器内联，不该出现在产物里
};

while (queue.length > 0) {
	const url = queue.shift();
	if (!url || url.startsWith("data:") || seen.has(url)) continue;
	seen.add(url);

	const file = path.join(root, url);
	if (!existsSync(file)) {
		missing.push(url);
		continue;
	}
	if (!file.endsWith(".js")) continue;

	jsFiles += 1;
	const code = readFileSync(file, "utf8");
	const specs = new Set();
	for (const m of code.matchAll(/(?:^|[^A-Za-z])import[^"']*["'](\.\/[^"']+|\.\.\/[^"']+|\/[^"']+)["']/g)) specs.add(m[1]);
	for (const m of code.matchAll(/import\s*\(\s*["']([^"']+)["']\s*\)/g)) specs.add(m[1]);
	for (const m of code.matchAll(/new URL\(\s*["']([^"']+)["']/g)) specs.add(m[1]);
	for (const s of specs) {
		const a = absolutize(s, url);
		if (a) queue.push(a);
	}
}

if (missing.length > 0) {
	for (const m of missing) console.log(`MISS ${m}`);
	console.log(`ERR 有 ${missing.length} 个引用在产物里不存在`);
	process.exit(1);
}
console.log(`OK 资产图完整：HTML 引用 ${refs.size} 项，沿 import 共核对 ${seen.size} 个文件（含 ${jsFiles} 个 JS chunk）`);
