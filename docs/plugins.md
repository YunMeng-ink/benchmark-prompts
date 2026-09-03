# DSH / Pi 插件适配规格

> 目标：让 DSH 与 Pi 两个 Agent 框架都能「一键测试」「随机测试」。核心约定 —— **插件只做薄胶水，一律调 `bench` CLI**。

## 1. 统一交互契约（两框架一致）

> `bench` CLI 已实现（M3）。以下命令、参数与退出码已是**真实可用行为**，
> 由 `scripts/smoke-cli.sh` 以真实二进制验证过 35 项。
> 构建：`make build-cli` → `dist/bench`（Windows 为 `dist/bench.exe`）。

| 用户意图 | 插件行为 | 底层命令 |
|----------|----------|----------|
| 一键测试（指定某条） | 列出/接受 id → 拉取正文 → 回填到会话 | `bench get <id> --json` |
| 随机测试 | 随机拉一条 → 回填 | `bench random [--tag=x] [--fresh] --json` |
| 不重复抽同一 | 排除本地已见过的 | `bench random --fresh --json` |
| 打分 | 对刚测的提示词打分 | `bench score <id> <1-5> --json` |
| 浏览/搜索 | 分页/按标签浏览（不含正文） | `bench list [--tag=x] [--all] --json` |
| 同步 | 手动刷新本地缓存 | `bench sync --json` |
| 离线取回 | 不访问网络 | `bench get <id> --local --json` |
| 上传 | 进审核队列 | `bench upload -c 正文 -t a,b --client-id=… --json` |
| 状态 | 本地是否落后 | `bench meta --json` |

**插件的硬性要求：**
1. **必须用 `--json`** 解析结构化输出（不解析人类可读文本）。
   `--json` 时**连错误也走 stdout** 的 `{"ok":false,"error":{"code":…}}`，
   不需要去混流的 stderr 里抓错。
2. 读退出码做错误分支（稳定契约，见 `docs/client.md` §11）：
   `0` 成功 / `1` 网络或服务端 / `2` 限流（可重试）/ `3` 鉴权 / `4` 不存在 / `5` 参数或校验
3. 不实现任何网络/缓存/评分逻辑 —— 全部托 `bench`。
4. 元信息（`# id v3 tags=…`）走 stderr、正文走 stdout；若插件自己拼管道，
   用 `--quiet` 可抑制 stderr 噪声。

## 2. 插件前置：确保 `bench` 可用

```
优先级：BENCH_BIN 环境变量 > PATH 上的 bench
首次用到时懒探测（跑 `bench version --json`），失败不缓存，用户装好后无需重启
不可用 → 报 bench_missing，文案里直接给出 make build-cli 与 BENCH_BIN 两个出路
```

两个踩过的坑：
- **探测命令不能是 `bench --json`**：bench 把第一个位置当子命令名，`--json`
  会被当成未知命令退 5，于是把完全正常的二进制误判为“版本过旧”。
  必须是 `bench version --json`。
- **不在插件初始化时探测**：pi 文档明确警告工厂函数可能在“永不开会话”的
  调用里执行，那时不该有进程/IO 副作用。

## 3. Pi 适配（已实现）

> ⚠️ **本节早期版本曾写过一个 `tools:` YAML 配置形（带 `handler: run:` 字段），
> 那是凭经验臆造的 schema，pi 实际不存在这种声明式配置。** 以下为读完
> `docs/extensions.md`、`docs/skills.md` 与官方示例后核实的真实形态。

Pi 的扩展是 **TypeScript 文件**，由 jiti 直接加载（无需编译），
默认导出一个接收 `ExtensionAPI` 的工厂函数。因此：

- SDK 是 Go 的，Pi 扩展**无法直接 import `pkg/client`**；只能走 `bench` CLI。
  这反过来印证了“能力下沉 CLI”的必要性——它是唯一能跳语言复用的边界。
- 执行命令用 `pi.exec(bin, args, { signal, timeout })` → `{ stdout, stderr, code, killed }`。

### 目录

```
plugins/pi/
├── extension/
│   ├── index.ts             5 个工具 + 6 个斜杠命令
│   ├── bench-core.ts        纯逻辑（不依赖 pi，可被 node --test 驱动）
│   └── bench-core.test.ts   33 项（含对真实 bench 二进制的集成测试）
└── skill/benchmark-testing/SKILL.md
```

安装位置（任选）：`~/.pi/agent/extensions/bench/`（全局）、
`.pi/extensions/bench/`（项目）、或 `pi -e ./plugins/pi/extension/index.ts`（临时）。

### 工具定义的真实形状

