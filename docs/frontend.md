# 前端静态页规格

> 目标：一个**纯静态**站点，全量托管到亚太 CDN，**零回源**。源站带宽不承担任何前端资源。

## 1. 定位

- 面向极客用户的轻量浏览页：**浏览提示词、按标签筛选、随机一条、查看打分、上传提示词**。
- 纯前端直连 API（`/v1/*`），不需要源站渲染。
- **实现：Astro（`output: 'static'`）+ Preact 岛**。
  选型理由：需要 hash 路由下的局部交互态（列表/详情/表单），岛式架构
  可以只在交互区域注水运行时，其余 HTML 预渲染；同时 Astro 自带
  内容 hash 文件名与 CSS 打包，直接满足 §4.2 的长缓存要求。
  选 Preact 而非 React：API 兼容、岛运行时小一个数量级，且不需要额外配置。
- **历史注记**：本节原先写的是“无框架优先：原生 HTML+CSS+ES Modules”。
  2026-09-03 改为 Astro + 岛式 UI，同时取消 §4.1 的首屏 JS 硬预算（见下）。
  两者都是用户决策，不是实现时的顺手偏离。

## 2. 页面与路由

| 路由（hash） | 内容 |
|--------------|------|
| `#/` | 列表页：分页 + 标签筛选 |
| `#/random` | 随机页：点按钮抽一条 |
| `#/detail/{id}` | 单条详情 + 打分 |
| `#/upload` | 上传（进审核队列） |

> 用 hash 路由（无需源站重写规则，CDN 零配置）。

## 3. 关键交互与 API 映射

| 交互 | 调用 | 注意 |
|------|------|------|
| 首页列表 | `GET /v1/prompts?cursor=&limit=20&tag=` | 渲染 PromptSummary，正文点进去才拉 |
| 加载更多 | 用返回的 `cursor` 翻页 | 不一次性全量拉 |
| 随机 | `GET /v1/prompts/random?tag=&exclude=` | `exclude` 传最近 N 条避免重复 |
| 详情 | `GET /v1/prompts/{id}` | 完整正文 |
| 查看打分 | `GET /v1/prompts/{id}/score` | 匿名可读；无人打分为 `avg:0,count:0` |
| 打分 | `POST /v1/scores` | 需 Key/签名；匿名则提示 |
| 上传 | `POST /v1/prompts` | 生成并复用 `clientId` |

## 4. 技术约束

1. **体积**：**不设硬预算**（2026-09-03 决定）。但构建必须**报告**每个产物的
   gzip 体积，作为观测指标归档；不得无声增长。真正的硬约束是下面两条。
   当前基线（`make web-build` 输出，Astro 7.2.10 + Preact 岛，45 项冒烟同次实测）：
   首屏必经 **19.7 KB gzip**（其中 JS 15.1 KB），产物合计 20.3 KB gzip。
   顺便说明：当初的 20 KB 预算并非被源站带宽逼出来的——前端根本不该回源，
   它约束的是慢链路上的用户体验。
2. **缓存**：产物必须带内容 hash 文件名 + `Cache-Control` 长缓存，CDN 开 gzip/br。
   资产**不得回源**（源站不服务 `web/*`）。
3. **鉴权**：写入用 `Authorization: Bearer <key>`（源站 `auth.go` 的优先级是
   Bearer > HMAC 签名，所以前端**不需要也不应该**持 secret）。Key 存 `localStorage`，
   页面上必须说明“浏览器里存的是可写的凭据”。
4. **降级**：API 53x/429 时显示重试提示，用返回的 `retry_after`。
5. **可访问性**：语义化标签，键盘可达；文案中文为主。
6. **API 地址**：`apiBase` 一律是**源站根**（如 `https://bench.example.com`），路径里的
   `/v1` 由客户端拼——与 Go SDK 的 `NormalizeEndpoint` 同构，两处规则必须一致。
   容错：粘成 `.../v1` 也会被剔掉（`docs/api.md` 的 Base URL 写法就是带 `/v1` 的）。
   取值优先级：`localStorage.bench_api_base` > `public/runtime-config.js`（后者部署时
   可直接改文件，无需重新构建）。

