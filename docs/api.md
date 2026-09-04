# Benchmark 提示词平台 —— REST API 契约 v1

> 状态：**已冻结（v1）并已实现**。本契约是源站、CLI/SDK、DSH/Pi 插件的并行开发依据。
> 变更遵循“向后兼容”原则：只增字段、不改字段语义、不删字段。

---

## 1. 基础约定

| 项 | 值 |
|----|----|
| Base URL | `https://<host>/v1` |
| 协议 | HTTPS（生产强制） |
| 编码 | `application/json; charset=utf-8` |
| 压缩 | 强制 `Content-Encoding: gzip`（br 可选） |
| API 版本 | 通过 Base 路径 `/v1` 体现，另在信封携带 `v` 字段 |

### 1.1 响应信封

所有成功与业务失败均返回统一信封，**HTTP 状态码语义化**（见错误码）：

```jsonc
{
  "ok": true,            // 布尔，业务是否成功
  "data": { },           // 成功时的载荷（可为 null）
  "error": null,         // 失败时的错误对象 { code, message }（成功时为 null）
  "cursor": null,        // 分页游标，仅 list 使用
  "v": 1                 // 协议版本号
}
```

成功示例（无数据）：`{"ok":true,"data":null,"error":null,"cursor":null,"v":1}`

### 1.2 错误码

遵循“HTTP 状态码 = 错误类别”，`error.code` 为稳定机器码（程序据此分支）。

| HTTP | error.code | 含义 |
|------|-----------|------|
| 400 | `bad_request` | 请求参数缺失/非法 |
| 401 | `unauthorized` | 缺少或非法的鉴权信息 |
| 403 | `forbidden` | 无权限（如未审核用户上传）；或触发限流 |
| 404 | `not_found` | 资源不存在（含单条提示词不存在） |
| 409 | `conflict` | 资源状态冲突（保留码，当前打分/上传均为幂等合并，不使用） |
| 413 | `too_large` | 请求体超限 |
| 422 | `validation_failed` | 字段校验失败（附字段名） |
| 429 | `rate_limited` | 频率/配额超限 |
| 500 | `internal` | 服务器内部错误 |
| 503 | `unavailable` | 过载/维护中 |

错误对象：

```jsonc
{
  "code": "rate_limited",       // 稳定机器码
  "message": "请求过于频繁",      // 人类可读（可能含动态信息，勿用于分支）
  "retry_after": 30              // 可选，建议等待秒数
}
```

### 1.3 鉴权

- **只读端点**（meta/list/get/random/delta）：允许匿名访问（限流较宽）。
- **写入端点**（scores/post prompts）：需鉴权。

两种鉴权方式（任一即可）：

1. **API Key**：`Authorization: Bearer <api_key>`
2. **HMAC 签名**（防中间人重放，SDK/CLI 推荐）：
   ```
   X-Api-Key: <key>
   X-Timestamp: <unix_seconds>
   X-Signature: <hex(HMAC-SHA256(secret, method + "\n" + path + "\n" + timestamp + "\n" + body_sha256))>
   ```
   - 服务端校验 `|now - timestamp| ≤ 300s`，防重放。

> **浏览器前端例外**：`secret` 无法安全下发到浏览器，故前端写操作仅使用 **API Key（方式 1）**，不使用 HMAC。HMAC 仅用于可分发的 CLI/SDK。

---

## 2. 通用数据类型

### 2.1 Prompt（提示词）

字段用短名以减少流量；`p` 为正文，其余为元数据。

| 字段 | 类型 | 说明 |
|------|------|------|
| `id` | string | 唯一 ID，形如 `p_9f3a2c` |
| `p` | string | 提示词正文（完整内容） |
| `t` | string[] | 标签数组，如 `["coding","reasoning"]` |
| `v` | int | 版本号，单调递增，`/delta` 增量同步的依据 |
| `s` | string | 状态：`pending`/`approved`/`rejected`/`featured`（只读端点仅暴露 approved/featured） |
| `h` | string | `content_hash` 前 8 位，供客户端做轻量去重/ETag |

示例：

```jsonc
{
  "id": "p_9f3a2c",
  "p": "你是一名资深算法工程师……",
  "t": ["coding", "reasoning"],
  "v": 3,
  "s": "approved",
  "h": "8c2e4f1a"
}
```

