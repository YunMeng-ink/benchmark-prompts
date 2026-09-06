# benchmark-prompts —— 个人测试用 benchmark 提示词平台

集**源站 API + `bench` CLI + Pi/DSH 双框架插件 + 发布工程**于一体的完整平台。
需求与方案属于私人笔记、不随仓库发布；开发文档见 [`docs/`](../docs/README.md)，
对外契约见 [`docs/api.md`](../docs/api.md)，发布记录见 [`CHANGELOG.md`](./CHANGELOG.md)。

**部署形态**：香港（HK）轻量服务器 2H2G + 10M 带宽 + 亚太 CDN（无备案）。
前端产物可由本服务托管（`server.static_dir`）并由 CDN 在其前面缓存；
不配 `static_dir` 时本服务只出 `/v1/*` JSON。

## 状态

| 模块 | 状态 |
|------|------|
| 11 个 API 端点 | ✅ 已实现，按 `docs/api.md` 契约测试通过 |
| 鉴权（Bearer Key + HMAC 签名） | ✅ 已实现 |
| 分级限流 / gzip / ETag 304 | ✅ 已实现 |
| 增量同步（catalog hash + delta） | ✅ 已实现 |
| 带宽看门狗 | ✅ 已实现（单元验证，待真实流量压测） |
| 审核队列与运维子命令 | ✅ 已实现 |
| SDK `pkg/client`（缓存/ETag/增量同步/重试/签名） | ✅ 已实现（M3） |
| `bench` CLI（meta/sync/get/random/list/score/upload/config/reset/version） | ✅ 已实现（M3） |
| Pi 适配（5 个工具 + 6 个斜杠命令 + 配套 skill） | ✅ 已实现并真实验证（M4） |
| DSH 适配 | ✅ **已实现并真实验证**（Cordis 插件：5 工具 + 6 命令 + skill 复用；`make smoke-dsh` 19/19，要点见 `plugins/dsh/README.md` 与 `../docs/handover-dsh.md`） |
| 凭据发放（邀请码自助注册 + 作用域隔离） | ✅ 已实现（`-gen-invite` 签发；`POST /v1/keys` 换 Key；自助 Key 只有 writer，读不到 `/-/metrics`） |
| 前端站点（列表/随机/详情/打分/上传/申请 Key） | ✅ 已实现并本地真验证（Astro 纯静态 + Preact 岛；`make smoke-web` 58/58，M5。上 CDN 属 M6） |
| 发布工程（版本注入 / 8 平台产物 / 校验值 / 产物验证） | ✅ 已实现（`make release`、`make release-verify`） |
| 前端 / 部署 | ⬜ 后续（M5、M6） |

验证结果：`go build` ✅ ｜ `go vet` ✅ ｜ `staticcheck` 零发现 ｜ `gofmt` ✅ ｜
`go test -race` **14/14 包通过** ｜
聚合覆盖率 **78.3%** ｜ `scripts/smoke.sh` **47/47** ｜ `scripts/smoke-cli.sh` **49/49** ｜
`node --test` **40/40**（pi bench-core 33 + dsh bench-core 副本 7）+ **23/23**（dsh plugin.test）｜
`make typecheck-dsh` ✅ ｜ `scripts/smoke-pi.sh` **12/12**、`scripts/smoke-dsh.sh` **19/19**（真实框架 + 真实 LLM 调用）、
`scripts/smoke-web.sh` **64/64**。

## 快速开始

```bash
# 1. 国内网络必须先切代理，否则 go mod tidy 拉不到依赖
go env -w GOPROXY=https://goproxy.cn,direct
go mod tidy

# 2. 门禁（fmt + vet + build + race 测试）
make check

# 3. 端到端冒烟（真实编译 + 真实 HTTP + 真实 SQLite）
bash scripts/smoke.sh

# 3b. CLI 二进制端到端冒烟（验证插件实际要调的东西）
make smoke-cli

# 3c. Pi 扩展真实验证（需已安装 pi 与模型凭据，否则自动 SKIP）
make test-ts && make smoke-pi

# 3d. DSH 插件真实验证（需 DSH 安装树与模型凭据，否则自动 SKIP）
make smoke-dsh

# 3e. 发布：全平台交叉编译 + 打包 + 校验值，然后验证产物
make release && make release-verify
make version                 # 看将要注入的 VERSION / COMMIT / DATE 与来源

# 4. 本地起服务（明文 HTTP，无需证书）
make dev            # 等价于 go run ./cmd/server -dev -config config.example.yaml
curl -s localhost:8080/v1/meta
```

## 目录

