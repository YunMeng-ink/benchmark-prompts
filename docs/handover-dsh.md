# DSH 插件交接文档（已完成 → 转为框架研究底稿）

> **当前定位（2026-09-03 整理）**：本文原本是写给“将要实现 `plugins/dsh/` 的 Agent”
> 的交接材料。**实现已完成**（5 工具 + 6 命令 + subprocess 桥，`make smoke-dsh` 19/19）。
> 因此本文现在的价值是：
>
> - **DSH 框架研究底稿** —— 插件 API 的准确形状、本机源码位置与行号（将来
>   扩展 DSH 能力、或适配别的 Cordis 应用时仍然用得上）；
> - **`bench` CLI 契约的地面数据**（§9，由脚本实测采集，不靠转述）；
> - **方法论与历史决策记录**（§11–§12）。
>
> **日常使用/安装请看 `benchmark-prompts/plugins/dsh/README.md`**，它比本文§§3/10 更新且经实测。
>
> 写作时间：2026-09-03。代码基线：`benchmark-prompts/` M1–M4（Pi + DSH 均已完成）。
>
> **路径约定**：本文里 `benchmark-prompts/…` 与 `docs/…` 均相对于仓库根
> （下面写作 `<仓库根>`）；`~` 指本机用户目录（Windows 上实测可用）。
>
> **本文所有 DSH 结论均为本机实测所得，非推测。** 未实现前标注为“未核实”的部分
> 已由实现阶段逐条落地（见 §12），保留原文供追溯。

---

## 0. 先读这段（TL;DR）

三句话（当初给实现者的，今天仍然成立）：

1. **不需要调研 DSH 框架** ——本文已把它实测抹平。它不是 pi 那套
   `pi.registerTool`，而是 **Cordis 插件 + `defineTool`**，参数 schema 用
   **schemastery 声明式对象**而不是 TypeBox。
2. **Pi 那套代码不能直接拷**，但 `bench-core.ts`（argv 构造、退出码→语义映射、
   信封解码）**几乎可以整份复用** —— 它当初就被刻意写成零依赖纯逻辑。
   （实现阶段采用“逐字节副本 + sha256 钉住测试”而非共享包，见 §10 注记。）
3. **skill 一行代码都不用写**：DSH 读 `~/.dsh/skills/` 与 `.agents/skills/` 下
   同样格式的 `SKILL.md`，复制现有那份即可（已实跑验证）。

实现已完成。**若你只是要用它** → 看 `plugins/dsh/README.md`；
**若你要改它或扩能力** → 先读本文 §4、§7、§9，再读 §10 的“推荐 vs 实际”差异。

---

## 1. 任务与验收标准

### 要做什么

在 `benchmark-prompts/plugins/dsh/` 下实现 DSH 侧薄适配，提供与 Pi 等价的能力：

| 能力 | Pi 里的名字 | DSH 里要做成 |
|---|---|---|
| 随机取一条 | `bench_random` 工具 + `/bench-random` | tool + command |
| 按 id 取一条 | `bench_get` + `/bench-get` | tool + command |
| 打分 1–5 | `bench_score` + `/bench-score` | tool + command |
| 浏览/同步/状态 | `bench_catalog` + `/bench-list` 等 | tool + command |
| 上传 | `bench_upload` | tool |
| 流程知识 | `plugins/pi/skill/…/SKILL.md` | 复制即可（§6） |

### 验收标准（缺一不可）

> **实现结果：五条全部达成**，逐条证据在 `make smoke-dsh`（19/19）
> 与 `plugins/dsh/`；对照实验、旁路禁用、SKIP 语义均照 §11 执行。

- [x] 在**真实 DSH** 里，模型能调用工具取到源站上一条**带唯一标记**的提示词
      —— 不是模型自己编的文本（用 §11 的标记法验证）。
- [x] 断网/未配置/分值越界/条目不存在四类失败，DSH 侧显示的是**可行动的**中文提示，
      且**没有**被标成成功结果。
- [x] `bench` 不存在时给出安装指引，且用户装好后**不重启 DSH** 也能恢复
      （Pi 侧靠“探测失败不缓存”达成，见 `bench-core.ts` 的 `ensureBench`；DSH 侧同一手法，
      且有单元断言）。
- [x] 有一条能在本机自动跑的验证脚本（`scripts/smoke-dsh.sh` + `make smoke-dsh`）。
- [x] 回写 `docs/plugins.md` §4（已改为“已实现，真机验证”，§7/§8 同步更新）。

### 硬性约束

**插件里不许出现任何业务逻辑**：不许直接发 HTTP、不许自己解析 ETag/增量同步、
不许自己拼 URL。全部走 `bench` CLI。理由见 §9 末尾。

---

## 2. DSH 是什么（实测身份认定）

| 项 | 值 | 证据 |
|---|---|---|
| 主包 | `@deepseek-ai/dsh` **0.1.2-rc.1** | `~/.dsh/profiles/node_modules/@deepseek-ai/dsh/package.json` |
| 本质 | DeepSeek 官方 coding agent，**223 个 `dsh-*` 子包**（实测计数） | 同目录列表 |
| 插件框架 | **`@deepseek-ai/cordis` 4.0.2**（+ `cosmokit` 1.8.3） | `cordis/package.json` |
| 参数/配置校验 | **`@deepseek-ai/schemastery` 3.18.2**，工具内部也用 **zod ^4.4.3** | `dsh-tool-todo/package.json` |
| LLM 层 | **内嵌 `@earendil-works/pi-ai` 0.84.2** | `node_modules/@earendil-works/pi-ai/package.json` |
| 数据目录 | `~/.dsh/`（`profiles/`、`sessions/`、`storages/`、`settings.yaml`） | 本机 |