### 2.2 PromptSummary（列表/目录条目，不含正文）

列表与 meta 返回**不含正文**的精简条目，正文仅在 `get`/`random`/`delta` 返回，以节省带宽。

| 字段 | 类型 | 说明 |
|------|------|------|
| `id` | string | 唯一 ID |
| `t` | string[] | 标签 |
| `v` | int | 版本号 |
| `h` | string | content_hash 前 8 位 |

---

## 3. 端点明细

### 3.1 `GET /v1/meta` —— 版本/目录元信息

客户端启动与 `sync` 的第一步。

**响应 `data`：**

| 字段 | 类型 | 说明 |
|------|------|------|
| `total` | int | 已审核提示词总数 |
| `catalog_hash` | string | 全目录 hash，客户端据此判断是否走 `delta` 增量 |
| `schema_version` | int | schema 版本，不兼容时客户端可提示升级 |
| `server_time` | int | Unix 秒，供客户端校准签名时钟 |

```jsonc
// 200
{
  "ok": true,
  "data": {
    "total": 10240,
    "catalog_hash": "9f1d3c8e2a7b...",
    "schema_version": 1,
    "server_time": 1756791234
  },
  "error": null, "cursor": null, "v": 1
}
```

### 3.2 `GET /v1/prompts` —— 分页列表

**Query：**

| 参数 | 类型 | 必填 | 说明 |
|------|------|------|------|
| `cursor` | string | 否 | 上一页返回的游标（首页省略） |
| `limit` | int | 否 | 每页条数，默认 20，范围 1–100 |
| `tag` | string | 否 | 按标签过滤（精确匹配单个标签） |

**响应 `data`：**

```jsonc
{
  "ok": true,
  "data": {
    "items": [
      { "id": "p_9f3a2c", "t": ["coding"], "v": 3, "h": "8c2e4f1a" }
      // … PromptSummary（不含正文）
    ],
    "has_more": true
  },
  "error": null,
  "cursor": "eyJvZmZzZXQiOjIwfQ",   // 下一页游标，has_more=false 时为 null
  "v": 1
}
```

**分页规则：**
- 游标为不透明字符串（服务端编码 offset），客户端原样透传，**不解析其内容**。
- `cursor` 仅用于稳定分页，不做实时排序快照保证。

### 3.3 `GET /v1/prompts/{id}` —— 单条（一键测试）

**Path：** `id` = Prompt.id

**响应：**

```jsonc
// 200
{
  "ok": true,
  "data": {
    "id": "p_9f3a2c", "p": "……", "t": ["coding"], "v": 3,
    "s": "approved", "h": "8c2e4f1a"
  },
  "error": null, "cursor": null, "v": 1
}
```

```jsonc
// 404
{ "ok": false, "data": null, "error": { "code": "not_found", "message": "提示词不存在" }, "cursor": null, "v": 1 }
```

### 3.4 `GET /v1/prompts/random` —— 随机（随机测试）

**Query（均可选）：**

| 参数 | 类型 | 说明 |
|------|------|------|
| `tag` | string | 按标签随机 |
| `exclude` | string | 逗号分隔的 ID 列表，避免重复抽到（最多 100 个） |

**响应：** 完整 Prompt（同 3.3 的 `data`），直接返回正文。

### 3.5 `GET /v1/prompts/delta` —— 增量变更集

见第 4 节协议。

### 3.6 `POST /v1/scores` —— 提交评分（需鉴权）

**请求体：**

```jsonc
{
  "id": "p_9f3a2c",
  "value": 5,                    // 1–5 整数
  "deviceId": "a1b2c3..."        // 客户端生成的稳定设备指纹，用于去重
}
```

**响应：**

```jsonc
// 200
{
  "ok": true,
  "data": { "avg": 4.62, "count": 128 },
  "error": null, "cursor": null, "v": 1
}
```

**规则：**
- 同一 `deviceId` 对同一 `id` 重复评分 → 幂等，返回当前均值（不重复累加），或按实现返回 `409 conflict`（本项目约定**幂等合并**，重复提交覆盖旧分值）。
- 超限流返回 `429`。

### 3.7 `POST /v1/prompts` —— 上传（需鉴权，进审核队列）

**请求体：**

