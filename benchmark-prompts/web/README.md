# web/ —— 前端站点（Astro 纯静态 + Preact 岛）

浏览提示词、按标签筛选、随机一条、查看与提交打分、上传新题。
**零业务逻辑**：所有数据都直连源站 `/v1`，本目录只负责取与展示。
规格见 [`docs/frontend.md`](../../docs/frontend.md)。

## 常用命令（都在仓库根的 Makefile 里）

```bash
make web-install   # pnpm install（依赖版本见 pnpm-lock.yaml）
make web-build     # 构建 → web/dist/，并打印产物体积报告
make web-check     # biome + astro check（类型与静态检查）
make web-preview   # 本地预览已构建产物，默认 http://localhost:4321
make smoke-web     # 端到端冒烟：真源站 + 真产物 + 真浏览器那份 api.ts
```

## 部署

**步骤与缓存头/CORS 的权威规定在
[`docs/deployment.md` §7](../../docs/deployment.md)**，此处不重复一份
（同一事实抄两处必然漂移，见 `docs/README.md` §0）。

一句话概括：`make web-build` 之后把 `web/dist/` 整目录上 CDN，
**前端资产一个字节都不回源**，源站带宽只服务 API JSON。

### 换源站地址（不必重新构建）

编辑 `web/dist/runtime-config.js`（部署后可直接覆盖这个文件）：

```js
window.__BENCH_WEB__ = { apiBase: "https://bench.example.com" };
```

`apiBase` 是**源站根地址**。填成 `https://bench.example.com/v1` 也能用——
`normalizeBase()` 会剔掉尾部的 `/v1`，与 Go SDK 的 `NormalizeEndpoint` 同一套规则。
浏览器里还可以在页面底部「连接设置」临时改（存 `localStorage`，优先级高于上面这个文件）。

### 源站要做的事

`cors.allowed_origins` 填前端域名（精确值——前端能带 Key 写入，`*` 不够安全），
详见 `docs/deployment.md` §7 第 3 步。

## 结构

```
astro.config.mjs        output: 'static' + preact 集成
pnpm-workspace.yaml     allowBuilds: esbuild（pnpm 12 需要显式批准依赖构建脚本）
public/runtime-config.js  部署期可改的源站地址
src/pages/index.astro   预渲染壳（首屏 HTML 不含 JS 也有内容）
src/components/         岛：App / PromptList / RandomView / DetailView / UploadView /
                        ScorePanel / Credentials
src/lib/api.ts          /v1 薄封装：信封解包、错误码→中文提示、Bearer 注入、地址归一
src/lib/router.ts       hash 路由（#/ #/random #/detail/{id} #/upload）
src/styles/global.css   样式（构建时由 Astro 内联进 HTML）
```

## 安全边界

- **前端不持有 HMAC secret**。写入用 `Authorization: Bearer <key>`；源站 `auth.go`
  的优先级就是 Bearer > 签名。secret 进浏览器等于交给所有能读到你脚本的人。
- Key 存在本浏览器 `localStorage`，**它是可写凭据**：公共机器上留空，只读浏览即可
  （浏览/随机/详情/看分数都不需要 Key）。
- 渲染一律用文本节点（Preact 自动转义），提示词正文走 `<pre>`，不拼 `innerHTML`。

## 体积

不设硬预算（`docs/frontend.md` §4.1），但每次构建必须报告，防止无声增长：

```
首屏必经（index.html + 全部 JS，gzip）  ≈ 19.7 KB
其中 JS（Preact 岛运行时 + 应用代码）    ≈ 15.1 KB
```

`make web-build` / `make smoke-web` 每次都会重新打印真实数字。

## 已知限制

- 交互验证止步于「浏览器那份 `api.ts` 真打了源站 + CORS/压缩/错误分支全过」这一层，
  没有真浏览器点按：本机 chrome-devtool MCP 起不了 Chrome。`scripts/smoke-web.sh`
  里那 45 项覆盖的是数据链路与渲染产物，不是点击事件。
- `runtime-config.js` 没有内容 hash，改了要靠 CDN 刷新或 `no-cache` 生效。
- `.astro` 文件 biome 不支持，只有 `ts`/`tsx` 进 lint 门禁。
