# 架构与模块设计

## 1. 架构概览

```
浏览器/前端                 CLI / DSH / Pi 插件
    │  (静态资源)                │  (HTTPS /v1/*)
    ▼                            ▼
┌─────────────┐          ┌─────────────────────────────┐
│ 亚太 CDN     │          │  源站（2H2G）                 │
│ (web/*)     │          │  ┌─────────────────────────┐ │
└─────────────┘          │  │ api (net/http ServeMux)  │ │
                         │  │  ├─ middleware 链         │ │
                         │  │  ├─ handlers              │ │
                         │  │  └─ 带宽看门狗            │ │
                         │  └───────┬─────────────────┘ │
                         │          │                   │
                         │  ┌───────▼─────────────────┐ │
                         │  │ auth  │ ratelimit │ sync │ │
                         │  ├───────┴───────────┴─────┤ │
                         │  │ store ─► cache ─► SQLite │ │
                         │  │ moderation              │ │
                         │  └─────────────────────────┘ │
                         └─────────────────────────────┘
```

**核心原则：**
1. **前端零回源**：`web/*` 全量放 CDN，源站带宽只做 API。
2. **读多写少**：热点读走内存 LRU；写路径（评分/上传）低频且量小。
3. **无状态**：API 进程无本地会话，水平可扩（当前单机足够）。
4. **可替换的存储接口**：`store` 定义接口，SQLite 是实现，后续换库不动上层。

## 2. 分层与依赖方向

```
internal/api ──► internal/{store,cache,sync,auth,ratelimit,moderation,metrics}
       │
       └──► internal/model（纯数据，被所有层引用）

pkg/client ──► 仅依赖标准库 + gopkg.in/yaml.v3（独立可发布）
```

- **禁止**：`internal/*` 依赖 `cmd/*`；下层依赖上层。
- **`model` 只放结构体与校验**，不掺 IO、不掺业务。
- **`store` 是唯一数据库出入口**；`cache` 是 `store` 的可选装饰（先查缓存，未命中回源再回填）。

## 3. 模块职责

| 模块 | 职责 | 关键导出 |
|------|------|----------|
| `internal/config` | 从 YAML/环境变量读配置，启动时校验 | `Load(path) (*Config, error)` |
| `internal/model` | 领域结构体 + 字段校验 | `Prompt`、`PromptSummary`、`Score`、`Meta` |
| `internal/store` | SQLite 仓储：CRUD、分页、delta 变更集 | `Store` 接口 |
| `internal/cache` | 进程内 LRU + TTL（自实现，约 90 行） | `Get/Set/Delete` |
| `internal/catalog` | 目录 hash 计算、按 since 生成 changes/deleted | `Hash()`、`Refresh()`、`Delta()` |
| `internal/auth` | API Key + HMAC 签名校验 | `Middleware`、`Verify` |
| `internal/ratelimit` | 令牌桶/滑动窗口限流 | `Limiter` |
| `internal/moderation` | 敏感词/规则过滤、审核队列 | `Filter(text)` |
| `internal/metrics` | 内存计数器（请求量、带宽、304、限流触发） | `Registry` |

## 4. 配置项（`config.yaml`）

```yaml
server:
  addr: ":443"
  read_timeout: 10s
  write_timeout: 20s
  tls_cert: /etc/bench/tls.crt
  tls_key: /etc/bench/tls.key

store:
  path: /var/lib/bench/bench.db
  migrate: true

auth:
  readonly_anonymous: true          # 只读端点匿名开放
  max_clock_skew: 300s              # HMAC 时间戳容差

ratelimit:
  anonymous: { meta: 10, list: 60, random: 60, get: 60, delta: 5 }
  authed:    { meta: 60, list: 300, random: 300, get: 300, delta: 30, scores: 30, upload: 10 }

bandwidth:
  watch_enabled: true
  max_mbps: 8.0                     # 出站告警/降级阈值，推导见 deployment.md §6
  degrade: [delta, list]            # 优先级最低的降级端点

moderation:
  enabled: true
  banned_words_file: /etc/bench/banned.txt
  max_prompt_len: 8192
```

