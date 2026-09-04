# 开发文档总纲

本目录承载项目的全部开发文档。阅读顺序：**README → architecture → storage → server → client → plugins → frontend → deployment → testing**。

## 文档地图

| 文档 | 内容 |
|------|------|
| [api.md](./api.md) | REST API 契约（已冻结，各方并行开发依据） |
| [architecture.md](./architecture.md) | 系统架构、分层、依赖方向、配置、日志、错误模型 |
| [storage.md](./storage.md) | 数据模型、DDL、迁移、缓存、去重、审核状态机 |
| [server.md](./server.md) | 源站实现规格 |
| [client.md](./client.md) | SDK 与 CLI 实现规格 |
| [plugins.md](./plugins.md) | DSH / Pi 插件适配规格（两框架能力对齐表、统一回填模板、边界情况） |
| [handover-dsh.md](./handover-dsh.md) | **DSH 框架研究底稿**：插件 API 形状 + 源码行号，
  兼 `bench` CLI 契约的**实测地面数据**采集（§9）与 §12 核实记录。实现已完成，
  日常使用看 `../benchmark-prompts/plugins/dsh/README.md` |
| [frontend.md](./frontend.md) | 前端静态页规格 |
| [deployment.md](./deployment.md) | 构建、部署、运维、监控、安全 |
| [testing.md](./testing.md) | 测试策略与验收标准 |

仓库根另有两份**私人笔记**（`初始构想.md` 需求、`方案与可行性研究.md` 方案），
已加入 `.gitignore`，**不随仓库发布**。它们的有效结论已归入本目录：
架构决策与 ADR 见 [architecture.md](./architecture.md)，里程碑与发布记录见
[`../benchmark-prompts/CHANGELOG.md`](../benchmark-prompts/CHANGELOG.md)。

---

## 0. 谁是事实来源（权威层级）

约定：

| 主题 | 唯一权威 | 其余文档的角色 |
|---|---|---|
| 线上 API 契约（字段、错误码、信封） | **[api.md](./api.md)**（冻结，只增不改不删）+ `internal/api/contract_test.go` | 其他文档引用而非重定义 |
| `bench` CLI 的**命令面与退出码** | [client.md](./client.md) §5/§11 + `internal/cli/commands.go` | `plugins.md` §1 只列“插件要用哪几条” |
| CLI `--json` 的**真实输出样本** | [handover-dsh.md](./handover-dsh.md) §9（由 `scripts/capture-contract.sh` 采集，可重跑再生） | — |
| **DSH** 插件 API 形状与实测证据 | [handover-dsh.md](./handover-dsh.md) §2–§8（源码路径 + 行号） | `plugins.md` §4 只给结论摘要 |
| 插件**安装/使用/装载路线** | `../benchmark-prompts/plugins/{pi,dsh}/README.md`（由实现者维护） | `plugins.md` 不写安装步骤 |
| 适配层规格与两框架对齐表 | [plugins.md](./plugins.md) | — |
| 前端**规格**（页面、约束、体积基线） | [frontend.md](./frontend.md) | `../benchmark-prompts/web/README.md` 只讲怎么用、注意什么 |
| 前端**部署步骤 / 缓存头 / CORS 白名单** | [deployment.md](./deployment.md) §7 | `frontend.md` §9 与 `web/README.md` 均只指向它 |
| 凭据**契约**（注册端点、作用域语义） | [api.md](./api.md) §3.9–§3.10 | — |
| 凭据**发放操作**（签发邀请码、列/吊销 Key） | [deployment.md](./deployment.md) §5 + [server.md](./server.md) §11 | `web/README.md` 只讲使用者视角 |
| **测试项数 / 覆盖率 / 门禁状态** | [testing.md](./testing.md) §10 与 `../benchmark-prompts/README.md` | 其他地方引用时写目标名（`make check`）而非抄数字 |
| 里程碑与发布记录 | [`../benchmark-prompts/CHANGELOG.md`](../benchmark-prompts/CHANGELOG.md)；ADR 表在 [architecture.md](./architecture.md) | 私人笔记（方案研究）不入库 |
| **代码与测试本身** | ✅ **最高优先级** | 与文档矛盾时以代码为准，**并立即回写文档** |

