# 测试策略与验收标准

## 1. 测试金字塔

```
        ▲ 端到端（少量）
       / \
      /   \ 契约测试（api.md 为主）
     /     \
    / 集成  \（store/sync/cache + httptest）
   /_________\
  单元测试（大量，纯函数与校验逻辑）
```

## 2. 单元测试（`go test`）

| 目标 | 用例 |
|------|------|
| `model` 校验 | 正文长度、标签格式、value 范围 |
| `sync.CatalogHash` | 确定性：同数据同 hash；改 tags/正文后 hash 变化 |
| `sync.Delta` | since 命中→只回差集；since 未知→全量 |
| `auth.Verify` | 正确签名通过；篡改 body/超时/错 secret 拒绝 |
| `ratelimit` | 窗口内超限被拒；窗口滑过后恢复 |
| `moderation` | 敏感词命中、长度越界 |

## 3. 集成测试

| 目标 | 用例 |
|------|------|
| `store` | CRUD、分页 cursor、随机排除 exclude、upsert 评分幂等、上传 clientId 幂等 |
| 缓存一致性 | 写后失效对应项，目录 hash 更新 |
| 中间件链 | 压缩产出 gzip、ETag→304、429 限流响应 |

## 4. 契约测试（关键，贯穿 `docs/api.md`）

对每个端点断言：**状态码 + 信封结构 + error.code**。

```go
// 例：GET /v1/prompts/random
func TestContractRandom(t *testing.T) {
    rec := doRequest("GET", "/v1/prompts/random", nil)
    assert 200
    assert body.ok == true
    assert data.id 存在 && data.p 非空 && data.h 存在
}
```

重点覆盖：
- 信封字段名与 `docs/api.md` 完全一致（`ok/data/error/cursor/v`、字段短名 `p/t/v/s/h`）。
- 错误码映射表（400/401/403/404/409/413/422/429/500/503）。
- `delta` 的全量/增量/`has_more` 翻页语义。
- `meta`/`get` 的 `If-None-Match → 304`。

> **规则**：任何 `api.md` 变更，先写（或改）对应契约测试，再改实现（TDD）。

## 5. 端到端（CLI 视角）

用真实 `bench` 打真实服务（本地起 server + 临时 DB）：

1. `bench config init` → 生成 `device_id`。
2. `bench upload -f sample.txt` → 得到 `pending` id（需先人工置为 approved 或走审核接口）。
3. `bench sync` → 本地缓存就绪。
4. `bench random` → 拿到正文。
5. `bench get <id>` → 同一正文。
6. `bench score <id> 5` → 返回 avg/count。
7. 重复 `bench sync` → 第二次无变化（增量空）不报错。

## 6. 插件验收

见 `plugins.md` §7 —— 核心是“一句自然语言能触发一键/随机测试，且结果进入框架上下文”。

## 7. 前端手工验收

- 列表分页、标签筛选、随机、详情打分、上传表单。
- 断网只靠 CDN 缓存能见首屏骨架。
- 429/5xx 有重试提示，不白屏。

## 8. 发布门禁（实际可执行版）

不再是“概念示意”——下面每一项都在本机真实跑过：

```bash
# 一条命令跑完开发门禁（Go + TS + 前端）
make check

# 它展开为：
gofmt -l .                                              # 格式
# 或 go fmt ./...（自动修）
go vet ./...
staticcheck ./...                                       # 2026.2.1，当前零发现
go build -trimpath ./cmd/server ./cmd/cli
make build-cli
go test -race -count=1 ./...                            # 14 包
biome check plugins/pi/extension/ plugins/dsh/ web/src/ # 17 文件，0 error 0 warning
astro check                                          # 前端 0 errors（make web-check）
node --test plugins/pi/extension/bench-core.test.ts             # 33
node --test plugins/dsh/bench-core.test.ts                      # 7（含哈希钉）
node --import ./scripts/dsh-module-hook.mjs --test plugins/dsh/plugin.test.ts   # 23
```