### 一个容易误判的点

DSH 内嵌 `pi-ai`，**但 `pi-ai` 只是模型/Provider 抽象层**（TypeBox 再导出、
`StringEnum`、各家 API 方言），**不是 Pi 的扩展层**（`pi-coding-agent` 的
`ExtensionAPI` / `registerTool`）。DSH 通过自己的 `dsh-llm-pi-ai` 包用前者。

所以：**Pi 的扩展代码不能因为“DSH 也用了 pi”就照搬。** 这是本任务最可能
是容易浪费人天的误区：看到 DSH 内嵌 `pi-ai` 很容易误以为可以照搬 Pi 扩展代码。

### 如何自行复核（重要）

**DSH 的 npm 包直接附带 TypeScript 源码**，`package.json` 里有
`"./src/*": "./src/*"` 导出。也就是说本机就有全部实现可读，
不需要外部文档、不需要猜：

```bash
ls ~/.dsh/profiles/node_modules/@deepseek-ai/dsh-tool-todo/src/
# client.ts  index.ts  invariant.ts  types.ts
```

看一个真实工具怎么写，就读 `dsh-tool-todo/src/index.ts`（约 190 行，五脏俱全）。
**这份文档 §4 的所有断言都出自该文件与 `dsh-tools/src/index.ts`。**

---

## 3. Cordis 插件模型

一个 DSH 插件 = 一个 TS/JS 模块，导出四样东西（照 `dsh-tool-todo/src/index.ts`）：

```ts
import type { Context } from '@deepseek-ai/cordis'
import z from '@deepseek-ai/schemastery'

export const name = 'dsh-bench'                 // 插件标识
export const inject = ['tools', 'subprocess']   // 依赖的服务名，缺了就不启动
export const Config = z.object({                // 用户可配项（可省略）
  endpoint: z.string().default(''),
})

export function apply(ctx: Context, config: Config): void {
  // 在这里注册工具/命令/服务
}
```

`inject` 是 Cordis 的核心机制：**服务可用性驱动激活**。声明了 `['tools']`，
就要等 `tools` 服务就绪才 `apply`；不声明则永远拿不到该服务（类型上也不存在）。

服务通过 `declare module` 增补类型（`dsh-commands/src/index.ts` 里就是这么写的）：

```ts
declare module '@deepseek-ai/cordis' {
  interface Context { commands: CommandRuntime }
}
```

### 插件怎么被装载

Profile 目录 `~/.dsh/profiles/<profile>/`：

| 文件 | 作用 | 你该不该改 |
|---|---|---|
| `package.json` | `dsh.profile.bundles: ["@deepseek-ai/dsh-base", …]` 声明装了哪些 **bundle** | 发布成 npm 包时走这里 |
| `cordis.yml` | 空入口列表，注释明写 "Edit cordis.patch.yml, **not** this file" | ❌ 不要改 |
| `cordis.patch.yml` | **你的 patch 层**，在所有 bundle 之后应用 | ✅ 走这里 |

patch 是“按 id 定向覆盖”的 YAML 数组，支持 `insert` / `config` 覆盖 / `disabled` /
`inject`，允许 `!!js` 表达式。真实样例（`dsh-base/cordis.patch.yml`）：

```yaml
- insert:
    - id: tool-todo
      name: '@deepseek-ai/dsh-tool-todo'
      config:
        allowParallelInProgress: true

    - id: spill-policy
      name: '@deepseek-ai/dsh-spill-policy'
      config:
        maxInlineBytes: 50000
```

所以本项目最可能的装载方式：

```yaml
# ~/.dsh/profiles/web/cordis.patch.yml
- insert:
    - id: bench
      name: './plugins/dsh-bench'      # 或 npm 包名
      config:
        endpoint: https://<你的源站>
```

> **✅ 已落地（§12-A）**：patch `insert` 里的相对 `name` 由 `dsh-app-boot` 的
> `anchorInsertedPluginNames` 按 **patch 文件自身目录** 锚定为 file URL；
> bundle 层（包内 `cordis.patch.yml`）则锚定在**包目录**，所以 `dsh plugin add
> file:<dir>` 复制安装后仍成立。ESM **不补扩展名、不猜 index**，必须给全文件名；
> 且插件文件必须在 profile 树内（或在其中可上溯到 `profiles/node_modules`），
> 否则 `@deepseek-ai/*` 裸导入解析不到。详见 `plugins/dsh/README.md` “装载三条路线”。

---

## 4. 工具注册 API（本任务主战场）

```ts
import { defineTool } from '@deepseek-ai/dsh-tools'

ctx.tools.register(defineTool({
  name: 'bench_random',
  description: '随机取一条 benchmark 提示词用于测试本地模型',
  parameters: {                       // ← 声明式 spec，不是 TypeBox
    tag: { type: 'string', description: '按标签过滤' },          // 可选 = 省略 required
    fresh: { type: 'boolean', description: '排除最近抽过的条目' },
    // ⚠️ 实现时订正：`required: false` 通不过类型（ParameterPropertySpec 的
    // required 是 `required?: true`）。必填属性写 `required: true`，可选属性整个省略该键。
  },
  output: {                           // ← output 是强制的
    schema: {
      type: 'object',
      additionalProperties: false,
      properties: {
        id:   { type: 'string', required: true },
        body: { type: 'string', required: true },
        tags: { type: 'array', required: true, items: { type: 'string' } },
      },
    },
    render: (args, value) => [{ type: 'text', text: `…` }],   // 模型可见内容
    presentationMeta: (args, value) => ({ id: value.id }),    // 可选，供 UI 回放
  },
  timeoutMs: 60_000,                  // 可选，见下
  isConcurrencySafe: () => false,     // 可选，见下
  async execute(args, exec) {         // ← 返回符合 output.schema 的 JSON 值
    if (exec.signal.aborted) throw new Error('已取消')
    const res = await spawnBench(['random', '--json'], exec.signal)
    return decode(res)
  },
  presentCall: a => ({ card: 'generic', title: 'Bench 随机取题', kind: 'other' }),
}))
```

