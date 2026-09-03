# 更新日志

本项目遵循 [Keep a Changelog](https://keepachangelog.com/zh-CN/1.1.0/)，
版本号遵循 [语义化版本](https://semver.org/lang/zh-CN/)。

版本号来源：`VERSION` 文件；在有 git tag 的环境里由 `git describe --tags` 覆盖。
`bench version` 与 `bench-server -version` 报告的即是这个值。

## [未发布]

### 修复

- **`bench` CLI 不关闭 SQLite 缓存句柄** —— `clientFor` 的 7 个调用点均未 `Close()`。
  对 Linux/macOS 用户不可见（进程退出时 OS 回收），但在 Windows 上未关的句柄会
  阻塞临时目录删除，导致测试集偶发失败且**失败包在 `internal/cli` 与 `pkg/client`
  之间漂移**，看上去像“随机不稳”。现在每个客户端都在命令返回时 `defer c.Close()`。
  连跑 3 轮全量 `go test -race ./...` 稳定 14/14。

> 说明：v0.1.0 已公开发布，**不移动已发布的 tag**（即使下载数为 0），
> 以保持“已发布二进制里嵌入的 commit 号 ≡ tag 目标”这个不变量。
> 本修复随下一版发布。

## v0.1.0 — 2026-09-03

首个可发布版本。后端、CLI 与两个 Agent 框架适配全部完成并通过真实验证。

### 新增

**源站 API（Go + SQLite，仅 2 个直接依赖）**

- 7 个端点：`/v1/meta`、`/v1/prompts`、`/v1/prompts/random`、`/v1/prompts/{id}`、
  `/v1/delta`、`/v1/score`、`/v1/upload`，契约见 `docs/api.md`（冻结）。
- Bearer + HMAC-SHA256 双模鉴权；Key 密文存储（AES-GCM，HMAC 需可逆）。
- 进程内 LRU 缓存、ETag/304、gzip、接口级限流、**2M 带宽看门狗**（超阈值自动降级）。
- 上传必过审核队列；运维子命令 `-review / -approve / -reject / -backup / -put-key`。
- 目录快照表 `catalog_snapshots`，支撑增量同步与回退全量。

**`bench` CLI（插件的唯一入口）**

- `meta / sync / get / random / list / score / upload / config / reset / version`。
- 统一 `--json` 信封 `{ok,data,error,v}`，成功与失败同形（v0.1.0 内做过一次
  非对称→对称的统一，属破坏性收敛，故为首版）。
- 退出码 `0/1/2/3/4/5` 与错误码一一对应；`canceled`、`local_error` 为 CLI 本地分类。
- SQLite 本地缓存 + 增量同步 + 离线 `--local` 路径；写请求不重试。
- `--fresh` 语义为"排除最近抽过的"滚动窗口（上限 50）。

**Agent 框架适配（薄胶水，零业务逻辑）**

- **Pi**（`plugins/pi/`）：5 工具 + 6 斜杠命令 + skill。验证 12/12。
- **DSH / DeepSeek Harness**（`plugins/dsh/`）：Cordis 插件，5 工具 + 6 命令 +
  `ctx.subprocess` 桥。真 headless DSH + 真 LLM 验证 19/19。
- 两框架共用同一份 `SKILL.md`（零改动），与同一份 `bench-core.ts` 纯逻辑
  （副本 + sha256 哈希钉防漂移）。

**发布工程**

- `make release`：bench 5 平台 + server 3 平台交叉编译 → tar.gz → `sha256sums.txt`
  + `RELEASE-INFO`。
- `make release-verify`：验证产物（字节级 `-X` 注入证据 + 校验值 + 归档结构 + 本机真跑）。
- `internal/buildinfo`：版本/提交/构建时间单一注入点，`bench version` 与
  `bench-server -version` 共用。

**测试与门禁**

- Go：14 包 `-race` 全绿；`staticcheck` 零发现。
- TS：63 项（pi 33 + dsh core 7 + dsh 插件层 23）。
- 冒烟：`smoke.sh` 45/45、`smoke-cli.sh` 35/35、`smoke-pi.sh` 12/12、`smoke-dsh.sh` 19/19。
- `scripts/capture-contract.sh` 采集 `bench --json` 真实输出作为契约地面数据。

### 修复（开发过程中被测试抓出的真实缺陷）

- `compressMW` 未接入中间件链 —— gzip 从未生效（2M 带宽方案第一要件）。
- 同步翻页时把新 `since` 回喂服务端 —— 第二页起静默丢数据，不报错。
- `--fresh` 原实现排除"全部本地缓存"，`sync` 后必然 404。
- 插件探测命令用 `bench --json` 被当成未知命令，正常二进制被误判为版本过旧。
- `assertScoreValue` 用 `parseInt` 把 `3.5` 静默截成 `3`，把无效入参变成真实一票。
- `flag` 包静默丢弃位置参数之后的选项（`config init --home X` 里 `--home` 被忽略）。
- `os.WriteFile` 的权限位仅在创建时生效，已存在的凭据文件不会被收紧。
- `CreatePendingPrompt` 只在 handler 里 trim，任何新写入方可绕过 —— 已下沉到入库边界。
- `PruneSnapshots` 声明了参数却漏传 SQL —— 运行必崩。
- 构建产物不带版本注入，全部报 `dev`（本次发布工程补齐）。

### 已知限制

- Pi 扩展入口无法本地类型检查（Scoop 发行版不带 `dist/*.d.ts`）；DSH 侧相反，
  `make typecheck-dsh` 可做真类型检查。
- 带宽看门狗阈值（`max_mbps: 1.6`）只做过单元验证，未经真实流量校准。
- Windows 无 POSIX 权限位，配置文件 0600 仅映射只读位，真实隔离依赖用户目录 ACL。
- `web/`（M5）与 `deploy/`（M6）目录尚未创建；审核目前是命令行操作。
- `golangci-lint`、`govulncheck` 本机不可用，未纳入门禁。