```jsonc
{
  "p": "提示词正文……",
  "t": ["coding"],               // 可选，最多 10 个标签
  "clientId": "uuid-v4"           // 幂等键，客户端生成，重复提交去重
}
```

**响应：**

```jsonc
// 202（进入审核队列，非即时可用）
{
  "ok": true,
  "data": { "id": "p_9f3a2c", "s": "pending" },
  "error": null, "cursor": null, "v": 1
}
```

**规则：**
- 正文长度：1–8192 字符，超限 `422`。
- 重复 `clientId` → 幂等返回原 `id`，不重复入库。
- 上传状态为 `pending`，审核通过后才进入公开只读端点。

---

### 3.8 `GET /v1/prompts/{id}/score` —— 读取打分统计（只读）

**Path：** `id` = Prompt.id

**鉴权：** 不需要（与其他只读端点一致）。

**响应：**

```jsonc
// 200（无人打分也是 200 + 零值，不是 404）
{
  "ok": true,
  "data": { "id": "p_9f3a2c", "avg": 4.62, "count": 128 },
  "error": null, "cursor": null, "v": 1
}
```

| 字段 | 类型 | 说明 |
|------|------|------|
| `id` | string | 回显请求的提示词 ID |
| `avg` | float | 平均分，保留两位；无人打分为 `0` |
| `count` | int | 打分人数（按 `deviceId` 去重） |

**规则：**
- 未公开（`pending`/`rejected`）与不存在同样返 `404 not_found`，不泄露存在性。
- `Cache-Control: no-store`：分数随每次提交变化，且聚合查询极轻，不值得缓存。
- 限流计入 `get` 端点的桶（成本同级别）。

### 3.9 `POST /v1/keys` —— 用邀请码自助注册 Key

**鉴权：** 不需要（注册动作本身就是发凭据）。限流见 §6 的 `keys` 行。

**请求体：**

```jsonc
{
  "inviteCode": "CQV7Z-XXCEU",   // 运维者 -gen-invite 签发，形如 XXXXX-XXXXX
  "deviceId": "a1b2c3...",        // 客户端稳定指纹，与 §3.6 同一个
  "label": "我的笔记本"            // 可选，≤40 字符，仅用于人读
}
```

**响应：**

```jsonc
// 201
{
  "ok": true,
  "data": {
    "key": "bk_9f3a2c…",         // 明文 Key，只在这次响应出现；服务端只存 sha256
    "ref": "cb4f408e3095",        // key_hash 前 12 位，吊销时用的句柄
    "name": "self:cb4f408e:我的笔记本",
    "scope": "writer",            // 自助注册永远是 writer，拿不到 admin
    "deviceId": "a1b2c3…"
  },
  "error": null, "cursor": null, "v": 1
}
```

**规则：**
- **一设备一 Key**：同一 `deviceId` 重复申请返回 `409 conflict`，不会悄悄发第二把。
- 邀请码按 `max_uses` 消费；**校验顺序是先验码、再查设备**，所以码打错时得到的是
  `forbidden` 而不是"该设备已领过 Key"这种误导性回答；设备冲突时**不消费**名额。
- 邀请码不存在 / 已停用 / 已过期 / 已用尽一律返回 `403 forbidden` 同一文案，
  否则本端点会变成邀请码存在性探针。
- 响应带 `Cache-Control: no-store`。明文 Key 不可找回，丢了只能重新申请。
- 自助 Key 只有 Bearer 一条鉴权路（不下发 HMAC secret，见 §1.3）。

### 3.10 `GET /v1/keys/self` 与 `DELETE /v1/keys/self` —— 查看 / 作废自己这把 Key

**鉴权：** 需要（Bearer 或签名均可）。只能操作调用者自己的 Key，无任何"列他人 Key"的能力。

```jsonc
// GET 200
{ "ok": true,
  "data": { "ref": "cb4f408e3095", "name": "self:cb4f408e", "scope": "writer",
            "deviceId": "a1b2c3…", "enabled": true, "created_at": 1756791234 },
  "error": null, "cursor": null, "v": 1 }

// DELETE 200
{ "ok": true, "data": { "ref": "cb4f408e3095", "revoked": true }, "error": null, "cursor": null, "v": 1 }
```