### 字段全表（`DefineToolOptions` 在 `dsh-tools/src/schema.ts:483-513`；
`ToolDefinition` 在 `dsh-tools/src/index.ts:214`）

| 字段 | 必填 | 说明 |
|---|---|---|
| `name` | ✅ | 全局唯一 |
| `description` | ✅ | 直接发给模型 |
| `parameters` | ✅ | `ParameterSchemaSpec` 逐属性表；编译成隐式 open object 根 |
| `output.schema` | ✅ | **每次成功调用的值都会被它校验** |
| `output.render(args, value)` | ✅ | 纯函数，把 JSON 值投影成 `ContentBlock[]`（模型所见） |
| `output.presentationMeta` | ❌ | 顶层调用才计算，可回放的展示元数据 |
| `execute(args, exec)` | ✅ | **只返回 JSON 值**，不返回 content |
| `timeoutMs` | ❌ | 协作式超时预算；由 `dsh-tool-call-timeout-policy` 强制执行；**永不发给模型** |
| `isConcurrencySafe(args)` | ❌ | 声明能否进并行组。默认按独占处理 |
| `finalizeContent(exec, result)` | ❌ | 物化前的最后一道内容变换，必须全函数、**不许抛** |
| `presentCall` / `presentResult` | ❌ | UI 卡片投影，纯函数，流式与回放都会调 |

### 与 pi 最要紧的三个差异

| | Pi | DSH |
|---|---|---|
| 参数 schema | TypeBox `Type.Object` | schemastery 声明式对象，`required: true` 写在属性里 |
| `execute` 返回 | `content` + `details`（模型所见即你给的文本） | **只返回 JSON 值**，由 `output.schema` 校验、`output.render` 决定模型看到什么 |
| 取消 | `execute` 收到 `signal` 入参 | `exec.signal`；文档明确要求"异步工作必须观察或转发 `exec.signal`，并在 abort 后到达静默才 settle" |

**第二个差异最容易写错**：从 pi 迁移时如果直接把文本 return 出去，会被
`output.schema` 校验打回。**先把结果形状定义清楚，再写 execute。**

### 错误传递

与 pi 一致：**抛异常**。`dsh-tool-todo/src/index.ts` 里通篇是
`throw new Error('invalid todos: …')`，以及
`if (!exec.agent) throw new Error('todo_write requires an owning agent session')`。

> Pi 侧的实测教训：把错误包成“正常返回一个带 error 字段的对象”会被当成成功，
> 模型于是信心满满地幻觉出一条提示词。**DSH 同理，别写这种代码。**

### 大结果：DSH 有框架级兜底，但别依赖

`dsh-spill-policy` 配置 `maxInlineBytes: 50000`：超阈值的工具结果会被替换成
**有界的首尾预览 + 落盘引用**（`dsh-spill-policy/src/index.ts` 明确写了
"model-facing result with a bounded head/tail preview"）。

- 单条提示词默认上限 8192 字符（服务端 `moderation.max_prompt_len`，**可配**，默认值见
  `internal/config/config.go:120`），**远低于** 50KB，所以取题类工具不会触发 spill。
  所以取题类工具不会触发 spill。
- 但 `bench_catalog action=list` 同步大目录后可能超阈值 —— 届时模型拿到的是
  **预览而非全文**。**仍然要在插件侧自己做分页/限流截断**，别把正确性寄托在 spill 上。
  Pi 侧的截断逻辑（`bench-core.ts` 的 `truncate`）直接搬过来用。

---

## 5. 命令（slash command）API

`dsh-commands/src/index.ts`：

```ts
ctx.commands.register({
  name: 'bench-random',                  // 正则：/^\/([a-z][a-z0-9_-]*)(?=$|[\t\n\r ])/u
  description: '随机取一条 benchmark 提示词',
  input: { /* CommandInputDescriptor，可选 */ },
  recordInput: true,                     // 默认 true
  handler: async (inv) => { /* … */ },
})
```

`CommandInvocation` 提供：`agent`、`rawInput`（命令名之后的原文，含分隔空白）、
`attachments`（需在 `input.images` 声明才会给）、`signal`。

**关键差异**：DSH 的 command `handler` 是"**Execute against the receiving agent
without sending the command to the model**"——直接在 agent 上执行，**不经过模型**。
而 Pi 的 slash command 默认把文本塞进上下文让模型动作（Pi 侧实现用 `setEditorText`
把正文填进输入框）。

DSH 的注释还提到 `command/run` 与 `dsh-client-ui-commands` 的斜杠菜单。

> **✅ 已落地（§12-E）**：对应原语就是 `CommandResult { kind: 'success'|'error', text }`
> （`dsh-commands/src/types.ts`）—— handler 返回的 `text` 由派发它的 UI 直接渲染，
> 不过模型。所以 DSH 侧 `/bench-get` 把正文块放在命令结果里，
> 而不是像 pi 那样 `setEditorText` 填输入框。headless 形态**没有**命令派发入面
> （task 恒走模型），那里只用工具路径即可。

