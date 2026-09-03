# 前端静态页规格

> 目标：一个**纯静态**页面（或极小 SPA），全量托管到亚太 CDN，**零回源**。源站带宽不承担任何前端资源。

## 1. 定位

- 面向极客用户的轻量浏览页：**浏览提示词、按标签筛选、随机一条、查看打分、上传提示词**。
- 纯前端直连 API（`/v1/*`），不需要源站渲染。
- 无框架优先：原生 HTML+CSS+ES Modules，产出可被 CDN 直接托管。

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
| 详情 | `GET /v1/prompts/{id}` | 完整正文 + 打分区 |
| 打分 | `POST /v1/scores` | 需 Key/签名；匿名则提示 |
| 上传 | `POST /v1/prompts` | 生成并复用 `clientId` |

## 4. 技术约束

1. **体积**：首屏 JS 控制在 ~20KB gzip 以内；不引入重型框架。
2. **压缩**：CDN 开 gzip/br；代码自带 `Cache-Control` 长缓存 + 内容 hash 文件名（如 `app.8c2e4f.js`）。
3. **鉴权**：API Key 存浏览器 `localStorage`，签名在客户端做（HMAC 需要 secret，注意 secret 放前端有泄露风险 → **上传/评分签名场景改用服务端校验 API Key 模式，不强求前端 HMAC**；前端写操作仅用 Bearer Key）。
4. **降级**：API 53x/429 时显示重试提示，用返回的 `retry_after`。
5. **可访问性**：语义化标签，键盘可达；文案中文为主。

## 5. 目录

```
web/
├── index.html          # hash 路由壳
├── assets/
│   ├── app.js          # 路由 + 渲染 + fetch
│   ├── api.js          # 薄封装 /v1 调用（信封解包、错误码）
│   ├── style.css
│   └── lib.js          # 通用小工具（hash、gzip 由浏览器处理）
└── README.md           # 构建/校验说明
```

## 6. API 薄封装约定（api.js）

```js
async function api(path, opts) {
  const r = await fetch(BASE + path, { headers: { 'Accept-Encoding': 'gzip' }, ...opts });
  const env = await r.json();
  if (!env.ok) throw { code: env.error.code, message: env.error.message, retry_after: env.error.retry_after };
  return env;   // { data, cursor, v }
}
```

## 7. 质量门禁

- `pnpm biome check web/`（若引入 JS/JSON 文件）。
- 手工验收：断网仅靠 CDN 缓存能打开首屏骨架；API 域与前端域分离（CORS 配置见下）。

## 8. CORS（前端与 API 可能跨域）

- 前端在 CDN 域，API 在源站域 → 源站 `Access-Control-Allow-Origin` 需放行前端域。
- 只读端点放行 `*`；写入端点用 `Access-Control-Allow-Credentials` + 精确 Origin，仅放行前端域与本地插件域。

## 9. 发布

- 静态文件上传 CDN（OSS/COS + CDN 加速，或纯 CDN 存储）。
- 每次构建生成内容 hash 文件名 + `index.html` 引用最新，实现长缓存 + 秒级发布。