```ts
import { StringEnum, Type } from "@earendil-works/pi-ai";
import type { ExtensionAPI } from "@earendil-works/pi-coding-agent";
import { Text } from "@earendil-works/pi-tui";
import { buildArgs, decodeBench, ensureBench, resolveBinary, type Exec } from "./bench-core.ts";

export default function (pi: ExtensionAPI) {
  const { bin } = resolveBinary();
  const exec = pi.exec as unknown as Exec;

  pi.registerTool({
    name: "bench_random",
    label: "Bench 随机测试",
    description: "……（会直接进系统提示）",
    promptSnippet: "随机取一条 benchmark 提示词做本地 LLM 测试",
    promptGuidelines: [
      // 必须指名工具：这些 bullet 被扁平追加到 Guidelines，
      // 写“Use this tool when…”会让模型不知道 this 是哪个
      "Use bench_random with fresh=true when the user wants something new.",
    ],
    parameters: Type.Object({
      tag: Type.Optional(Type.String({ description: "按标签过滤" })),
      fresh: Type.Optional(Type.Boolean({ description: "排除最近抽过的条目" })),
    }),

    async execute(_id, params, signal) {
      await ensureReady();
      const res = await exec(bin, buildArgs("random", params), { signal, timeout: 60_000 });
      const data = decodeBench(res, bin, []) as BenchPrompt;
      return {
        content: [{ type: "text", text: renderPromptBlock(data) }],  // 进 LLM 上下文
        details: { id: data.id, truncated: false },                  // 供渲染与状态持久化
      };
    },

    renderCall(args, theme) { return new Text(theme.fg("toolTitle", "bench_random"), 0, 0); },
    renderResult(result, { isPartial }, theme) { return new Text("…", 0, 0); },
  });
}
```

### 必须遵守的 pi 约定

| 约定 | 后果 |
|---|---|
| **错误靠 throw 传递** | `execute` 只有一抛异常才会被标 `isError: true`；return 一个带 error 字段的对象**不会生效**，LLM 会把它当成成功 |
| **输出必须自行截断** | 不限长会把整个提示词库灌进上下文。本实现限 120 行 / 12KB，并告知省略了多少 |
| **`StringEnum` 而非 `Type.Union`** | 字符串枚举用 `Type.Union`/`Type.Literal` 在 Google API 上不工作 |
| **`promptGuidelines` 要点名工具** | bullet 被扁平追加，没有工具名前缀 |
| **工厂里不开后台资源** | 懒初始化 + 失败不缓存 |
| **print/json 模式 `hasUI=false`** | 所有 UI 调用都要先守 `ctx.hasUI`，否则通知丢失或报错 |

### 工具与命令清单

| 能力 | 工具 | 命令 |
|---|---|---|
| 随机测试 | `bench_random(tag?, fresh?)` | `/bench-random [tag]` |
| 一键测试 | `bench_get(id, local?)` | `/bench-get <id>` |
| 打分 | `bench_score(id, value)` | `/bench-score <id> <1-5>` |
| 浏览/同步/状态 | `bench_catalog(action=list\|sync\|status)` | `/bench-list` `/bench-sync` `/bench-status` |
| 上传 | `bench_upload(content, tags?, client_id?)` | — |

取题类命令用 `ctx.ui.setEditorText(...)` 把**完整正文放进输入框**；
早先用 `ctx.ui.notify` 展示整段提示词是错的设计（notify 是单行 UI）。

配套 **skill**（`plugins/pi/skill/benchmark-testing/`）负责流程知识：
提示词必须原样投喂不得改写、抽题与打分交替、`--fresh` 的真实语义、常见报错怎么读。
skill 只走“描述常驻 + 正文按需 read”的渐进式暂露，不占常驻上下文。

验证：`make test-ts`（pi 33 项 + dsh 侧 7+23 项）+ `make smoke-pi`（12）
+ `make smoke-dsh`（19，DSH 侧见 §4）。

## 4. DSH 插件适配（已实现，真机验证）

> 实现说明与安装路线：[`benchmark-prompts/plugins/dsh/README.md`](../benchmark-prompts/plugins/dsh/README.md)。
> 调研底稿与 §12 未核实项的逐项落地：[handover-dsh.md](./handover-dsh.md)。

实测结论（DSH = `@deepseek-ai/dsh` 0.1.2-rc.1，插件框架 **Cordis** 4.0.2）：

- 插件形态：导出 `{ name, inject, Config, apply(ctx, config) }` 的 TS 模块；
  工具用 `ctx.tools.register(defineTool({...}))` 注册，命令用 `ctx.commands.register`。
- **参数 schema 是 schemastery 声明式对象，不是 TypeBox**；
  `output.schema` + `output.render` 是**强制的**，`execute` 只返回 JSON 值。
- 错误同样靠 `throw`（与 Pi 一致）；大结果有框架级 spill（`maxInlineBytes: 50000`）。
- 执行命令用 `ctx.subprocess.spawn({ argv, cwd, stdio, graceMs, signal, env })`，
  **argv 不经 shell 解释**；但环境会被净化（`DSH_*` 与
  `/KEY|PASSWORD|SECRET|TOKEN/i` 被剥），需显式放进 `spec.env` 才透传 ——
  因此插件配置一律走 `--home`/`--endpoint` 命令行参数，不寄托于宿主环境。