---

## 6. skill：零代码复用

`dsh-skill-filesystem/src/index.ts` 显示 DSH 扫描这些根（241–263 行）：

| 来源 | 路径 | rank |
|---|---|---|
| project | `<projectRoot>/.dsh/skills` | 项目层 |
| project | `<projectRoot>/.agents/skills` | 项目层 |
| custom | 配置里的 `skillDirs` | 插在 project 与 user 之间 |
| user | `~/.dsh/skills` | 用户层（`skipSystem: true`） |
| user | `~/.agents/skills` | 用户层 |
| bundled | `$DSH_BUNDLED_SKILL_DIR` | 受信 host |

规则同样是**目录下的 `SKILL.md` + YAML frontmatter**，且
**frontmatter 必须含 `name` 与 `description`**，缺失/非法时 `ctx.logger.warn` 后忽略。

```bash
cp -r benchmark-prompts/plugins/pi/skill/benchmark-testing ~/.dsh/skills/
```

DSH 额外认识这几个 frontmatter 字段（pi 不认，但多字段不会报错，pi 忽略未知键）：

| 字段 | 出处 |
|---|---|
| `invocation` | `src/index.ts:824` |
| `disable-model-invocation` | `src/index.ts:996` |
| `user-invocable` | `src/index.ts:997` |

⚠️ **它会对“旧字段名”直接抛错**（`frontmatter field "${legacy}" is unsupported;
use "${canonical}"`，1006 行）。**别加 `allowed-tools` 之类 Claude Code 老字段。**

另一个调试坑：`<projectRoot>` 不是 `cwd`，而是由 `findProjectRoot()` 向上探测得出，
而 project root marker **是可配的**（`dsh-agent-instructions/src/config.ts:41`，
`projectRootMarkers`）。项目级 skill 加载不到时先查这里，而不是查文件内容。

反过来（DSH→pi 方向）也要注意：pi 对未知 frontmatter 的容忍度需实测；
本项目的做法是**只保留最小 frontmatter（`name` + `description`），一份文件两框架通用**。

---

## 7. 怎么执行 `bench`

用 `ctx.subprocess`（`inject` 里声明 `'subprocess'`）。接口在
`dsh-subprocess/src/types.ts`：

```ts
interface SubprocessSpawnSpec {
  argv: readonly string[]   // argv[0] 是可执行文件；**永不经过 shell 解释**
  cwd: string
  stdio: SubprocessStdio    // 逐流设置，取 stdout 就要 pipe
  graceMs: number           // terminate 升级与管道排空的宽限期
  signal?: AbortSignal
  env?: NodeJS.ProcessEnv   // 显式项会**覆盖净化**，字符串=故意透传，undefined=删除该 ambient 项
}
```

`spawn(spec)` **立即返回 handle**，`handle.done` 在进程关闭时 resolve 并给出退出事实
（只有 spawn 级失败才 reject）。还有 `resolve(command)` 把裸命令名解析成规范可执行路径。

### 两个直接影响实现的性质

1. **`argv` 不经 shell** → 提示词正文含引号/换行/`$` 都安全。Pi 侧需要防范的注入
   问题在 DSH 不存在。**不要把正文拼进命令行字符串。**
2. **环境会被净化**：`dsh-subprocess/src/index.ts` 注释明说 harness 自己的
   `DEEPSEEK_API_KEY`/secrets 不应隐式泄漏给子进程，`DSH_*` 会被剥掉。
   **显式 `env` 项可以透传**（"a deliberate caller opt-in … survives the scrub"）。
   → 如果用户靠 `BENCH_ENDPOINT` 等环境变量配置，**必须显式塞进 `spec.env`**，
   否则会神秘失效。**建议：插件一律用 `--home`/`--endpoint` 命令行参数，
   不依赖环境变量透传。** 该结论未端到端实测（见 §12-B）。

### 兜底方案

若 `ctx.subprocess` 不可用或行为不合用，DSH 有 `dsh-tool-bash`（模型可见的 bash 工具）
与 `dsh-subprocess-local`（本地实现）。但**不要**把 `bench` 调用退化成“让模型自己
敲 bash”——那会丢掉工具的结构化结果与错误分支。

---

## 8. 可用的钩子（若要加策略）

`dsh-tools/src/index.ts` 定义的 waterfall/emit 钩子：

| 钩子 | 模式 | 用途 |
|---|---|---|
| `tools/execute` | waterfall | 包住派发（超时策略即由此实现）；`next()` 放行，可替换结果 |
| `tools/post-execute` | waterfall | 接受/替换/加严/拦截结果；**抛异常的工具也会进到这里** |
| `tools/result` | emit | 观察深冻结的最终结果 |
| `tools/change` | emit | 工具集变化（**未做 scope 过滤**，全局变更所有 agent 都收到） |

如果将来要做“某工具不可用就禁用”或“审计所有 bench 调用”，走这里，
不要往 `execute` 里塞全局状态。

---

## 9. `bench` CLI 契约（**真实采集，非文档转述**）

本节输出全部由 `scripts/capture-contract.sh` 在 M4 当天实测采集，
可重跑再生成（见 §11）。**这是跨框架唯一稳定的契约面。**

### 信封

```json
{"data":{"version":"dev"},"error":null,"ok":true,"v":1}
```

- **成功与失败同形**（M4 时特意统一的，之前成功返回裸对象、失败返回信封）。
- 键序为字典序 `data,error,ok,v`（Go `map[string]any` 序列化所致）。
  **不要依赖键序，必须用 JSON 解析器。**