两条维护规则：

1. **文档与代码冲突时，以代码为准并当场回写**。“只是口头承认差异”不算修完。
2. **不要在引用位抄数字**。

---

## 0.5 用语与写作约定

> 本节是文档校对与新增内容的唯一依据。括号里的原名是代码/API 中的字面量，保留不改。

### 术语

> 领域名词的**定义**见 §7；本节只管**用词统一**。

| 统一用词 | 不再使用 | 边界 |
|---|---|---|
| 源站 | 后端、服务端（指部署形态时） | 指 HK 上的 `bench-server` 进程与其数据；讲代码分层时才写“后端代码” |
| 插件 | 扩展（指适配物时） | Pi 的具体机制仍叫 extension，路径/命令保留原名 `plugins/pi/extension/` |
| Pi | 正文里的小写 pi | 小写 `pi` 只用于可执行文件名、路径、包名、代码；中文行文统一 Pi |
| DSH | 重复展开 DeepSeek Harness | 仅首次出现处展开一次，此后一律 DSH |
| 打分 / 评分 | 计分、评价 | 动作用“打分”，结果（`avg`/`count`）用“评分” |
| 端点 | 接口（指 HTTP endpoint 时） | “接口”只留给 Go interface 与抽象边界 |
| 门禁 | 质量门、闸门 | 指提交前必须全绿的一组检查；命令入口一律写 `make check` |
| 产物 | 制品、构建物 | 指 `dist/` 下的文件；发布产物 = 二进制 + 归档 + 校验值 |
| 本地缓存 | 客户端缓存 | 指 `bench` 的 SQLite；服务端那份只叫“缓存”（LRU） |
| 香港 | 港服、HK 节点 | 行文用“香港”；`HK` 只出现在表格与紧凑注记里，首次写“香港（HK）” |
| CLI | 命令行工具（反复出现时） | 首次介绍可写“命令行工具（CLI）”，此后一律 `CLI`；子命令名用 `bench <cmd>` |
| 抓出 | 拓出、抖出、暴露 | 固定句式：“被测试抓出的缺陷” |
| 阈值 | 门限 | |
| 版本注入 | 写入版本、埋版本 | 统一“版本注入 / `-X` 注入” |

### 写法

1. **交叉引用**：跳文件用 Markdown 链接——同目录 `[testing.md](testing.md)`、
   跳目录 `[testing.md](../docs/testing.md)`；指向小节写“见 §8”，不写“见上”。
   行文里提到文件用反引号 `docs/api.md`，**不混用裸文件名**。
2. **引号**：中文语境用 “ ”；代码、命令、路径、字段名一律反引号。
3. **命令引用**：写 `make smoke-dsh`，不写 `bash scripts/smoke-dsh.sh`——除非语境就要求“能直接执行”。
4. **状态符号只用三个**：✅ 已完成 / ⬜ 未做 / 🔶 部分完成。`⚠️` 只用于警示块，
   `★` 只用于文件清单里标“先读这个”。
5. **小节引用一律用编号**（“见 §12”），不用“上一节/上面那个表”这类相对位置词。
6. **不写第一人称与过程自述**：错误原因按“事实 + 后果”陈述，保留可复用的规则、
   删掉“我猜错了/本轮发生过/上一轮整理”这类叙述——要细节查提交历史。
7. **批量改词先排除约定表本身**：“不再使用”列里写的正是被替换的对象，不隔离就会
   把规则本身改掉。改完必须回读本节与 §0 两张表。

---

## 1. 已锁定的技术栈

