# dsh-bench —— benchmark 提示词的 DSH（DeepSeek Harness）插件

 DSH 侧薄适配：**零业务逻辑**，一切能力经 `bench` CLI。

| 能力 | 工具（LLM 调用） | 命令（用户直发） |
|---|---|---|
| 随机测试 | `bench_random(tag?, fresh?)` | `/bench-random [tag]` |
| 一键测试 | `bench_get(id, local?)` | `/bench-get <id>` |
| 打分 | `bench_score(id, value)` | `/bench-score <id> <1-5>` |
| 浏览/同步/状态 | `bench_catalog(action=list\|sync\|status)` | `/bench-list [tag]` `/bench-sync` `/bench-status` |
| 上传 | `bench_upload(content, tags?, client_id?)` | — |
| 流程知识 | `plugins/pi/skill/benchmark-testing/SKILL.md`（零改动共用，见下） | |

## 前置

```bash
make build-cli                                   # 产出 dist/bench(.exe)
dist/bench config init --endpoint <源站> --key <K> --secret <S>   # 读操作可省 key/secret
```

打分/上传需要凭据；只读匿名可用。

## 装载

### A. 临时试跑 / CI —— `--patch` 覆盖层（不装任何东西）

把本目录**复制进 profile 树**（必须！插件的 `@deepseek-ai/*` 裸导入靠 Node ESM
沿目录上溯到 `~/.dsh/profiles/node_modules` 解析，仓库原位没有这些包），然后：

```bash
mkdir -p ~/.dsh/profiles/web/plugins
cp plugins/dsh/index.ts plugins/dsh/bench-core.ts ~/.dsh/profiles/web/plugins/
dsh --profile web --patch 'D:\绝对路径\patch.yml'
```

```yaml
# patch.yml 放哪都行；但 insert 里若用相对 name，它以 **patch 文件自身目录** 为基准锚定。
# 最稳的写法是绝对路径 + 扩展名（Node ESM 不做 index 推断、不补扩展名）。
- insert:
    - id: bench
      name: 'C:\Users\<you>\.dsh\profiles\web\plugins\dsh\index.ts'
      config:
        bin: 'D:\...\dist\bench.exe'
        home: 'D:\...\bench-home'
```

### B. 长期安装 —— `dsh plugin add`

```bash
dsh plugin --profile web add file:<仓库根>/benchmark-prompts/plugins/dsh
```

包内 `package.json` 声明了 `dsh.bundle.patch → cordis.patch.yml`，
安装即自动追加一层 `- insert: [{id: bench, name: './index.ts'}]`
（bundle 层的相对 name 锚定在**包目录**里，pnpm 复制安装后依然成立）。
pnpm `file:` 是**复制**语义：改了源码要重新 `add` 一次（日常开发走路线 C）。

### C. 开发回路（改文件重启即生效）

在 profile 目录挂一个 **junction** 指回仓库：

```bat
mklink /J %USERPROFILE%\.dsh\profiles\web\plugins\dsh <仓库根 Windows 路径>\benchmark-prompts\plugins\dsh
```

再在 `~/.dsh/profiles/web/cordis.patch.yml` 里放路线 A 的 `- insert:` 块
（name 写 junction 的绝对路径）。改代码 → **重启 DSH**。

> 相对路径**符号链接**（`ln -s`/`mklink` 不带 `/J`）不要用：Node ESM 默认 realpath，
> 裸导入会解析回仓库原位而找不到框架包。junction（reparse point）无此问题。
> profile 的 `patchReload: live` 只热更 patch 层，不热更已缓存的模块代码。

## 插件配置（`Config`，全部可留默认）

| 键 | 默认 | 说明 |
|---|---|---|
| `bin` | `''` | bench 路径；空 = `BENCH_BIN` 环境变量 > PATH 上的 `bench` |
| `home` | `''` | 透传 bench `--home`（配置与缓存目录） |
| `endpoint` | `''` | 透传 bench `--endpoint`（覆盖配置文件的服务地址） |
| `timeoutMs` | `60000` | 单次 bench 子进程超时预算 |
| `graceMs` | `3000` | terminate 升级宽限 |

**环境净化警告**：DSH 给子进程的环境会剥掉匹配
`/KEY|PASSWORD|SECRET|TOKEN/i` 的名字与全部 `DSH_*`（`dsh-subprocess/src/index.ts`）。
也就是说 `BENCH_API_KEY` / `BENCH_SECRET` **不会**透传给 bench，即便你在启动 dsh 的
shell 里设过。`BENCH_HOME` / `BENCH_ENDPOINT` 不在剥离名单、可透传，但**推荐一律显式
配 `home`/`endpoint`**（或 `bench config init --home DIR` 写好配置文件）。

## skill（零代码）

```bash
cp -r plugins/pi/skill/benchmark-testing ~/.dsh/skills/
```

DSH 与 pi 读同一套 `SKILL.md` 格式（name + description 两个 frontmatter 键即可双框架通用）。
也可不动用户目录，在 patch 里给 `skill-filesystem` 配
`customSkillDirs: ['D:/.../plugins/pi/skill']`。
**勿加** `allowed-tools` 等 Claude Code 旧字段——DSH 遇到旧字段名直接抛错。

## 文件与“两份 bench-core”的纪律

```
index.ts            Cordis 插件入口：defineTool ×5 + commands ×6 + subprocess 桥
bench-core.ts       ★ 从 plugins/pi/extension/bench-core.ts 逐字节复制（同一份纯逻辑）
package.json        bundle 声明（路线 B 用）
cordis.patch.yml    bundle patch 层（路线 B 用）
bench-core.test.ts  哈希钉 + 行为冒烟（不依赖 DSH 安装树）
plugin.test.ts      假 ctx + 真 defineTool 的框架层功能测试（23 项）
```

`bench-core.ts` 与 Pi 侧**改动必须双向同步**后，一起更新两处钉住的 sha256
（`bench-core.test.ts`、`plugin.test.ts`）。
本次适配已在两份里同步修掉一个类型缺陷（`as typeof envelope` 自指引出 `never`；
tsc 检查逮到的，纯类型改动、运行时行为不变）。

## 验证

```bash
make test-ts        # bench-core（pi + dsh 双份）+（有 DSH 安装树时）plugin.test.ts
make typecheck-dsh  # tsc noEmit 对着 DSH 真实 .d.ts（缺 @types/node 自动降级 minimal 模式）
make smoke-dsh      # 真装载：对照实验 + 真 LLM 调用 + 失败分支 + skill 发现
```

## 已知限制

- `/bench-*` 命令在无头（headless）形态没有派发入口——命令是 UI 面能力，web/tui
  表层自然可用；headless 下模型走工具路径即可（能力等价）。
- 工具结果超 50KB 会被 DSH 全局 spill 策略换成预览+落盘引用（`dsh-spill-policy`）。
  本插件自身已对 list/正文截断（120 行/12KB），不依赖该兜底。
- 装载副本（路线 A/C 的临时目录）里没有 `plugins/pi/` 兄弟目录时，
  `bench-core.test.ts` 的逐字节比对项静默跳过——哈希钉仍生效，漂移防线不依赖布局。
- 无头探测插件加载失败会**硬失败整个启动**（fiber 失败沿 root ready 上抛）。
  这正是对照实验可用的原因，但也意味着坏 patch 会让 profile 起不来——手改
  `cordis.patch.yml` 出错时用 `--dump-config` 先自检。