```
cmd/server/            服务入口 + 运维子命令
internal/
  cli/                 bench 子命令（逻辑在此以保证可测，cmd/cli 只有三行胶水）
  api/                 HTTP 层：路由、中间件、handler、信封、错误码
  store/               SQLite 仓储 + go:embed 迁移
  catalog/             目录 hash 与 delta（原名 sync，因与标准库冲突而改）
  cache/               进程内 LRU + TTL
  auth/                Bearer Key 与 HMAC 签名校验
  ratelimit/           固定窗口限流
  moderation/          敏感词过滤
  buildinfo/           版本/提交/构建时间的单一注入点（-X 写入，两二进制共用）
  metrics/             内存指标 + 带宽看门狗
  model/               领域结构与校验
  config/              YAML + 环境变量配置
  secretbox/           AES-GCM 加解密 HMAC secret
scripts/               check.sh / smoke*.sh / web-api-check.mjs / web-asset-graph.mjs
                     / web-size.sh / dsh-module-hook.mjs / dsh-typecheck.sh
plugins/
  pi/                  Pi 适配（extension/index.ts + bench-core.ts + skill）
  dsh/                 DSH 适配（Cordis 插件：index.ts + 同一份 bench-core.ts + 两层测试）
pkg/client/            公开 SDK：类型、错误码、签名、本地缓存、同步
web/                 前端站点：Astro 纯静态 + Preact 岛，产物由源站托管、CDN 缓存
deploy/                部署文件本体：systemd 单元、nginx 站点、备份脚本与 timer
```

## 运维子命令

同一个二进制，不需要额外的后台系统：

```bash
export BENCH_SECRET_KEY=<64位hex，即32字节主密钥>

bench-server -config c.yaml -put-key "alice:<plainKey>:<plainSecret>"
bench-server -config c.yaml -review                        # 待审核队列
bench-server -config c.yaml -approve p_1a2b3c4d            # 审核通过
bench-server -config c.yaml -reject  p_1a2b3c4d            # 审核打回
bench-server -config c.yaml -backup  /backup/bench.db      # 一致性备份
```

这些命令可与**运行中的服务共用同一个 DB 文件**（WAL + `busy_timeout=5000`）。
`-approve/-reject` 会递增 `version`，从而自动驱动 ETag 失效与 delta 下发。

## 实现要点（为什么这么写）

1. **只有 2 个直接依赖**：`modernc.org/sqlite`（纯 Go，无 CGO，交叉编译零成本）与
   `gopkg.in/yaml.v3`。路由用 Go 1.22 增强 `ServeMux`，LRU 自实现。
2. **列表不返回正文**：`/v1/prompts` 只回 `PromptSummary`（`id/t/v/h`），正文仅在
   `get`/`random`/`delta` 出现。这是带宽受限源站上最重要的取舍。
3. **`compress` 包在 `metrics` 之内**：`BytesOut` 统计到的才是真实压缩后字节，
   看门狗判断才准确。
4. **正文只 `TrimSpace`，不折叠内部空白**：benchmark 提示词常为多行，折叠会破坏内容；
   空白归一**仅用于计算 content_hash**（从而让排版不同的重复提交可被去重）。
5. **`updated_at >= since`**：同一秒内“登记快照”与“内容变更”可能同戳，用 `>` 会漏发；
   客户端 upsert/delete 幂等，宁可多返回。
6. **`deleted` 判定取 `NOT(approved|featured)`**：会把 pending 也列出（客户端本就不该
   持有，无害），换取“已公开条目被回退”这种严重场景不漏删。
7. **主密钥只来自环境变量**，`api_keys` 里 key 存 sha256、secret 存 AES-GCM 密文
   （不能存哈希，否则无法验签——这是复核阶段抓到的缺陷）。

## 待办

- [ ] 上 HK 服务器部署（systemd + TLS），见 `docs/deployment.md`
- [ ] 真实流量下校准带宽看门狗阈值（权威值见 `docs/deployment.md` §6）
- [x] M4：Pi 适配 ✅（12/12）+ DSH 适配 ✅（19/19）—— 调研、实现、真机验证均完成
- [x] M5：前端站点已实现并本地验证（`make web-build` / `make smoke-web` 45/45）
- [ ] 前端上 CDN（含两组缓存头与 CORS 白名单配置，属 M6）
- [ ] 审核动作目前是命令行，量大时需补一个受保护的管理端点
- [ ] CLI 分发：三平台二进制 + 一键安装脚本（`make build-all` 已就绪）

## 已知限制

- **Windows 无 POSIX 权限位**：配置写入请求 0600 并显式 `Chmod`（`os.WriteFile`
  对已存在文件不会改权限），但 Windows 只映射只读位，真实隔离靠用户目录 ACL。
  因此 `Chmod` 失败**不当致命错误**，否则 `config init` 在 Windows 直接不可用。
- `cmd/*` 本身 0% 覆盖（main 只做装配），由两个冒烟脚本间接验证。
- 上传幂等只在显式传 `--client-id` 时才能真正重放安全；默认随机 clientId 配合
  “写请求不重试”策略，避免重试造成重复入库。