# SDK 与 CLI 实现规格

> 实现对象：`pkg/client`（SDK）+ `cmd/cli`（bench 命令）。**这是解多框架适配的核心**，DSH/Pi 插件均调 `bench`。

## 1. 为什么能力下沉 CLI

| 诉求 | 方案 |
|------|------|
| 双框架（DSH/Pi）适配成本 | 插件只做「调命令 + 回填」，零业务重复 |
| 单二进制分发 | Go 交叉编译出 Win/macOS/Linux 单文件，无运行时依赖 |
| 本地缓存/增量同步 | CLI 内建 SQLite 缓存，无框架也能用 |

## 2. SDK 接口（`pkg/client`）

```go
type Client struct {
    BaseURL   string
    APIKey    string
    Secret    string          // HMAC 用，可为空（只读匿名）
    HTTP      *http.Client    // 复用连接、超时
    Cache     *LocalCache     // 本地 SQLite
}

// 对应 docs/api.md 端点：
func (c *Client) Meta(ctx) (*Meta, error)
func (c *Client) Get(ctx, id string) (*Prompt, error)
func (c *Client) Random(ctx, tag string, exclude []string) (*Prompt, error)
func (c *Client) List(ctx, tag string) ([]PromptSummary, error)
func (c *Client) Sync(ctx) error              // 增量同步到本地缓存
func (c *Client) Score(ctx, id string, v int) (*ScoreResult, error)
func (c *Client) Upload(ctx, content string, tags []string) (*UploadResult, error)
```

## 2. SDK 接口（`pkg/client`，已实现）

```go

type Options struct {
    BaseURL   string
    APIKey    string
    Secret    string          // 非空走 HMAC，否则退回 Bearer
    Timeout   time.Duration
    HTTP      *http.Client     // 可注入；nil 时用默认 Transport
    CachePath string
    NoCache   bool             // 一次性脚本可关缓存
    DeviceID  string           // 覆盖评分去重指纹
    MaxAttempt int
    Now     func() time.Time            // 测试注入
    Backoff func(attempt int) time.Duration
}

func New(opt Options) (*Client, error)
func (c *Client) Close() error
func (c *Client) Cache() *Cache

// 对应 docs/api.md 端点：
func (c *Client) Meta(ctx context.Context) (*Meta, error)
func (c *Client) Get(ctx context.Context, id string) (*Prompt, error)
func (c *Client) Random(ctx context.Context, tag string, exclude []string) (*Prompt, error)
func (c *Client) List(ctx context.Context, tag string, limit int, cursor string) (*ListPage, error)
func (c *Client) Delta(ctx context.Context, since string, limit int, cursor string) (*Delta, error)
func (c *Client) Sync(ctx context.Context) (*SyncReport, error)
func (c *Client) Score(ctx context.Context, id string, v int) (*ScoreResult, error)
func (c *Client) Upload(ctx context.Context, content string, tags []string) (*UploadResult, error)
func (c *Client) UploadWithClientID(ctx context.Context, content string, tags []string, clientID string) (*UploadResult, error)

// 离线与状态：
func (c *Client) Cached(ctx context.Context, id string) (*Prompt, error)
func (c *Client) CheckStatus(ctx context.Context) (*Status, error)
func (c *Client) LocalCount(ctx context.Context) (int, error)
```

> **`pkg/client` 的生产代码不 import `internal/`**。它会被插件与第三方脚本直接引用，
> 一旦把内部类型泄露到导出签名，使用者就被锁死在我们的内部实现上。
> 唯一例外是测试文件（签名一致性测试必须两边对比，见 §12）。

**核心行为：**
- gzip 不手工处理：由 `net/http` 默认 Transport 自动加 `Accept-Encoding: gzip` 并透明解压；
  自己设这个头反而要自己解压。
- `Meta`/`Get` 自动带 `If-None-Match`，`304` 时回读本地副本/缓存。
- 写请求自动生成 `clientId`（上传）与 `deviceId`（评分，首次生成后持久化）。
- **本地先行校验**：空 id、分值越界、空正文直接报错，不浪费一个往返。
- 失败按 §7 重试策略。

## 3. 本地缓存（`LocalCache`，SQLite）

路径：`~/.bench/cache.db`（Windows `%APPDATA%\bench\cache.db`）。

```sql
CREATE TABLE prompt_cache (
  id   TEXT PRIMARY KEY,
  content TEXT NOT NULL,
  tags  TEXT,
  version INTEGER NOT NULL,
  content_hash TEXT,
  updated_at INTEGER
);
CREATE TABLE kv (k TEXT PRIMARY KEY, v TEXT);  -- 存 catalog_hash / device_id
```

- `Sync()` 结束后把新 `catalog_hash` 写入 `kv`。
- `Meta`/`Get` 的 304 命中即读该库，不重复下载。