**门禁之外必跑**（`make check` 不涵盖）：

```bash
make smoke            # 47 项，服务端真实 HTTP + 真实 SQLite
make smoke-cli        # 35 项，真实 bench 二进制
make typecheck-dsh     # tsc noEmit 对 DSH 真实 .d.ts
make release           # 全平台交叉编译 + 打包 + sha256sums + RELEASE-INFO
make release-verify    # 验证发布产物
make smoke-pi         # 12 项，真实 pi + 真实 LLM（无凭据则 SKIP）
make smoke-dsh        # 19 项，真 DSH headless + 真 LLM（无安装树则 SKIP）
make smoke-web        # 45 项，前端真源站 + 真产物 + 浏览器那份 api.ts
make contract         # 重新采集 bench --json 地面数据
```

产物矩阵、发布流程与用户侧校验见 `deployment.md` §1（唯一权威，此处不重复）。

未纳入且**本机不可用**的可选工具（已核实，不要假设它们在）：
`golangci-lint`、`govulncheck`。后者建议上服务器/CI 后补做（直接依赖只有
`modernc.org/sqlite` 与 `gopkg.in/yaml.v3` 两个，攻击面小）。

## 9. 验收标准（MVP 就绪定义）

- [x] 8 个端点全部按 `api.md` 契约通过
- [x] gzip + ETag/304 + delta 增量生效
- [x] 限流 + 带宽看门狗生效
- [x] 备份 + 审核运维链路就绪
- [x] CLI 子命令可用且 `--json` / 退出码构成稳定契约（M3）
- [x] Pi 可一句话触发一键/随机测试（M4，真实 pi 验证）
- [x] DSH 适配（M4：真 DSH headless + 真 LLM 验证 19/19）
- [ ] 前端静态上 CDN，源站不服务前端（M5）

## 10. 实测结果（M2–M4 + 发布工程，已跑通）

| 检查 | 命令 | 结果 |
|------|------|------|
| 编译 | `go build ./...` | ✅ 通过（修 1 个真实 bug：`PruneSnapshots` 漏传 SQL 参数） |
| 静态检查 | `go vet ./...` | ✅ 无告警 |
| 深度静态检查 | `staticcheck ./...`（2026.2.1） | ✅ **零发现**（M4 后才纳入 `make check`；M2–M4 期间漏跑） |
| Go 格式 | `gofmt -l .` | ✅ 无输出 |
| Go 单测+契约 | `go test -race -count=1 ./...` | ✅ **14/14 包通过**，无 DATA RACE |
| 聚合覆盖率 | `go test -coverpkg=./...` | **78.3%**（cache/model/ratelimit 100%，auth 91.5%，catalog 87%，internal/cli 84.8%） |
| TS lint/格式 | `biome check plugins/pi/extension/ plugins/dsh/` | ✅ **7 个文件** 0 error 0 warning（120 列 + tab，对齐 Pi 官方示例风格） |
| TS 单测 | `make test-ts`（node --test） | ✅ **63 项**：Pi 侧 33（含 3 项真实 bench 集成、2 项 skill 结构校验）+ dsh bench-core 7（含哈希钉）+ **dsh 插件层 23**（假 ctx + 真 `defineTool`） |
| DSH 类型检查 | `make typecheck-dsh` | ✅ `tsc noEmit` 对着 DSH 真实 `.d.ts`（DSH 包附 `src/`，可类型检查 —— pi 入口做不到） |
| 服务端端到端 | `bash scripts/smoke.sh` | ✅ **47/47** |
| CLI 二进制端到端 | `bash scripts/smoke-cli.sh` | ✅ **35/35** |
| Pi 扩展真实验证 | `bash scripts/smoke-pi.sh` | ✅ **12/12**（真实 pi 加载 + 真实 LLM 调用工具 + 失败分支） |
| DSH 插件真实验证 | `bash scripts/smoke-dsh.sh` | ✅ **19/19**（headless 真装载 + 真 LLM + HMAC 写路径 + 四类失败分支 + skill 发现） |
| 发布产物验证 | `make release && make release-verify` | ✅ **20 通过 / 0 失败 / 1 诚实 SKIP**（Windows 不在 server 矩阵） |
| 验证器自身的对照实验 | 抽掉 `-X` 重建一个产物后重跑 | ✅ 只对被改的那一个报 FAIL —— 检查有诊断力，不是永真机器 |
| 一键入口 | `make check` | ✅ Go + TS 门禁串通 |

