import preact from "@astrojs/preact";
import { defineConfig } from "astro/config";

// 产物必须纯静态：整个 dist/ 上传 CDN，源站不承担前端流量（docs/frontend.md §1、§9）。
// 若部署在子路径下，改 `base` 即可，资源引用会自动跟随。
export default defineConfig({
	output: "static",
	integrations: [preact()],
	build: {
		// Astro 默认就给资产加内容 hash 文件名，满足 §4.2 的长缓存前提。
		inlineStylesheets: "auto",
	},
});