- `v` 恒为 1；不是 1 就按契约不符失败（`bad_response`）。

### 命令与真实输出

```bash
# bench version --json        exit=0
{"data":{"version":"dev"},"error":null,"ok":true,"v":1}

# bench meta --json           exit=0
{"data":{"local_count":0,"catalog_hash":"","server_total":0,
 "server_hash":"e3b0c44…b855","up_to_date":false,
 "last_check":"2026-09-03T09:43:55.4710966Z"},"error":null,"ok":true,"v":1}

# bench sync --json           exit=0
{"data":{"local_count":1,"report":{"upserted":1,"deleted":0,
 "since":"90b74455…d1a8","pages":1,"full_sync":true,
 "changed":["p_6e905430"]}},"error":null,"ok":true,"v":1}

# bench random --json         exit=0        （bench get <id> --json 同形）
{"data":{"id":"p_6e905430","p":"请用五号字写一封辞职信，语气要克制但锋利。",
 "t":["writing"],"v":2,"s":"approved","h":"c95c173b"},"error":null,"ok":true,"v":1}

# bench list --all --json     exit=0        （注意：无正文，只有摘要）
{"data":{"count":1,"items":[{"id":"p_6e905430","t":["writing"],"v":2,
 "h":"c95c173b"}]},"error":null,"ok":true,"v":1}

# bench score <id> 4 --json   exit=0
{"data":{"avg":4,"count":1},"error":null,"ok":true,"v":1}

# bench config show --json    exit=0
{"data":{"device_id":"","endpoint":"http://127.0.0.1:18099",
 "has_key":true,"has_secret":true,"path":".contract\\home\\config"},
 "error":null,"ok":true,"v":1}
```

提示词字段是**压缩名**：`p`=正文、`t`=标签、`v`=版本、`s`=状态、`h`=短 hash。
（`docs/api.md` 冻结的线上格式，省带宽用的；别指望是可读的英文名。）

### 错误：退出码 + `error.code`

| 命令 | exit | 真实输出 |
|---|---|---|
| `get does-not-exist` | **4** | `{"error":{"code":"not_found","message":"not_found: 资源不存在 (HTTP 404)"},…}` |
| `random --tag=no-such-tag` | **4** | `not_found`，message 为“当前没有可用的提示词” |
| `score <id> 9` | **5** | `{"error":{"code":"validation_failed","message":"validation_failed: value 必须是 1-5"},…}` |
| 未配置 endpoint 就 `meta` | **5** | `{"error":{"code":"local_error","message":"尚未配置服务地址；请运行 bench config init --endpoint <url>，或设置环境变量 BENCH_ENDPOINT"},…}` |
| 源站不可达（`--json`） | **1** | `{"error":{"code":"network","message":"network: 网络请求失败 (…connectex…)"},…}` |

退出码常量（`pkg/client/errors.go:30-35`）：

| 码 | 名 | 含义 |
|---|---|---|
| 0 | `ExitOK` | 成功 |
| 1 | `ExitNetwork` | 网络/超时/取消（**可重试**） |
| 2 | `ExitRateLimited` | 限流（退避后重试） |
| 3 | `ExitAuth` | 未授权/禁用（提示配置 Key） |
| 4 | `ExitNotFound` | 不存在/无可用（提示换条件） |
| 5 | `ExitBadInput` | 入参/本地配置错误（**不要重试**） |

错误码全集（`pkg/client/errors.go:13-24` + `internal/cli/cli.go` 的 `classifyError`）：

```
服务端映射：bad_request unauthorized forbidden not_found conflict too_large
           validation_failed rate_limited internal unavailable
本地分类：bad_response  network  canceled  local_error
```

> ⚠️ **`canceled` 与 `local_error` 尚未写进 `docs/client.md` §11 的错误码表**
> （那里只列了 12 个）。这是文档缺口，实现时按代码为准，并顺手补上文档。

### 关键陷阱：不带 `--json` 就拿不到错误信封

**不带 `--json` 时，错误只走 stderr，stdout 为空**：

```
$ bench get p_x                      # 无 --json
exit=1  stdout=（空）
stderr= 错误[network] network: 网络请求失败 (…)
```

插件**必须**始终带 `--json`。Pi 侧最初探测版本用 `bench --json`，结果 bench 把
`--json` 当子命令名退 5，把正常二进制误判为“版本过旧”，扩展 100% 不可用 ——
**探测请用 `bench version --json`**（`bench-core.ts` 已这么写，直接复用）。

### 为什么必须走 CLI

缓存、增量同步、ETag/304、退避重试、HMAC 签名、离线回退全在 Go 里实现了一份。
如果 DSH 插件直接发 HTTP，就要用 TS 重写整套同步协议 —— 而它是**冻结契约**，
两边各写一遍必然漂移。这是本项目的核心设计约束（`docs/architecture.md` ADR-6）。

---

## 10. 推荐实现方案

### 分层：复用 bench-core

Pi 适配当初刻意把易错逻辑抽成零依赖纯模块 `plugins/pi/extension/bench-core.ts`，
**不 import 任何框架代码**。它导出：

```
buildArgs(command, params)      参数 → argv
decodeBench(res, bin, prefix)   退出码 + stdout → 数据 / 抛 BenchError
ensureBench(bin)                探测可用版本（失败不缓存）
truncate(text, maxLines, maxBytes)
assertScoreValue / assertId     入参前置校验（不消耗网络往返）
resolveBinary()                 BENCH_BIN > PATH
formatStatus() / BenchError / ExitCode
```