各冒烟脚本覆盖的真实链路（**均为真实进程与真实 HTTP，无 mock**）：

- **smoke.sh**：启动服务 → 登记 Key → 上传 → 跨进程审核 → 列表/随机/详情 → ETag 304 →
  gzip 魔术字节校验 → delta 增量/回退全量/下架回收 → 评分幂等覆盖 → 错误码 → 指标 → 备份。
- **smoke-cli.sh**：编译出真实 `bench` 二进制，以**插件将要使用的同一方式**
  （shell 调用 + `--json` + 退出码）跑全流程，含源站下线后的 `--local` 离线路径。
- **smoke-pi.sh**：先跑**对照实验**（故意加载坏扩展，确认 pi 会报
  `Failed to load extension`，否则“没报错”不构成证据），再加载本项目扩展，
  最后让真实 LLM 调用 `bench_random` / `bench_catalog`，断言取回的是源站上那条
  带唯一标记的提示词（而不是模型编造的文本）。pi 或模型凭据缺失时自动 SKIP。
  用 `--tools` 白名单堵死“模型自己跑 bench”的旁路（该白名单作用于内置+扩展工具）。
- **smoke-dsh.sh**：同构且更严 —— A 前置 → B 单元/tsc → C 对照实验（DSH 必须报
  `failed to import/apply loader entry`）→ D 真 LLM 调用 → E 四类失败分支 → F skill 发现。
  测试 patch 把 `tool-pwsh`/`tool-fs`/`tool-web` 等一切旁路 `disabled: true`
  ——**第一轮实测就靠这条抓到了“模型自己跑 CLI 取数据”的假通过**。

### 被测试抓出的真实缺陷（按严重度）

1. **`compressMW` 未接入中间件链** —— gzip 从未生效。带宽方案的第一要件，
   仅靠写文档发现不了，由 `TestContractGzipRoundTrip` 抓出。
2. **同步伪代码自带丢数据 bug**（`docs/client.md` §4 原稿）—— `hash = d.since`
   写在翻页循环内，第二页会把新 hash 回喂服务端 → 服务端判定“已最新”返回空集 →
   只同步到第一页。不报错不崩溃，只是默默丢数据。回归测试
   `TestEndToEndSyncKeepsSinceFixedAcrossPages`（107 条跨页）。
3. **`--fresh` 语义错误** —— 原实现排除“全部本地缓存 id”，而 `bench sync` 会把
   整个目录灌进缓存，等于永远 404。改为排除“最近抽过”的滚动窗口（上限 50）。
   由真实 Pi 调用抓出。
4. **探测命令写成 `bench --json`** —— bench 把第一个位置当子命令名，`--json` 被
   当未知命令退 5，导致正常二进制被误判为“版本过旧”。应为 `bench version --json`。
5. **`flag` 包静默丢弃位置参数之后的选项** —— `bench config init --home X` 里
   `--home` 被悄悄忽略且不报错。已加 `splitArgs` 分拢。
6. **`CreatePendingPrompt` 未做 trim** —— 裁剪只在 handler，任何新写入方都会绕过。
   已下沉到入库边界。
7. **`os.WriteFile` 的 perm 仅创建时生效** —— 含凭据的配置若已存在不会被收紧权限。
   已补显式 `Chmod`。
8. **`PruneSnapshots` 声明了 `cutoff` 却漏传 SQL 参数** —— 运行必崩，编译器抓出。
9. **测试自身的假阳性** —— `TestSDKSignatureAcceptedByServerVerifier`
   首版用被篡改的 body **重新签名**，那是个自洽的合法签名，服务端接受完全正确，
   测试会以“防篡改已验证”的假象通过。改为“沿用旧签名 + 偷换 payload”后才是真实攻击场景。