- 环境变量优先级高于 YAML：`BENCH_<PATH>` 形式（如 `BENCH_SERVER_ADDR`）。
- 敏感项（API secret）只允许环境变量注入，不入仓库。

## 5. 日志规范（`log/slog`）

| 级别 | 用途 |
|------|------|
| DEBUG | 单请求内部细节（本地联调） |
| INFO | 启动/关闭、迁移、备份 |
| WARN | 限流触发、降级、审核队列积压 |
| ERROR | 500、DB 错误、不可恢复异常 |

- 结构字段统一：`method`、`path`、`status`、`dur_ms`、`bytes`、`client_id`（鉴权后）。
- **禁止**把提示词正文、API Key、签名写入日志。

## 6. 错误模型

- 领域错误定义在 `internal/api/errs.go`，映射到 `docs/api.md` §1.2 的 `error.code`：

```go
type APIError struct {
    Code    string
    Message string
    HTTP    int
}
var (
    ErrBadRequest   = APIError{Code: "bad_request", HTTP: 400}
    ErrUnauthorized = APIError{Code: "unauthorized", HTTP: 401}
    ErrNotFound     = APIError{Code: "not_found", HTTP: 404}
    ErrRateLimited  = APIError{Code: "rate_limited", HTTP: 429}
    // ...
)
```

- handler 返回 `(*APIError)` 时中间件统一渲染信封；其余错误渲染 `internal`(500) 并 ERROR 日志。

## 7. 技术栈备选（Node/TS 切换成本点）

若切换 Node/TS，以下不变：API 契约、数据模型、目录 hash 算法、增量同步协议、CDN 方案。需要替换的仅是实现层：`net/http → Fastify`、`modernc.org/sqlite → node:sqlite`、`自实现 flag → commander`、`crypto/hmac → crypto`。建议评估完团队工具链后**一次性决策，不再反复**。

> 实现进度备注：源站已按 **Go 主线** 完成并通过全部测试（含 45 项端到端冒烟），未发生切换。

## 8. 关键技术决策记录（ADR 简表）

| 编号 | 决策 | 理由 |
|------|------|------|
| ADR-1 | 源站 Go + SQLite 单机单进程 | 2H2G 资源余量大，避免多进程/多服务运维 |
| ADR-2 | 纯 Go SQLite 驱动（无 CGO） | 交叉编译零成本（供多平台 CLI） |
| ADR-3 | 前端零回源 · 纯静态 | 源站带宽有限，前端资产必须完全脱离源站（阈值见 `deployment.md` §6） |
| ADR-4 | 增量同步用“目录 hash + delta 变更集” | 避免全量拉取，最小化重复流量 |
| ADR-5 | 列表返回 PromptSummary（无正文） | 正文只在 get/random/delta 返回，省带宽 |
| ADR-6 | 能力下沉 CLI，插件只做薄胶水 | 解 DSH/Pi 双框架适配 |
| ADR-7 | 包名 `internal/sync` → **`internal/catalog`** | 与标准库 `sync` 包名冲突，import 后无法再用 `sync.Mutex` |
| ADR-8 | 路由用 Go 1.22 增强 `ServeMux`，不引入 chi | 减依赖；方法+路径模式与 `PathValue` 已够用 |
| ADR-9 | LRU 自实现，不引入 golang-lru | 只需字节缓存，90 行且 100% 测试覆盖 |
| ADR-10 | 正文只 `TrimSpace`，**不折叠内部空白** | benchmark 提示词常为多行，折叠会破坏内容；空白归一仅用于算 hash |
| ADR-11 | CLI 用 stdlib `flag` + 内置分发器，不用 cobra | 继续压依赖；子命令逻辑全在 `internal/cli` 以保证可测 |
| ADR-12 | SDK 重试**只限幂等只读请求** | 服务端变慢时重试写请求会把延迟成倍放大，比偶发失败更糟 |
| ADR-13 | `pkg/client` 生产代码不 import `internal/` | 公开 SDK 一旦泄露内部类型，使用者就被锁死在本项目的实现上 |