`Exec` 类型是自定义的最小函数签名，因此**不绑定 Pi 的 `pi.exec`**。

于是 DSH 适配最省事的做法：

```
plugins/dsh/
├── index.ts        # 只做：apply(ctx, config) + defineTool + ctx.subprocess 适配 + commands
├── bench-core.ts   # 从 plugins/pi/extension/bench-core.ts 复制（同一份代码）
└── README.md
```

**不要**抽成共享包 —— 两边装载机制不同（pi 走 jiti 相对导入、DSH 走 npm 包解析），
为一个文件引入 workspace 打包不值得。**但要**在 `plugins/dsh/README.md` 里写明
“本文件复制自 pi，改动必须双向同步”，并让 TS 测试同时跑两份（`node --test` 两边路径
都列上，或让 DSH 那份 test 直接 import pi 那份断言行为一致）。

> 更好的办法（如果嫌复制脏）：让 `plugins/dsh/bench-core.ts` 只是一行
> `export * from '../../pi/extension/bench-core.ts'`。装载上是否允许跨目录相对导入，
> 属于 §12-A 那个未验证问题的一部分。

### 建议的 `apply` 骨架

> **实现注记**：骨架里 `resolveBinary(config.bin)` 与实际签名不符
> （`resolveBinary(env)` 只认 `BENCH_BIN`/PATH；bin 的 config 覆盖在入口做）；
> `makeSubprocessExec` 的实际实现叫 `makeExec`，见
> `benchmark-prompts/plugins/dsh/index.ts`——以代码为准。

```ts
export const inject = ['tools', 'commands', 'subprocess']

export function apply(ctx: Context, config: Config): void {
  const { bin } = resolveBinary(config.bin)

  // 一次封装：argv → { stdout, stderr, code }，并把 exec.signal 透传给 spawn
  const exec = makeSubprocessExec(ctx.subprocess, config.graceMs)

  const run = async (args: string[], signal?: AbortSignal) => {
    await ensureBench(bin, exec)                       // 懒探测，失败不缓存
    const res = await exec(bin, args, signal)
    return decodeBench(res, bin, args)                 // 抛 BenchError = 工具失败
  }

  ctx.tools.register(defineTool({
    name: 'bench_random',
    description: '……',
    parameters: { tag: { type: 'string', required: false, description: '…' },
                  fresh: { type: 'boolean', required: false, description: '…' } },
    output: { schema: promptSchema, render: (_, v) => [{ type: 'text', text: renderPromptBlock(v) }] },
    timeoutMs: 60_000,
    isConcurrencySafe: () => false,                    // random 会写"最近抽过"窗口
    async execute(a, exec) { return await run(buildArgs('random', a), exec.signal) },
  }))
  // …get / score / catalog / upload
}
```

`renderPromptBlock`（Pi 侧的正文模板）也照搬，保持两框架**给用户完全一致的排版**：

```
【Benchmark 提示词 <id>】标签: <tags>  v<version>
----------------------------------
<正文>
----------------------------------
把上面这段原样发给被测模型；回答完成后调用 bench_score(id="<id>", value=1-5) 记录评分。
```

（模板出处：`docs/plugins.md` §5。）

> ⚠️ `renderPromptBlock` 在 **`plugins/pi/extension/index.ts:456`**，是个**未导出的
> 私有函数**，不在 `bench-core.ts` 里 —— 所以得从入口文件抄代码，而不是 import。

### `isConcurrencySafe` 怎么选

- `bench_get` / `bench_meta` / `bench_list` → **true**（只读）
- `bench_random` → **false**：它会推进客户端“最近抽过”滚动窗口，并发调用会互相吃掉排除集
- `bench_score` / `bench_upload` → **false**（写请求，且写不重试，见 `docs/client.md` ADR-12）

### 推荐 vs 实际（实现阶段对本节的修正）

以下是本文§§3–§10 的推荐与最终实现的差异，**以代码为准**：

| 本文原本写的 | 实测事实 | 影响 |
|---|---|---|
| 骨架里 `tag: { type: 'string', **required: false** }` | **可选属性必须省掉 `required` 键**；写 `required: false` 类型不过 | 直接抄骨架会编译失败，见 `plugins/dsh/index.ts` 真实写法 |
| §7：“`DSH_*` 与 API key 被剥” | 清洗范围更宽：匹配 `/KEY\|PASSWORD\|SECRET\|TOKEN/i` 的名字**全部**被剥 + 所有 `DSH_*` | `BENCH_API_KEY`/`BENCH_SECRET` **不会**透传；`BENCH_HOME`/`BENCH_ENDPOINT` 不在名单但也不建议依赖 |
| §10：“拷一份就行” | 装载副本目录里**看不到** `plugins/pi/` 兄弟目录 | 比对方案改为 **sha256 哈希钉**（不依赖布局）+ 布局可用时再逐字节比对 |
| 未提装载细节 | symlink 会被 Node ESM realpath 解析回仓库原位→裸导入失败；**要用 junction** | 开发回路的关键，见 README 路线 C |
| 未提 | `patchReload: live` **只热更 patch 层，不热更已缓存模块代码** | 改代码必须重启，别指望热更 |
| §7 建议“把凭据显式塞进 `spec.env`” | 最终实现**刻意不塞 env**：显式 env 会覆盖净化，可能泄漏宿主凭证 | 配置一律走 `--home` / `--endpoint` 参数（更安全） |

