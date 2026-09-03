// 部署期可编辑的运行时配置（放在 public/ 下，构建时原样复制到 dist 根目录）。
// 改这个文件不需要重新构建站点：CDN 上覆盖它即可换源站地址。
//
// apiBase 填源站根地址（路径里的 /v1 由前端拼）：
//   window.__BENCH_WEB__ = { apiBase: "https://bench.example.com" };
// 粘成 https://bench.example.com/v1 也能用：两端都会剔掉尾部的 /v1。
//
// 留空时站点会显示「连接设置」面板，让用户自己填（存在本浏览器 localStorage）。
// 注意：源站必须把本站域名加进 cors.allowed_origins，否则浏览器会拒掉跨域响应。
window.__BENCH_WEB__ = {
	apiBase: "",
};
