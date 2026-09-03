/// <reference types="astro/client" />

interface Window {
	/** 由 public/runtime-config.js 注入；部署后可直接编辑该文件，无需重新构建。 */
	__BENCH_WEB__?: {
		apiBase?: string;
	};
}
