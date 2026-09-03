# 源站实现规格（`cmd/server` + `internal/api`）

> 实现对象：`cmd/server` + `internal/api` + 依赖模块。契约依据 `docs/api.md`。

## 1. 依赖清单（已实现）

```
module github.com/example/benchmark-prompts

go 1.22

require (
    gopkg.in/yaml.v3 v3.0.1
    modernc.org/sqlite v1.34.5
)
```

> 实际只有 **2 个直接依赖**。原计划的 `chi` 与 `hashicorp/golang-lru` 均已改为自实现：
>
> * 路由 → **Go 1.22 增强 `net/http.ServeMux`**（支持 `"GET /v1/prompts/{id}"` 方法+路径模式与 `r.PathValue`）。依赖更少，也消除了一个不可验证的第三方假设。
> * LRU → `internal/cache`（约 90 行，带 TTL，测试 100% 覆盖）。
>
> 静态检查、鉴权、限流、压缩全部标准库。**注意**：国内网络下 `go mod tidy` 需先
> `go env -w GOPROXY=https://goproxy.cn,direct`，默认 `proxy.golang.org` 不可达。

## 2. 中间件链（已实现，由外到内）

```
recover            → panic 兜底，转 500 不崩进程
 request-id        → 注入/透传 X-Request-Id
  security-headers → 安全头 + CORS 白名单
   metrics(+日志)  → 请求计数 + 真实出站字节（带宽看门狗数据源）
    compress       → gzip（304 不压缩、不写尾块）
     mux/handler   → 路由与业务
```

按端点包装的顺序：

```
只读： degrade(endpoint) → limit(endpoint) → handler
写入： auth → limit(endpoint) → handler      # 限流必须在鉴权之后，才能按身份分桶
```

**两处与早期设想的差异（实现时修正）：**

1. **`logging` 与 `metrics` 合并为一层**。若拆成两层，各自包一次 `ResponseWriter`，
   同一批字节会被累加两遍，带宽统计失真。
2. **`compress` 必须包在 `metrics` 之内**。这样 gzip 后的字节才流入 `metricsWriter`，
   `BytesOut` 统计到的才是真实出站量——这直接决定看门狗判断是否准确。
   （首版实现漏接了 `compressMW`，由契约测试 `TestContractGzipRoundTrip` 抓出。）
- 写响应统一走 `renderOK / renderErr`，避免散落。ETag 判定在 handler 内做（meta/get），
  命中时直接 `WriteHeader(304)`，不进入压缩。

## 3. 鉴权实现（`internal/auth`）

**API Key 校验**：
1. 读 `Authorization: Bearer <key>` 或 `X-Api-Key`。
2. `sha256(key)` 查 `api_keys`，校验 `enabled`。
3. 命中后把 `name` 写入 context（供日志/限流区分）。

**HMAC 签名校验**（SDK/CLI 推荐）：
```go
func Verify(r *http.Request) error {
    ts, _ := strconv.ParseInt(r.Header.Get("X-Timestamp"), 10, 64)
    if abs(time.Now().Unix()-ts) > cfg.MaxClockSkew { return ErrUnauthorized }
    key := r.Header.Get("X-Api-Key")
    sig := r.Header.Get("X-Signature")
    body, _ := io.ReadAll(r.Body)          // 需缓存 body 供 handler 复读
    // canonical: method + "\n" + path + "\n" + timestamp + "\n" + sha256(body)
    expected := hmacSHA256(secret, canonical)
    if !hmac.Equal([]byte(expected), []byte(sig)) { return ErrUnauthorized }
}
```
- `secret` 用于 HMAC 签名比对，**必须以可解密形式存储**：`api_keys.secret_enc` 存 AES-GCM 密文（密钥经环境变量注入，见 `storage.md` §2），校验时解密后计算 expected 签名并比对。不得存 `secret` 的哈希（不可逆则无法验签），也不得明文落盘。
- body 复读：handler 前用 `io.TeeReader` 缓存。

## 4. 限流实现（`internal/ratelimit`）

- **滑动窗口计数**（按 `client_id + endpoint` 分桶，或用 `golang.org/x/time/rate` 令牌桶）。
- 匿名键：`an:<ip>`；鉴权键：`au:<key_name>`。
- 返回 429 时带 `Retry-After`，并把 `retry_after` 写入 error 对象。

```go
type Limiter struct { window time.Duration; max map[string]int }
func (l *Limiter) Allow(endpoint, key string) (bool, time.Duration) { ... }
```

## 5. 压缩与 ETag

**ETag（`meta`/`get`）**：
```go
// meta: etag = catalog_hash
// get:   etag = `"p_xxx:v3"`   （版本号变化即失效）
if r.Header.Get("If-None-Match") == etag {
    w.WriteHeader(304); return
}
w.Header().Set("ETag", etag)
w.Header().Set("Cache-Control", "max-age=60")
```

**gzip**：
- `Accept-Encoding` 含 `gzip` 时压缩，`Content-Type` 为 `application/json` 或 `text/*`。
- 优先 `br`（客户端支持时），标准库无 br，初期 gzip 即可。
- 已在 cache 中缓存的 gzip 字节直接写出，避免重复压缩（见 `storage.md` §4）。

## 6. 端点处理逻辑（伪代码）

### 6.1 GET /v1/meta
```
data = { total: store.CountApproved(),
         catalog_hash: sync.CatalogHash() [读缓存],
         schema_version: 1,
         server_time: now }
etag = catalog_hash
```