实现阶段还两侧同步修了一个类型缺陷（`as typeof envelope` 自指引出 `never`，
由 `make typecheck-dsh` 的 tsc 逮到）——这恰好说明“pi 入口无类型检查”这个
已知限制，在 DSH 侧是**可以被补上的**（DSH 包自带 `.d.ts`，而 Pi 发行版不带）。

> 方法论上多出一层本文没预见的强化：测试 patch 把 `tool-pwsh`/`tool-fs`/`tool-web`
> 等一切旁路工具 `disabled: true`，**第一轮实测真的抓到了“模型自己跑 CLI 取数据”
> 的假通过**。Pi 侧等价物是 `smoke-pi.sh` 里的 `--tools` 白名单（已核：
> 该白名单作用于内置+扩展工具，旁路已堵）。两边现在同构。

---

## 11. 验证方法论（照抄 Pi 的做法，别自创）

### 原则：先证明“检测手段有效”，再证明“被测对象无错”

`scripts/smoke-pi.sh` 里做的是**对照实验**：

1. 先加载一个**故意写坏**的扩展，断言 DSH/pi 会报 `Failed to load …`。
2. 再加载本项目插件，断言**不出现**该报错。

没有第 1 步，“没报错”就毫无证据价值 —— 它同样兼容“根本没加载”。
**DSH 侧必须复刻这个结构。**

### 原则：断言“数据来自源站”，而不是“看起来像结果”

Pi 侧的真实调用做法：往源站上传一条含**唯一标记串**的提示词，然后让**真实模型**
调用工具，在输出里 grep 那个标记。这样能排除“模型自己编了一段提示词”——
这是这类适配最容易产生的假通过。

### 可直接复用的脚本

```bash
bash benchmark-prompts/scripts/capture-contract.sh   # 重新生成本文 §9 的全部输出
make -C benchmark-prompts smoke-cli                  # 真实 bench 二进制端到端（35 项）
make -C benchmark-prompts smoke-pi                   # pi 侧适配验证（12 项，结构模板）
make -C benchmark-prompts smoke-dsh                  # DSH 侧适配验证（19 项）
```

`capture-contract.sh` 会自建服务端、登记 Key、种两条提示词、过审，然后打印
每个命令的**真实 stdout 与退出码**。改完 DSH 插件后，用同一份 seed 数据在 DSH 里
跑一遍，就能对比“CLI 给的东西”和“插件报给用户的东西”是否一致。

### 建议新增

> **已交付。** `scripts/smoke-dsh.sh`（A 前置 → B 单元/tsc →
> C 对照实验 → D 真实调用 → E 失败分支 → F skill 发现；测试 patch 会 `disabled` 掉
> `tool-pwsh`/`tool-fs`/`tool-web` 等旁路，堵死“模型自己跑 CLI 取数据”的假通过——
> 第一轮实测真的抓到了这条路）+ Makefile `smoke-dsh` / `typecheck-dsh` 目标 +
> `node --test` 7+23 项（`plugins/dsh/*.test.ts`）。SKIP 语义照本文 §11 执行。

**SKIP 必须显式区别于 PASS。** Pi 的 `smoke-pi.sh` 在缺 pi/凭据时以 0 退出并打印
SKIP 字样；`make lint-ts`/`test-ts` 在缺 node/biome 时同样打印 SKIP 而非静默成功。
照这个语义写，别让 CI 变成绿色谎言。

---

## 12. 开工前置问题的核实结果

调研阶段列出 8 个未验证问题（装载方式、环境净化、审批门控、卡片枚举、命令展示原语、
headless 形态、TS 装载、文档缺口）。**全部已由实现阶段核实**，本表是这些结论的唯一权威，
`plugins/dsh/README.md` 不再重复，只做指向。

| ID | 问题 | 结论 | 证据 |
|---|---|---|---|
| **A** | 本地未发布包的 `name` 如何解析 | patch `insert` 的相对 `name` 按 **patch 文件所在目录** 锚定为 file URL；bundle 层锚定**包目录**。ESM **不补扩展名、不猜 index**。裸导入自插件文件位置上溯命中 `profiles/node_modules`，故插件须在 profile 树内 | `dsh-app-boot` 的 `anchorInsertedPluginNames` + `smoke-dsh` 实跑 |
| **B** | 子进程环境净化会不会吃掉 `BENCH_*` | 剥离范围 = 匹配 `/KEY\|PASSWORD\|SECRET\|TOKEN/i` 的名字 + 全部 `DSH_*`。即 `BENCH_API_KEY`/`BENCH_SECRET` **不透传**；`BENCH_HOME`/`BENCH_ENDPOINT` 可透传。结论：配置一律走 `--home`/`--endpoint` 参数，显式 `spec.env` 只在确需时 opt-in | `dsh-subprocess/src/index.ts` + smoke D/E 段 |
| **C** | 第三方工具是否受审批门控 | **不受**。`tools/pre-execute` 的消费者全树检索只有 hooks 桥与 jobs；headless 真会话直跑成功 | 检索 + `smoke-dsh` D 段 |
| **D** | `card` 取值全集 | call 侧 `generic\|terminal\|diff`；result 侧另加 `search\|read\|web`。本插件全用 `generic` | `dsh-tools/src/presentation.ts` |
| **E** | “把长文本交给用户看/复制”的原语 | `CommandResult { kind: 'success'\|'error', text }`，`text` 由派发方 UI 直接渲染，不过模型。等价于 Pi 的 `setEditorText` 用途 | `dsh-commands/src/types.ts` |
| **F** | headless 形态 | `dsh --profile headless "task"`：stdout=最终答案、stderr=推理流与错误、退出码 0/1；**不派发 slash 命令**（task 恒走模型），故命令只在 web/tui 层有意义 | `dsh-headless/src/index.ts` + smoke 实跑 |
| **G** | 是否需要同步构建 | 不需要。Node 26 原生执行 `.ts`（erasable syntax）；`import type` 运行时归零，可安全侧载服务声明 | `smoke-dsh` 实跑 |
| **H** | `docs/client.md` §11 缺错误码 | 已补 `canceled` / `local_error` | 已回写 |