## 4. 增量同步状态机

```
base := cache.GetKV("catalog_hash")       // 可能为空
next := base
cursor := ""
for {
    d := Delta(since=base, cursor=cursor)  // ← since 全程固定为 base
    applyChanges(d.changes)                // upsert 到 prompt_cache（幂等）
    applyDeleted(d.deleted)                // 删除本地条目（幂等）
    next = d.since
    if !d.has_more || d.cursor == "" { break }
    cursor = d.cursor
}
cache.SetKV("catalog_hash", next)          // 整轮结束后才推进
```

> ⚠️ **关键约束：翻页期间 `since` 必须固定。** 服务端返回的 `since` 是"本次结果对应的
> 新 hash"；若把它回喂给下一页，服务端会认为客户端已是最新而返回**空集**，
> 于是只能同步到第一页——一个不报错、但默默丢数据 bug。
> 回归测试：`TestEndToEndSyncKeepsSinceFixedAcrossPages`（用 107 条跨过一页）。

> 与 `docs/api.md` §4 一致。`Sync` 幂等，可随时重跑；无变化时只花一个请求。

## 5. CLI 命令（已实现）

> **不用 cobra**：与 ADR-8/9 一致的 stdlib-first 策略，用 `flag` + 内置分发器。
> 逻辑全部在 **`internal/cli`**（可测），`cmd/cli/main.go` 只有三行胶水——
> 因为 `main` 无法被单测驱动。

```
bench meta                        [--json]                 # 服务端/本地状态与是否落后
bench sync                        [--json]                 # 增量同步
bench get <id>                    [--json] [--local]       # 一键测试；--local 完全离线
bench random [--tag=] [--exclude=] [--fresh] [--json]      # 随机测试；--fresh 排除已见过
bench list [--tag=] [--limit=] [--all] [--json]            # 浏览摘要（不含正文）
bench score <id> <1-5>            [--json]                 # 打分（自动复用 device_id）
bench upload (-c 文本|-f 文件|管道) [-t a,b] [--client-id=] [--json]
bench config init|show|set                                 # init 需 --endpoint
bench reset                                                # 清缓存（保留 device_id）
bench version
```

**全局参数：** `--endpoint` `--key` `--secret` `--home DIR` `--json` `--quiet` `--timeout 20s`

> 参数顺序**宽容**：Go 的 `flag` 包遇到第一个位置参数就停止解析，会把位置参数后面的
> `--home` **静默丢弃**（不报错）。`splitArgs` 先做一次分拢，所以
> `bench config init --home X` 与 `bench config --home X init` 都能用。

**输出约定（插件解析契约）：**
- 默认：正文走 **stdout**，元信息（`# id v3 tags=…`）走 **stderr**，
  所以 `bench get p_x | your-llm-cli` 不会被污染。
- `--json`：结构化结果走 stdout；**出错时也走 stdout** 的 `{"ok":false,"error":{"code":…}}`，
  插件无需去混流的 stderr 里抓错。
- 退出码（见 §11）：`0/1/2/3/4/5`，与错误码一一对应。
- 凭据永不明文回显：`config show` 只打掩码，`--json` 只给 `has_key` 布尔。

## 6. 配置文件（`~/.bench/config`）

```yaml
endpoint: https://bench.example.com
api_key: ""
secret: ""            # 只读可留空
device_id: a1b2c3...   # 首次 init 生成，评分去重用
```

- 优先级：CLI flag > 环境变量 `BENCH_*` > 配置文件。

## 7. 重试与退避

| 场景 | 策略 |
|------|------|
| 网络错误 / 5xx | 指数退避重试（1s、2s、4s），**仅限幂等只读请求** |
| 写请求（score/upload）遇 5xx | **不重试**：服务端变慢时重试会把延迟成倍放大，比偶发失败更糟 |
| `429 rate_limited` | 读 `retry_after`，等待后重试（仍受只读限制） |
| `304` | 命中本地缓存，直接返回 |
| `conflict`/`validation_failed`/其它 4xx | 不重试，直接报错给用户 |
| 信封 `v` 不是 1 | 立即失败（`bad_response`），不尝试解析可能错位的字段 |

## 8. HMAC 签名（写入端点）

```go
func sign(method, path string, ts int64, body []byte, secret string) string {
    canonical := method + "\n" + path + "\n" + strconv.Itoa(ts) + "\n" + hex(sha256(body))
    mac := hmac.New(sha256.New, []byte(secret))
    mac.Write([]byte(canonical))
    return hex.EncodeToString(mac.Sum(nil))
}
// 头：X-Api-Key / X-Timestamp / X-Signature
```

## 9. 幂等键生成（已实现）