### 6.2 GET /v1/prompts
```
limit = clamp(query.limit, 1, 100; 默认 20)
offset = decode(cursor)  // cursor 不透明，服务端存 offset
rows = store.ListApproved(limit+1, offset, tagFilter)   // 多查一条判断是否有下一页
has_more = len(rows) > limit
items = rows[:min(limit, len(rows))]                     → []PromptSummary
cursor = has_more ? encode(offset+len(items)) : null
```

### 6.3 GET /v1/prompts/{id}
```
p = store.GetApproved(id) ；无 → 404
etag 按 version；命中 → 304
返回完整 Prompt
```

### 6.4 GET /v1/prompts/random
```
exclude = parse(query.exclude)
p = store.RandomApproved(exclude, tag)  // ORDER BY random() LIMIT 1，排除 exclude
无 → 404（或随机为空）
返回完整 Prompt（不强缓存）
```

### 6.5 GET /v1/prompts/delta
```
since = query.since
if since == "" : 全量分批（用 cursor 翻页返回所有 approved）
else :
    ts = sync.LookupSinceTime(since)  // 找不到 → 回退全量
    changes = store.PromptsUpdatedSince(ts)  → 完整 Prompt（含 p）
    deleted = store.DeletedSince(ts)         → []id
    newHash = sync.CatalogHash()
    data = { changes, deleted, since: newHash, has_more }
```

### 6.6 POST /v1/scores（鉴权 + 限流）
```
body 校验：value∈[1,5]；deviceId 非空；id 存在且 approved → 否则 404/422
store.UpsertScore(prompt_id, value, device_id)  // UNIQUE 冲突则 UPDATE
data = { avg: store.ScoreAvg(id), count: store.ScoreCount(id) }
失效缓存 prompt:id
```

### 6.7 POST /v1/prompts（鉴权 + 限流）
```
body 校验：len(content)∈[1,8192] ；clientId 必填
若 uploads[clientId] 存在 → 幂等返回原 id
若 content_hash 已存在 approved → 返回既有 id（去重，不新建）
否则：入库 status=pending + 记 uploads + 触发 modreation 过滤
返回 202 { id, s: "pending" }
```

## 7. 带宽看门狗（`internal/metrics` + `internal/api`）

- 后台协程每秒统计出站字节速率（滑动 60s 均值）。
- 超过 `bandwidth.max_mbps` 时：
  1. 置降级标志，`/delta`、`/list` 返回 `503 unavailable`（`error.code=unavailable`，`retry_after` 提示）。
  2. 保障 `get`/`random`/`meta` 核心体验。
  3. 低于阈值自动恢复。
- 指标暴露：`GET /-/metrics`（仅内网或带管理 Key），输出 JSON，供监控采集。

## 8. 静态资源与 TLS 终结

- 源站**不服务前端静态资源**（前端在 CDN）。
- 生产只开 443：Go 内建 `http.Server` + `TLSConfig`，证书挂 `/etc/bench/tls.{crt,key}`。
- 可选前置 Caddy/Nginx 做 TLS 与更细粒度压缩，但为少进程本文档倾向 Go 直接 TLS。

## 9. 启动流程（cmd/server/main.go）

```
1. config.Load() → 默认值 + YAML + 环境变量覆盖 → Validate
2. store.Open(path) → PRAGMA → Migrate（embed 迁移，幂等）
3. 维护子命令优先处理：-backup / -put-key / -review / -approve / -reject
4. 装配 cache / catalog / auth / ratelimit / moderation / metrics
5. 启动带宽看门狗 goroutine + 维护 goroutine（限流桶 GC、快照裁剪）
6. http.ListenAndServeTLS（-dev 或无证书时降级为明文）
7. 优雅关闭：SIGINT/SIGTERM → server.Shutdown(10s) → store.Close()
```

## 11. 运维子命令（同一个二进制）

审核与备份不需要单独的后台系统，共用 `bench-server`：

```bash
bench-server -config c.yaml -put-key "alice:<key>:<secret>"   # 登记 API Key
bench-server -config c.yaml -review                          # 列出待审核队列
bench-server -config c.yaml -approve p_1a2b3c4d              # 审核通过
bench-server -config c.yaml -reject  p_1a2b3c4d              # 审核打回
bench-server -config c.yaml -backup  /backup/bench-20260902.db  # 一致性备份
```

要点：
- 所有子命令复用同一纯 Go SQLite 驱动，**不依赖 `sqlite3` CLI**。
- 可与运行中的服务**开同一 DB**（WAL + `busy_timeout=5000`），冒烟测试已验证跨进程审核。
- `-approve/-reject` 会递增 `version`，从而自然驱动 ETag 失效与 delta 下发。
- 主密钥从环境变量 `BENCH_SECRET_KEY`（32 字节 hex）读取；`-dev` 下缺失会生成一次性密钥并告警。

## 10. 错误处理约定（handler 侧）

```go
func handleGet(w, r) {
    p, err := store.GetApproved(r.Context(), id)
    if errors.Is(err, store.ErrNotFound) { render.Error(w, errs.ErrNotFound); return }
    if err != nil { slog.Error(...); render.Error(w, errs.ErrInternal); return }
    render.JSON(w, 200, envelope{p})
}
```

- **不 panic**（除 main 装配）；`recover` 中间件兜底 500。
- 所有 DB 错误落到 `internal`(500)，不回显 SQL 错误给客户端。