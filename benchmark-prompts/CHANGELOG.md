# 更新日志

本项目遵循 [Keep a Changelog](https://keepachangelog.com/zh-CN/1.1.0/)，
版本号遵循 [语义化版本](https://semver.org/lang/zh-CN/)。

版本号来源：`VERSION` 文件；在有 git tag 的环境里由 `git describe --tags` 覆盖。
`bench version` 与 `bench-server -version` 报告的即是这个值。

## [未发布]

### 新增

- **凭据自助注册（邀请码 + 设备绑定）**：使用者不再需要运维者手工 `-put-key`。
  - 运维侧：`-gen-invite "标签:次数:有效天数"` 签发邀请码（只存 sha256，明文只打印一次）、
    `-list-invites`、`-list-keys`、`-revoke-key <哈希前缀|name>`。
  - 服务端：`POST /v1/keys` 换 Key（一设备一 Key，重复申请 `409 conflict`；
    无效/停用/过期/用尽一律 `403 forbidden`，避免变成邀请码探针）、
    `GET /v1/keys/self`、`DELETE /v1/keys/self`（作废不可撤销）。
  - 客户端：`bench key new|self|revoke`（自动写入 bench 配置）、前端「连接设置」
    里的「申请 Key / 作废当前 Key」。
  - 校验顺序是**先验码、再查设备**：反序会让"码打错了"的用户收到
    "该设备已领过 Key"这种误导性回答；设备冲突时不消费名额。
- **Key 作用域（scope）**：`api_keys` 新增 `scope`/`device_id`/`invite_id`（迁移 0002）。
  自助注册的 Key 永远是 `writer`；`-put-key` 签发的是 `admin`。
  `/-/metrics` 改为 `adminRoute`——在此之前它只要求"有任意有效 Key"，
  **自助注册一上线就会变成公开可读**，这是本次改动必须同时做的安全收口。
  迁移里显式 `UPDATE ... SET scope='admin' WHERE device_id IS NULL`，
  否则列默认值会把既有运维 Key 静默降级（有专门的升级测试钉住）。

- **前端站点 `web/`（M5）**：Astro 纯静态输出 + Preact 岛，四个 hash 路由
  （列表 / 随机 / 详情 / 上传）+ 打分区 + 连接设置。产物由源站托管、CDN 缓存分发
  （见下条架构变更）；构建时报告真实体积（当前首屏 20.6 KB gzip）。用法见 `web/README.md`。
- **只读端点 `GET /v1/prompts/{id}/score`**：返回 `{id, avg, count}`，无人打分是
  `0/0` 而非 404。动机：旧契约下 `avg`/`count` 只出现在 `POST /v1/scores` 的响应里，
  前端「查看打分」在没有提交过的条数上什么也读不到。契约按「只增不改不删」
  追加为 `docs/api.md` §3.8。
- **门禁**：`make web-install` / `web-build` / `web-check` / `web-preview` / `web-size` /
  `smoke-web`（真源站 + 真产物 + 直接 import 浏览器那份 `api.ts`；项数见 docs/testing.md §10）。
  `make check` 现在包含前端静态与类型检查。

- **`deploy/` 部署资产**：`bench-server.service`（systemd 加固 + `EnvironmentFile`）、
  `bench-server.env.example`、`nginx-bench.conf`（TLS 终结 + 必带 `X-Forwarded-For`）、
  `bench-backup.sh` 与 `bench-backup.{service,timer}`（每日 `VACUUM INTO` 快照、
  原子改名、14 天保留、0600）。`docs/deployment.md` §2 重写为该形态的步骤与判据，
  并删除了早期内联的单元草稿（它与交付件在路径、单元名上都不一致，且缺
  `EnvironmentFile`，照抄会启动失败）。
- **CDN 回源网段适配**：`scripts/cdn-summarize.mjs` 把节点清单（818 个 IPv4/IPv6
  单地址）归约为 CIDR 网段，交付 `deploy/trusted-proxies.cdn.yaml`（234 条，
  **精确覆盖**语义 —— 覆盖地址数与输入去重数完全相等，不多信任任何未列出地址）。
  `--pad` 可切换为按 /24 与 /64 整段宽化以容忍节点轮换，代价由脚本如实报出
  （IPv4 信任面 691 → 28160，约 40 倍）。12 项 `scripts/cdn-summarize.test.mjs`
  把守，其中 IPv4 结果与 Python `ipaddress.collapse_addresses` 做过独立对账。
  `scripts/verify-linux.sh` 增加第三阶段：用真实片段配 234 条网段起服务，验证
  链上有 CDN 节点时继续左移到真实客户端 —— 对照实验（只留回环）确认它会报
  “把 CDN 节点 178.236.38.0 当成了客户端”。
  同时记录规模实测：234 条时每请求两次查找 2.4 µs（默认 2 条 0.47 µs），
  因此**有意不做 v4 快速路径**，基准测试留在仓库里。
- **`scripts/verify-linux.sh`**：用即将部署的 linux/amd64 ELF 在真实 Linux 上跑
  10 项端到端（API、三级缓存头、`/v1` 不被静态兜底吞掉、可信/不可信对端的 IP 采信）。
- `config.example.yaml` 补齐 `server.static_dir`（此前只有代码里有、样例缺）
  与 `server.trusted_proxies`，并把“生产改为 :443 并配证书”的过时建议改为
  “nginx 终结 TLS、源站只听 127.0.0.1:8080”。

### 安全

- **修复：`X-Forwarded-For` 被无条件采信**（`internal/api/clientip.go` 重写）。
  匿名限流的主体与访问日志的 `ip` 都取自客户端地址，而旧实现直接取请求头的**最左跳**，
  任何直连客户端自带 `X-Forwarded-For: 1.2.3.<随机>` 就能每次换一个新的限流身份 ——
  逐 IP 限流当场失效，审计日志同时被洗白。现在**只有直连对端落在
  `server.trusted_proxies` 内时才采信转发头**，采信时从右往左取第一个非可信跳。
  默认为仅回环（匹配“nginx 与源站同机”）；显式配置即整体替换。
  新增 `TestIPResolver`（12 条边界）与攻防用例
  `TestForgedForwardedHeaderCannotEvadeLimit`（关掉修复必红）及其反向钉
  `TestRealClientsBehindTrustedProxyAreSeparate`。

### 变更

- **本地 SQLite 缓存的 `journal_mode` 由 WAL 改为 DELETE**：单连接、短命、单进程的
  本地缓存拿不到 WAL 的并发收益，却会在运行期多出 `.db-wal`/`.db-shm` 侧文件。
  服务端仍用 WAL（那里确有服务进程与运维子命令并发）。
  这是一项**独立的配置决定，不是** Windows 那个 `t.TempDir` 抖动的修复 ——
  后者原因仍未定位（见下）。新增 `TestCacheJournalModeIsDelete` 钉住该配置，
  并用对照实验确认过它会红。
- **前端改为「源站托管 + CDN 缓存分发」**（取代原先的“零回源、前端不上源站”）：
  新增 `server.static_dir`，非空时源站注册 `GET /` 兜底并按三级 `Cache-Control`
  下发（`/_astro/*` immutable 一年、入口 `max-age=0, must-revalidate` + 弱 ETag、
  其余 300s），CDN 挂在前面透传这些头。留空仍是“只出 API”的形态。
  静态兜底**不会**吞掉 `/v1` 与 `/-`：未知 API 路径仍返回 `not_found` 信封，
  这条有契约测试，并用对照实验证明过它有诊断力。
  副作用：前端与 API 同域后浏览器不再发跨域请求，CORS 退为分域部署时的备用配置。
- **源站带宽套餐 2M → 10M**：`bandwidth.max_mbps` 默认值 1.6 → 8.0（= 10 Mbps × 0.8），
  监控告警与容量分级同步抬高。阈值的**唯一权威定义收敛到 `docs/deployment.md` §6**，
  其余文档与代码注释不再重复这个数字。
  ⚠️ **v0.1.0 已发布的二进制仍携带 1.6 默认值**（编译进 `internal/config`）；
  在用旧产物的部署里需显式写 `bandwidth.max_mbps: 8.0`，否则看门狗会在新套餐下
  提前降级。下一个版本发布后该差异自然消失。

### 修复

- **前端两处水合缺陷（真浏览器点按抓出，脚本门禁看不见）**：
  1. 刷新后「连接设置」里源站地址显示空白、面板不自动收起——预渲染读不到
     `localStorage`，而 Preact 水合不修补 DOM，界面在骗人（地址其实已生效）。
     改为挂载后一次性同步。
  2. 保存新地址后列表不重取——各视图只在挂载时读过一次配置。现在地址变更即重载，
     只改 Key 不重载。
- **表单字段缺 `name` / `autocomplete`** —— Chrome 在 console 报 4 处 issue；已补齐，
  两个凭据字段标 `autocomplete="off"`（API Key 不该被密码管理器接管）。修复后 console 零消息。
- **endpoint 写成带 `/v1` 的形式会换来一个含糊的 405**：SDK 自己会拼 `/v1/...`，
  而 `NormalizeEndpoint` 原先不剔尾部的 `/v1`，于是请求打到 `/v1/v1/prompts`；
  又因为源站注册了 `OPTIONS /` 兜底（路径匹上、方法匹不上），Go 返回的是纯文本
  "Method Not Allowed" 而不是契约信封，CLI 只能报 `bad_response`。
  现在两端一致：Go 的 `NormalizeEndpoint` 与前端 `normalizeBase()` 都会归一到源站根。
  这个坑的触发概率不低——`docs/api.md` §1 的 Base URL 示例就是带 `/v1` 的写法。
- **撤回一条错误的“修复”**：上一版把 Windows 上 `t.TempDir` 清理失败
  （`unlinkat …: The directory is not empty`）归因于“`clientFor` 的 7 个调用点
  从不 `Close()`”，并据此加了 7 行 `defer func() { _ = c.Close() }()`。
  复查提交历史发现那 7 处**本来就有** `defer c.Close()`，新加的是重复调用，
  对症状没有任何作用；本轮已删除这 7 行冗余。
  **该抖动的真实原因至今未定位。** 期间又犯了第二次同类错误：把它归因于
  “缓存开 WAL 留下 `.db-wal`/`.db-shm`”，写了个测试声称钉住这件事 —— 对照实验
  （换回 WAL）显示测试**照样通过**，因为 SQLite 干净关闭时会 checkpoint 并删掉
  侧文件，于是该解释被自己的实验否定，测试也改成了只钉配置选择。
  取证进展：成功复现过一次（`TestErrorExitCodesAndJSONShape`，与最初那个用例
  不同，说明不绑定具体测试），此后改前改后合计 34 轮 `-race` 不复现。
  下次再出现时的正确做法是先取证（哪个测试、清理时目录里还剩什么文件），
  而不是先猜一个原因再改代码。

> 说明：v0.1.0 已公开发布，**不移动已发布的 tag**（即使下载数为 0），
> 以保持“已发布二进制里嵌入的 commit 号 ≡ tag 目标”这个不变量。
> 本修复随下一版发布。

## v0.1.0 — 2026-09-03

首个可发布版本。源站、CLI 与两个 Agent 框架适配全部完成并通过真实验证。

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
- `--fresh` 语义为“排除最近抽过的”滚动窗口（上限 50）。

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
- `--fresh` 原实现排除“全部本地缓存”，`sync` 后必然 404。
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