| 层 | 选型 | 版本约束 |
|----|------|----------|
| 源站 | Go | **1.22+**（实现用增强 `net/http.ServeMux`：方法+路径模式） |
| HTTP 路由 | 标准库 `net/http` | **不引入 chi**，见 `architecture.md` ADR-8 |
| 数据库 | SQLite（`modernc.org/sqlite`） | 纯 Go，无 CGO，跨平台 |
| 缓存 | 进程内 LRU + TTL（`internal/cache` 自实现） | 不引入 golang-lru，ADR-9 |
| CLI | 标准库 `flag` + 内置子命令分发器 | **不引入 cobra**，见 `architecture.md` ADR-11；直接依赖共 2 个 |
| 配置 | YAML（`gopkg.in/yaml.v3`） | |
| 签名 | 标准库 `crypto/hmac` + `crypto/sha256` | |
| 前端 | Astro（`output: 'static'`）+ Preact 岛 | 见 `frontend.md` §1；产物纯静态上 CDN |
| 测试 | 标准库 `testing` + `httptest` | |

> **备选方案**：若团队无 Go 工具链，可整体切换 **Node/TypeScript**（Fastify + better-sqlite3 + pnpm），接口契约不变。切换成本点见 `architecture.md` §7。当前以 **Go 为唯一主线**。

### 2.1 实际工具链情况（已真实验证）

源站已在下述环境**编译、vet、`-race` 测试、47 项端到端冒烟全部跑通**：

| 项 | 实际情况 |
|----|----------|
| Go | `D:\Scoop\apps\go\current` → **go1.27.1**（scoop 已装，但不在 WSL 默认 PATH，需显式加入） |
| Shell | Windows 上可能同时存在 **Git Bash(MINGW64)** 与 **WSL** 两种 bash，路径写法不同（`/d/...` 与 `/mnt/d/...`） |
| GOPROXY | 默认 `proxy.golang.org` **不可达**，已改为 `https://goproxy.cn,direct` |

首次拉依赖：

```bash
go env -w GOPROXY=https://goproxy.cn,direct   # 国内网络必需
go mod tidy
```

常用入口：`make check`（fmt+vet+build+race）、`bash scripts/smoke.sh`（端到端）。

## 2. 开发环境准备

**验证环境的 Go 工具链**（Windows 需自行安装）：

```bash
# 建议用官方安装包或 winget/scoop
go version          # 期望 go1.22+
go env GOPATH
```

必备工具自检（首次使用逐个 `--version`）：

| 工具 | 用途 | 命令 |
|------|------|------|
| gofmt | 格式化 | `gofmt -l .` |
| go vet | 静态检查 | `go vet ./...` |
| staticcheck | 深度静态检查 | `staticcheck ./...`（**已纳入 `make check`**，当前零发现） |
| go test | 测试 | `go test ./...` |
| biome | TS 插件与前端 JS/JSON/MD 的 lint + 格式化 | `biome check plugins/pi/extension/ plugins/dsh/`（M4 起的实际用法；配置见 `benchmark-prompts/biome.json`） |

## 3. 完整目录结构（约定）