```go
// deviceId：优先级 Options.DeviceID（来自 ~/.bench/config）
//                  > 本地缓存 kv 表的 device_id（自动生成并持久化）
//                  > NoCache 且未指定时临时生成 + 告警
func (c *Cache) DeviceID(ctx) (string, error)   // "d_" + hex(8 bytes)，首次写入后稳定

// clientId：每次上传一个新随机值；也可由调用方指定以便重放
newClientID := "c_" + hex(rand(8))
func (c *Client) UploadWithClientID(ctx, content, tags, clientID) (*UploadResult, error)
```

> 用随机 hex 而不是 UUID：少一个依赖，而幂等只需要“局部唯一 + 不可预测”，
> 8 字节随机数在这个量级上碰撞概率可忽略。
>
> **重要：**`Upload()` 自动生成的 clientId 意味着**网络重试不能用于上传**（已按§7 禁用）。
> 对可能超时的导入脚本，应显式传 `--client-id`，这样重放不会制造重复条目。

## 10. 构建与分发（已接入 Makefile）

```bash
make build-cli     # 当前平台 → dist/bench$(GOEXE)（Windows 自动带 .exe）
make build-all     # 交叉编译 linux/amd64、darwin/arm64、windows/amd64
make smoke-cli     # 用真实二进制跑端到端冒烟
```

或手写：

```bash
CGO_ENABLED=0 GOOS=linux   GOARCH=amd64 go build -trimpath -ldflags "-s -w" -o dist/bench-linux-amd64       ./cmd/cli
CGO_ENABLED=0 GOOS=darwin  GOARCH=arm64 go build -trimpath -ldflags "-s -w" -o dist/bench-darwin-arm64      ./cmd/cli
CGO_ENABLED=0 GOOS=windows GOARCH=amd64 go build -trimpath -ldflags "-s -w" -o dist/bench-windows-amd64.exe ./cmd/cli
```

- 纯 Go SQLite 驱动使 `CGO_ENABLED=0` 真正可行——这是选它的关键原因（插件用户跳不过装编译器这一关）。
- 分发：`go install` / GitHub Releases 三平台二进制 / 一键安装脚本。

## 11. 错误码透传

SDK 将 `error.code` 映射为 Go 错误类型，CLI 映射到 stderr + 退出码：

| error.code | 退出码 |
|-----------|--------|
| `rate_limited` | 2（可等重试） |
| `unauthorized`/`forbidden` | 3 |
| `not_found` | 4 |
| `bad_request`/`validation_failed`/`too_large`/`conflict`/`bad_response`/未知码 | 5 |
| 网络/`internal`/`unavailable` | 1 |
| `canceled`（context 取消/超时） | 1（`internal/cli` 的 `classifyError`） |
| `local_error`（本地配置错误，如未配置 endpoint） | 5（同上；**不要重试**，指引 `bench config init`） |

> 本地分类码 `canceled` / `local_error` 由 CLI 层生成（SDK 侧只有
> `pkg/client/errors.go` 的 12 个映射码 + `bad_response`/`network`）。
> 插件侧请以此为事实来源，不要只对照本表上半段。

> 未知错误码必须落到 **5** 而不是 0——否则新增的服务端错误码会被旧版插件当成成功。

## 12. 实现偏差与要点

| 项 | 偏差/要点 |
|----|------------|
| 路由/CLI 框架 | 不用 chi、不用 cobra、不用 golang-lru（ADR-8/9/11），依赖保持 2 个 |
| `internal/sync` | 改名 `internal/catalog`，避开标准库包名冲突（ADR-7） |
| CLI 位置 | 逻辑在 `internal/cli`，`cmd/cli` 只装信号与退出码；`main` 不可测 |
| device_id | 实际持久化在**本地缓存 kv 表**，`config` 里的字段是可选覆盖 |
| Windows 权限 | 配置目录写 0600 并**显式 Chmod**（`os.WriteFile` 对已存在文件不会改权限）；但 Windows 无 POSIX 权限位，Chmod 失败**不视为致命**，否则 `config init` 在 Windows 直接不可用 |

**签名一致性（最容易被拖坑的地方）**：SDK 与服务端各有一份 `Canonical`/`Sign`
（生产代码不能互 import），漂移的症状是“所有写请求 401”。因此：

- `TestSignatureParityWithServer`：逐向量比对两侧输出（含中文 secret、负时间戳、空 body、含空格 path）。
- `TestSDKSignatureAcceptedByServerVerifier`：拿**服务端生产代码** `auth.Authenticator`
  去验 SDK 产出的签名头，幷验证“沿用旧签名偷换 payload”必须被拒。
  （首版测试写成“用被篡改的 body 重新签名”，那是一个自洽的合法签名，测试会假阳性通过）
- `TestSDKBearerAcceptedByServerVerifier`：覆盖前端常用的 Bearer 路径。