- **skill 无需写代码**：DSH 读 `~/.dsh/skills/`、`~/.agents/skills/`、
  项目内 `.dsh/skills/` 下同样格式的 `SKILL.md`，直接复制即可
  （自定义根也可用 patch 配 `skill-filesystem.customSkillDirs`）。
- 装载（交接 §12-A/G 落地）：Node 26 原生执行 `.ts` 源码（无构建）；
  patch `insert` 的相对 `name` 锚定 **patch 文件自身目录**；ESM 解析不补扩展名；
  插件须在 profile 树内以命中 `~/.dsh/profiles/node_modules`；
  `dsh plugin add file:<dir>` 装为 bundle（package.json 的 `dsh.bundle.patch`）。
- 斜杠命令（§12-E）：handler 直接在 agent 上执行、不过模型，
  `CommandResult {kind, text}` 由派发它的 UI 渲染；headless 无命令派发面。

已交付实现（`benchmark-prompts/plugins/dsh/`）：5 工具 + 6 命令 + subprocess 桥，
`bench_random` / `bench_get` / `bench_score` / `bench_catalog` / `bench_upload`
与 Pi 侧同构、逐字共用渲染模板（§5）。验证三层：
`node --test`（7 项 bench-core 副本回归 + 23 项假 ctx 框架功能测试）、
`make typecheck-dsh`（tsc 对真实 .d.ts）、`make smoke-dsh`（真 DSH 真 LLM 19 项：
对照实验、禁用旁路后的唯一标记断言、四类失败分支、skill 发现）。


两条旧推断已死：

1. ~~“若 DSH 只支持函数式插件，就直接引用 SDK（`pkg/client`）”~~ ——
   **不可能**。DSH 插件是 TypeScript，`pkg/client` 是 Go，跨语言无法 import。
   这止进一步肯定 ADR-6：**`bench` CLI 是唯一能跳语言复用的边界**。
2. ~~“适配逻辑与 §3.2 完全一致”~~ —— §3.2 那个 `tools:` YAML 本身就是臆造的
   （已在 §3 标注并更正）。两框架的注册 API **形状完全不同**，
   能复用的只有 `bench-core.ts` 那层纯逻辑（§10）。

## 5. 结果回填格式（两框架统一）

插件拿到 prompt 后，按框架约定注入上下文，实现里用的就是下面这个模板
（Pi 侧由 `renderPromptBlock` 产生）：

```
【Benchmark 提示词 <id>】标签: <tags>  v<version>
----------------------------------
<prompt 正文>
----------------------------------
把上面这段原样发给被测模型；回答完成后调用 bench_score(id="<id>", value=1-5) 记录评分。
```

## 6. 常见边界

| 边界 | 处理 |
|------|------|
| `bench random` 返回空（无 approved） | 提示「当前无可测试提示词」，退出码 0 |
| 网络不通 / 未同步 | 提示「请先 bench sync 或检查网络」，不崩溃 |
| 未配置 endpoint | 首次激活 → 引导 `bench config init` |
| 鉴权失败（打分/上传） | 提示配置 `--key` / `--secret`，匿名只读不受影响 |

## 7. 验收清单

- [x] `bench get` / `random` / `score` / `list` / `sync` / `upload` 可用且 `--json` 可解析
- [x] 退出码分类正确（离线、401、404、参数错）——由 `make smoke-cli` 验证 35 项
- [x] 源站下线时 `--local` 仍能取回（离线能力）
- [x] 未配置时给出可操作的下一步提示（`config init --endpoint`）
- [x] Pi 中能用一句话触发“随机测试”并得到一条提示词（M4，smoke-pi 12/12）
- [x] Pi 中能按 id 触发“一键测试”（M4）
- [x] DSH 中同样两能力可用（M4，`make smoke-dsh` 真装载 + 真 LLM 19 项通过）
- [x] 打分后能读到返回的 avg/count
- [x] 离线/未同步时给出友好提示而非堆栈

## 8. 目录

```
plugins/
├── pi/
│   ├── README.md             # 安装、能力清单、设计说明、已知限制
│   ├── extension/
│   │   ├── index.ts          # 5 个工具 + 6 个斜杠命令
│   │   ├── bench-core.ts     # 纯逻辑，无 pi 依赖（★ 两框架共用的核心层）
│   │   └── bench-core.test.ts
│   └── skill/benchmark-testing/SKILL.md
└── dsh/
    ├── README.md             # 三条装载路线、配置、验证、§12 落地记录
    ├── index.ts              # Cordis 插件：defineTool ×5 + commands ×6 + subprocess 桥
    ├── bench-core.ts         # 逐字节复制自 pi（哈希钉防漂移，改动须双向同步）
    ├── bench-core.test.ts    # 副本回归（哈希钉 + 行为冒烟）
    ├── plugin.test.ts        # 假 ctx + 真 defineTool 的框架层功能测试
    ├── package.json          # bundle 声明（dsh plugin add 路线）
    └── cordis.patch.yml      # bundle patch 层
```