```
benchmark-prompts/
├── cmd/
│   ├── server/main.go          # 源站 API 入口（装配依赖、启动 HTTP）
│   └── cli/main.go             # bench CLI 入口
├── internal/
│   ├── config/                 # 配置加载与校验
│   ├── api/                    # HTTP 层：router、handler、middleware
│   ├── store/                  # SQLite 数据访问（仓储接口 + 实现）
│   ├── cache/                  # 内存 LRU 缓存
│   ├── model/                  # 领域模型（Prompt/Score/Meta/ApiKey）
│   ├── catalog/              # 目录 hash、delta 变更集（原定名 sync，与标准库冲突已改）
│   ├── auth/                   # API Key 校验、HMAC 签名
│   ├── ratelimit/              # 限流器
│   ├── moderation/             # 审核队列、敏感词过滤
│   └── metrics/                # 内存指标（请求量、带宽、304 命中率）
├── pkg/
│   └── client/                 # SDK：网络、缓存、同步、评分（供 CLI/插件复用）
├── plugins/
│   ├── pi/                   # Pi 适配：extension/index.ts（工具+命令）+ skill/
│   └── dsh/                    # DSH 侧适配（Cordis 插件薄胶水，M4 已实现并真机验证）
├── web/                        # 前端：Astro + Preact 岛；产物由源站托管，CDN 前置缓存
├── internal/store/migrations/  # SQL 迁移（go:embed 内嵌，故无顶层 migrations/）
├── cmd/server 另有 -review/-approve/-reject/-backup/-put-key 运维子命令
├── deploy/                     # systemd unit、Dockerfile（可选）、备份脚本
├── scripts/                    # 构建、交叉编译、发布脚本
├── docs/                       # 本文档目录
├── go.mod / go.sum
└── Makefile 或 taskfile        # 常用命令（build/test/lint/migrate）
```

## 4. 代码规范

- **命名**：包名小写单词；导出标识符遵循 Go 惯例（`HTTP`、`ID`）；结构体字段见 `storage.md` 的 DDL 对应。
- **格式化**：一律 `gofmt`，禁止手工排版。
- **错误处理**：统一 `fmt.Errorf("xxx: %w", err)` 包装，不吞错、不 `panic`（仅 `main` 装配阶段允许）。
- **上下文**：HTTP handler 全程透传 `context.Context`，超时用 `context.WithTimeout`。
- **日志**：`log/slog` 结构化，见 `architecture.md` §5。
- **注释**：导出项必须有 godoc 注释。
- **依赖方向**：`internal/api → store/cache/sync/...`；**禁止反向依赖**；`pkg/client` 独立可发布，不得依赖 `internal`。

## 5. Git 分支与提交规范

- **分支**：`main`（可发布）+ `feat/*` + `fix/*` + `docs/*`。
- **提交**：Conventional Commits：
  - `feat:` 新功能、`fix:` 修复、`docs:` 文档、`refactor:` 重构、`test:` 测试、`chore:` 杂项。
- **PR 要求**：通过 `go vet` + `go test`；源站端点变更必须同步更新 `docs/api.md` 并标注兼容性。
- **契约变更流程**：任何 API 改动先在 `docs/api.md` 走“只增不改不删”评审，再实现。

## 6. 门禁（合并前必须通过）

> **命令清单的唯一权威是 [testing.md](./testing.md) §8**（及其 §10 实测结果）。
> 本节不抄命令与项数：同一个数字抄在多个文件里必然出现版本差，以权威处为准。

不成文的约束（只有这里说）：

1. 用 `make check` 一个入口，不要手工挑着跑；它**串通了 Go 与 TS 两侧**。
2. 缺 node / biome / staticcheck 时目标会**打印 SKIP 并继续** ——
   要求“必须跑全”时得自己断言工具存在，**SKIP 不等于通过**。
3. 新端点/新字段**必须**有对应契约测试（见 `testing.md` §4）。
4. 改了 `bench-core.ts` 任何一份副本，**必须双向同步**并更新两处 sha256 钉，
   否则 `make test-ts` 直接红。
5. 代码与文档矛盾时以代码为准，**并当场回写文档**（见本文件 §0）。

---

## 7. 领域名词定义

| 术语 | 定义 |
|------|------|
| 源站 | 部署 API 的服务器（香港，2H2G） |
| 目录 hash | 全体 approved 提示词的确定性摘要，用于 `delta` 增量同步 |
| 增量同步 | 客户端基于 `since=hash` 只拉差异，不拉全量 |
| PromptSummary | 不含正文的列表条目（省带宽） |
| deviceId / clientId | 分别用于打分去重 / 上传幂等的客户端指纹 |
| CDN | 托管前端静态资源的亚太 CDN，源站零前端回源 |