**规则：**
- 两个响应都**不回显明文 Key**（本来也不可恢复）。
- 作废是把 `enabled` 置 0，**不可撤销**；之后再用该 Key 请求一律 `401 unauthorized`。
- 运维侧的对应入口是 `-list-keys` / `-revoke-key`（见 `server.md` §11）。

---

## 4. 增量同步协议（`/delta`）

目标：客户端本地已有一份目录快照时，只拉取差异，**避免每次全量下载浪费源站带宽**。

**请求：**

```
GET /v1/prompts/delta?since=<catalog_hash>
```

- `since`：客户端上次同步到的 `catalog_hash`（取自 meta 或上次 delta 返回）。
- 省略 `since` 视为首次全量，返回全部 approved 提示词的分批结果（用 `cursor` 翻页）。

**响应 `data`：**

```jsonc
{
  "ok": true,
  "data": {
    "changes": [                       // upsert：新增或更新的完整条目（含正文 p）
      { "id": "p_new", "p": "……", "t": [], "v": 1, "s": "approved", "h": "..." },
      { "id": "p_z",  "p": "……", "t": [], "v": 5, "s": "approved", "h": "..." }
    ],
    "deleted": ["p_old"],              // 已下架/删除的 ID
    "since": "9f1d3c8e...",            // 本次交易后的新 catalog_hash，作为下次 since
    "has_more": true                    // 变更过多时的分批标志
  },
  "error": null,
  "cursor": "eyJvZmZzZXQiOiI4OTIifQ",   // has_more=true 时下一页游标
  "v": 1
}
```

**同步状态机（客户端）：**

```
本地无快照 ──GET meta──► 拿到 catalog_hash
       │                        │
       └──GET delta(无since)───► 全量分批拉取（loop has_more）
                                        │
                          GET delta(since=上次hash)──► 应用 changes/deleted 到本地缓存
                                        │
                          更新本地 hash = 返回的 since
```

---

## 5. 缓存与压缩（服务端强制）

| 端点 | ETag 依据 | 说明 |
|------|-----------|------|
| `meta` | `catalog_hash` | 命中 `If-None-Match` → `304` |
| `get/{id}` | `v`（版本号） | 版本不变 → `304` |
| 其余 | 默认不强缓存 | 列表/随机不缓存（保持新鲜） |

- 所有响应头带 `Cache-Control`（meta/get 可短缓存，如 `max-age=60`）。
- 客户端（CLI/SDK）**必须**发送 `If-None-Match` 并处理 `304`，以把重复流量降到接近 0。

---

## 6. 限流与配额

| 端点 | 匿名 | 带 Key |
|------|------|--------|
| `meta` | 10 次/分 | 60 次/分 |
| `list`/`random`/`get` | 60 次/分 | 300 次/分 |
| `delta` | 5 次/分 | 30 次/分 |
| `scores` | 不允许 | 30 次/分 |
| `prompts`(上传) | 不允许 | 10 次/分 |
| `keys`(自助注册) | 5 次/分 | 30 次/分 |

> `keys` 的匿名额度**必须显式配置**：限流器的语义是"未配置即不限流"（`limiter.go`），
> 漏配就等于把注册端点敞开的。

- 超限返回 `429` + `Retry-After`。
- 服务端另设**全局出站带宽看门狗**，接近配置阈值时优先对 `delta`/`list` 降级限流，保障 `get`/`random` 核心体验。

---

## 7. 兼容性规则（版本演进）

1. **只增不改不删**：新增字段不破坏老客户端；老字段语义永不变更。
2. 删除字段需先标记 `@deprecated` 保留至少一个大版本。
3. 破坏性变更新开 `/v2` 路径，`/v1` 冻结。
4. 客户端读到未知字段必须**忽略**（前向兼容）。

---

## 8. 交互时序（客户端视角）

```text
CLI/SDK                    服务端
  │  GET /v1/meta            │
  │─────────────────────────►│  返回 catalog_hash
  │  GET /v1/prompts/delta?since=hash
  │─────────────────────────►│  返回 changes/deleted + 新 hash
  │  ... 本地缓存就绪 ...     │
  │                          │
  │  GET /v1/prompts/random  │        ← 用户“随机测试”
  │─────────────────────────►│  返回 1 条正文
  │  POST /v1/scores         │        ← 用户打分
  │─────────────────────────►│  返回 avg/count
```