### 已知的服务端事实（别重复发现）

- 只读能力**匿名可用**；`score`/`upload` 需要 API Key + HMAC 签名
  （`bench config init --endpoint URL --key K --secret S`）。
- `list` **只返回摘要无正文**（防带宽黑洞）；取正文必须 `get`。
- 正文只 `TrimSpace`，**不折叠内部空白**（benchmark 提示词是多行的）。
- `--fresh` 排除的是“最近抽过的”滚动窗口（上限 50），**不是**“全部本地缓存”。
  这个语义在 M4 才修对，别改回去。
- **写请求不重试**（ADR-12）。
- Windows 无 POSIX 权限位，配置文件的 0600 只映射只读位；`Chmod` 失败不致命。

---

## 13. 参考路径速查（本机绝对路径）

**DSH 侧（读源码即读文档，全部带 `src/`）**

```
~/.dsh/profiles/web/package.json                 profile → bundles 声明
~/.dsh/profiles/web/cordis.patch.yml             用户 patch 层（插入你的插件）
~/.dsh/profiles/node_modules/@deepseek-ai/dsh-base/cordis.patch.yml
                                                 273 行真实装配样例，含 tool-* 注册与 config
~/.dsh/profiles/node_modules/@deepseek-ai/dsh-tool-todo/src/index.ts
                                                 ★ 最该先读：一个完整工具插件
~/.dsh/profiles/node_modules/@deepseek-ai/dsh-tools/src/index.ts
                                                 defineTool / ToolDefinition / 钩子
~/.dsh/profiles/node_modules/@deepseek-ai/dsh-tools/src/schema.ts
                                                 DefineToolOptions(483) / *SchemaSpec
~/.dsh/profiles/node_modules/@deepseek-ai/dsh-commands/src/index.ts
                                                 CommandDefinition(56) / parseCommand
~/.dsh/profiles/node_modules/@deepseek-ai/dsh-subprocess/src/types.ts
                                                 SubprocessSpawnSpec(75)
~/.dsh/profiles/node_modules/@deepseek-ai/dsh-skill-filesystem/src/index.ts
                                                 skill 根(241-263) / frontmatter 规则
~/.dsh/profiles/node_modules/@deepseek-ai/cordis-plugin-loader/README.md
                                                 入口选项 {id,name,config,group,disabled,inject}
~/.dsh/profiles/node_modules/@deepseek-ai/dsh-spill-policy/src/index.ts
                                                 大结果 spill / 预览
```

**本项目侧**

```
benchmark-prompts/plugins/pi/extension/bench-core.ts   ★ 直接复用的纯逻辑
benchmark-prompts/plugins/pi/extension/index.ts        工具/命令映射参考（pi 方言）
benchmark-prompts/plugins/pi/README.md                 安装、设计说明、已知限制
benchmark-prompts/scripts/smoke-pi.sh                  对照实验的写法模板
benchmark-prompts/scripts/capture-contract.sh          §9 数据的来源
docs/plugins.md   §1 CLI 契约、§5 统一模板、§7 边界情况
docs/client.md    §5 命令、§11 退出码（注意 §12-H 的缺口）
docs/api.md       冻结的 HTTP 契约（你不需要直接碰它）
```

**Pi 官方文档**（对照用）：随 Pi 发行版一同安装在
`<pi 安装目录>/docs/extensions.md`（用 `command -v pi` 定位后向上找）。
DSH 侧的等价资料就在本机：`~/.dsh/profiles/node_modules/@deepseek-ai/<包>/src/`。

---

## 14. 交接状态

| 项 | 状态 |
|---|---|
| 服务端 7 端点、鉴权、限流、gzip、ETag/304、增量、审核队列、指标、备份 | ✅ M2 |
| SDK `pkg/client` + `bench` CLI 全部子命令 | ✅ M3 |
| Pi 适配（5 工具 + 6 命令 + skill），真实 pi + 真实 LLM 验证 12/12 | ✅ M4 |
| DSH 框架调研 | ✅ **本文档，已实测核实** |
| DSH 插件实现 | ✅ 已完成（`benchmark-prompts/plugins/dsh/`，5 工具 + 6 命令 + subprocess 桥；真 DSH + 真 LLM 验证 `make smoke-dsh` 19/19） |
| 前端上 CDN | ⬜ M5 |
| 部署 + 监控 + 带宽看门狗阈值实测 | ⬜ M6 |

回归基线（改完必须仍然全绿）：`make check` 一次跑完
gofmt / vet / build / `go test -race`(13 包) / biome / `node --test`(pi 33 + dsh 7 + 插件层 23)；
外加 `smoke.sh` 45/45、`smoke-cli.sh` 35/35、`smoke-pi.sh` **12/12**、`smoke-dsh.sh` 19/19。

> **最后一条提醒**：本项目 M1 阶段的设计文档里，曾凭经验臆造过一个 DSH/Pi 的
> `tools:` YAML 插件配置形（带 `handler: run:`），事后被证明完全不存在。
> 那份错误已经在 `docs/plugins.md` §3 标注并更正。**接手时请以代码与本文件为准，
> 不要把 `docs/*.md` 早期段落当作事实来源。**