10. **LLM 探针词被拒绝语引用 → 断言顺序缺陷** —— `smoke-dsh.sh` 先用
    `grep TOOL-OK` 判失败、再查错误标记。模型完全正确地拒绝了（“因此不能回答 TOOL-OK”），
    却因引用了探针词而被判为“把错误当成成功”。**插件无错，测试错了**。
    改为**先查错误标记、再查探针词**（仍保留诊断力：真发生了“错误被吞”，
    模型手里就没有错误文本可引）。同类断言已同步补进 Pi 侧。
11. **CLI 不关 SQLite 缓存句柄**（发布阶段才抓出）—— 只在 Windows 上表现为
    `t.TempDir` 清理失败，Linux/macOS 完全看不出来。已给 7 个 `clientFor`
    调用点补 `defer c.Close()`。
12. **`go version -m` 读不到 `-ldflags`** —— 原计划用 build info 验证跨平台产物的
    版本注入，实测 go1.27 只记 `-buildmode`/`-compiler`/`-trimpath`。改用构建时间戳
    做字节级证据，并对照实验证明该检查有诊断力。
13. **`pipefail` + `grep -q` 的 SIGPIPE 陷阱** —— `tar -tzf | grep -q` 里 grep 提前关
    管道会让 tar 收到 EPIPE，整条管线返回 141，**归档内容完全正确也会被判失败**。
    同类判断改成“先取进变量再模式匹配”。
——DSH 侧第一轮实测就抓到了这条路。修正：测试环境必须消除旁路
（DSH：patch 里 `disabled: true`；pi：`--tools` 白名单）。

### 已知限制（不阻塞）

- `cmd/*` 本身 0% 覆盖（main 只做装配），由三个冒烟脚本间接验证。
- **Pi 扩展入口 `index.ts` 无法本地类型检查**：Scoop 发行版未附 `dist/*.d.ts`，
  而 pi 用 jiti 加载本身也不做类型检查。当前防线是 `make smoke-pi` 的对照实验 +
  真实工具调用；这也是把易错逻辑抽到零依赖的 `bench-core.ts` 单独测试的原因。
- 真实带宽看门狗降级仅单元验证，未做压测（需上服务器用真实流量观察）。
- Windows 无 POSIX 权限位：配置文件的 0600 在 Windows 上仅映射只读位，
  真实隔离依赖用户目录 ACL；因此 `Chmod` 失败不当致命错误。
- **两份 `bench-core.ts` 是副本关系**（pi 与 dsh 各一份，因装载机制不同无法共享）。
  漂移风险由 **sha256 哈希钉测试** 把守（两测试文件各钉一份，改动必须双向同步
  后一起更新钉值），不依赖仓库布局。这是当前防止两框架行为分叉的唯一机制。
- 曾观察到一次 `-race` 下只数到 12/13 包，当时**猜**是 httptest 随机端口偶发争用。
  那个猜测是错的。后续在发布阶段重新复现并拿到了真实原因：
  `--- FAIL: TestRandomFreshOnlyExcludesRecentlyDrawn` 本身通过，
  挂在 `t.TempDir` 的清理上 —— `unlinkat …: The directory is not empty`。
  **根因是 CLI 创建了带 SQLite 缓存的 client 却从不 `Close()`**：Windows 不允许
  删除仍被句柄占用的文件，于是清理失败；失败包会在 `internal/cli` 与 `pkg/client`
  之间飘，单独跑又总过 —— 典型的资源泄漏伪装成“随机不稳”。
  已修：`clientFor` 的 7 个调用点均 `defer c.Close()`，连跑 3 轮全量 `-race` 稳定 14/14。
  **结论：“复现不了就先记一笔偶然”会把真 bug 合理化为噪声** —— 偶发失败必须多轮复跑定位，不能归因于环境。