## 5. 目录

```
web/
├── astro.config.mjs          # output: 'static' + preact 集成 + 内容 hash
├── package.json              # 依赖锺定（pnpm）
├── tsconfig.json
├── index.html                # （无手写入口，Astro 接管）
├── src/
│   ├── pages/index.astro     # 预渲染壳 + 样式 + 挂载岛
│   ├── components/*.tsx      # 岛：列表 / 随机 / 详情+打分 / 上传 / 凭据区
│   ├── lib/api.ts            # /v1 薄封装（信封解包、错误码、Bearer 注入）
│   ├── lib/router.ts         # hash 路由
│   └── styles/global.css
└── README.md                 # 构建/部署/校验说明
```

> 旧版本节列的是 vanilla 五文件结构（`assets/app.js` 等），已随 §1 选型变更作废。

## 6. API 薄封装约定（`src/lib/api.ts`）

```ts
async function api<T>(path: string, opts: RequestInit = {}) {
  const r = await fetch(base() + path, {
    ...opts,
    headers: { Accept: 'application/json', ...authHeader(), ...(opts.headers || {}) },
  });
  const env = await r.json();
  if (!env.ok) throw { code: env.error.code, message: env.error.message, retry_after: env.error.retry_after };
  return env; // { data, cursor, v }
}
```

注意两点实现事实：
- **不要手写 `Accept-Encoding: gzip`**。浏览器自动带且自动解压，手动设这个头
  反而会让响应变成需自己解压的二进制（与 Go 客户端 §2 的同一条约束）。
- `base()` = `localStorage.bench_api_base` 优先，否则用 `runtime-config.js`；两者都过
  `normalizeBase()`，拼出的 URL 形如 `https://host/v1/prompts`。

## 7. 门禁

```bash
make web-install   # pnpm install（锁定 pnpm-lock.yaml）
make web-build     # astro build → web/dist/ + 打印每个产物的 gzip 体积
make web-preview   # 本地预览，配合 scripts/smoke-web.sh 做真源站验证
make web-check     # biome + astro check（前端 0 errors）+ 体积报告
make smoke-web     # 45 项：真源站 + 真产物 + 直接跑浏览器那份 api.ts
```

手工验收：断网仅靠 CDN 缓存能打开首屏骨架；API 域与前端域分离（CORS 配置见下）。

## 8. CORS（前端与 API 可能跨域）

- 前端在 CDN 域，API 在源站域 → 源站 `Access-Control-Allow-Origin` 需放行前端域。
- 只读端点放行 `*`；写入端点用 `Access-Control-Allow-Credentials` + 精确 Origin，仅放行前端域与本地插件域。

## 9. 发布

步骤、两组 `Cache-Control`、CORS 白名单与自检项的**唯一权威是
`deployment.md` §7**，本节不重复一份。要点只有两条：

- Astro 默认产出带内容 hash 的文件名，所以长缓存与秒级发布可以兼得。
- `index.html` 与 `runtime-config.js` 不能永久缓存，否则新版与改地址都不生效。

## 10. 实现状态

| 项 | 状态 | 证据 |
|---|---|---|
| Astro 工程与四个 hash 路由 | ✅ M5 | `web/`；`astro check` 0 errors，biome 0 error |
| 真源站本地验证（列表/随机/详情/统计/打分/上传/错误分支） | ✅ M5 | `make smoke-web` 45/45，含直接 import `web/src/lib/api.ts` |
| 产物纯静态与资产图完整 | ✅ M5 | `scripts/web-asset-graph.mjs` 沿 import 核对 5 个文件 |
| CORS 白名单 / `Vary: Origin` / 陌生源拒绝 / gzip | ✅ M5 | 冒烟中以带 Origin 的 curl 断言 |
| 真浏览器点按验证 | 🔶 未做 | 本机 chrome-devtool 起不了 Chrome；见 `web/README.md` 已知限制 |
| 上 CDN | ⬜ M6 | 按 `deployment.md` §7 执行 |