# Pi 适配

给 [pi-coding-agent](https://github.com/badlogic/pi) 的 benchmark 提示词扩展。

**本目录不含业务逻辑**：所有能力都在 `bench` CLI 里，这里只是薄胶水。
同一套 `bench` 契约已被两个框架共同验证：本目录（Pi）与
[`../dsh/`](../dsh/README.md)（DSH，Cordis 插件）。两者形状完全不同
（TypeBox vs schemastery；返回 content vs 只返回受 `output.schema` 校验的 JSON），
但共担同一份 `bench-core.ts` 纯逻辑 —— 这正是“能力下沉 CLI”决策（ADR-6）的预期收益。

```
plugins/pi/
├── extension/
│   ├── index.ts             Pi 扩展入口：5 个工具 + 6 个斜杠命令
│   ├── bench-core.ts        纯逻辑：argv 构造、错误映射、截断（不依赖 pi，可单测）
│   └── bench-core.test.ts   node --test 单测 + 对真实 bench 二进制的集成测试
└── skill/
    └── benchmark-testing/
        └── SKILL.md         教 agent 怎么走"取题→原样投喂→打分"流程
```

## 前提

1. 构建 CLI：`make build-cli` → `dist/bench`（Windows 为 `dist/bench.exe`）
2. 让它可被找到：加入 `PATH`，或设 `BENCH_BIN=/绝对路径/dist/bench`
3. 配置一次：`bench config init --endpoint <源站地址> [--key <key> --secret <secret>]`

只读能力（随机/取题/浏览/同步）匿名即可；打分与上传需要 Key。

## 安装

```bash
# 方式一：临时加载（试用最方便）
pi -e ./plugins/pi/extension/index.ts --skill ./plugins/pi/skill/benchmark-testing

# 方式二：装到用户目录，自动发现
cp -r plugins/pi/extension ~/.pi/agent/extensions/bench
cp -r plugins/pi/skill/benchmark-testing ~/.pi/agent/skills/

# 方式三：只给当前项目用（需信任项目）
cp -r plugins/pi/extension .pi/extensions/bench
cp -r plugins/pi/skill/benchmark-testing .pi/skills/
```

> 扩展通过 jiti 直接跑 TypeScript，**不需要编译步骤**。

## 提供的能力

### 工具（LLM 可调用）

| 工具 | 作用 | 关键参数 |
|---|---|---|
| `bench_random` | 随机取一条（随机测试） | `tag`、`fresh` |
| `bench_get` | 按 id 取一条（一键测试） | `id`、`local` |
| `bench_score` | 打 1-5 分 | `id`、`value` |
| `bench_catalog` | 浏览 / 同步 / 状态 | `action=list\|sync\|status`、`tag`、`limit` |
| `bench_upload` | 上传新提示词（进审核队列） | `content`、`tags`、`client_id` |

### 斜杠命令（用户直接触发）

`/bench-random [tag]`、`/bench-get <id>`、`/bench-score <id> <1-5>`、
`/bench-sync`、`/bench-status`、`/bench-list [tag]`

取题类命令会把**完整正文放进输入框**（而不是塞进单行通知），方便复制去喂被测模型。

## 验证

```bash
make test-ts       # bench-core 单测 + 真实 bench 二进制集成测试（pi 33 项；另有 dsh 侧 7+23）
make smoke-pi      # 真实 pi 加载扩展并让 LLM 实际调用工具（12 项，含失败分支）
```

`make smoke-pi` 做的是**对照实验**：先加载一个故意写坏的扩展，确认 pi 会报
"Failed to load extension"，再加载本项目扩展并断言无此报错——否则“没报错”
不构成任何证据。它还会真的让模型调用 `bench_random`，检查取回的是源站上那条
带唯一标记的提示词，而不是模型自己编的文本。

两处防假通过的设计（均自 DSH 侧实测经验同步过来）：

- **`--tools` 白名单**堵死旁路：否则模型可以自己跑 `bench` 取数据，
  “取回源站标记”就不再证明工具链路走通了。
- **失败分支断言的判定顺序**：先查错误标记、再查探针词。模型正确拒绝时会
  原话引用探针词（“因此不能回答 TOOL-OK”），顺序写反会把正确行为判成失败。

pi 不可用或没有模型凭据时该脚本自动 SKIP 并以 0 退出，不阻塞 CI。

## 设计说明

- **为什么 shell 调 CLI 而不是直接用 HTTP**：缓存、增量同步、ETag、重试、签名
  这些逻辑必须在多处复用（CLI / Pi / DSH / 前端脚本）。放进 Go CLI 一份实现，
  各框架只剩胶水；否则每个框架都要重写一遍同步协议。
- **`bench-core.ts` 单独成文件**：Pi 的类型声明没有随 Scoop 发行版落地
  （无 `dist/*.d.ts`），扩展入口无法本地类型检查。把最易出错的 argv 构造、
  退出码→语义映射、输出截断抽成零依赖模块，就能被 `node --test` 真实覆盖。
- **错误一律 throw**：Pi 的约定是"`execute` 抛异常才会被标记为 `isError`“，
  return 一个带 error 字段的对象不会生效。
- **输出必截断**：自定义工具不限长会把整个提示词库灌进上下文。正文限 120 行 / 12KB，
  并显式告知省略了多少行。
- **不在工厂函数里探测二进制**：Pi 文档警告工厂可能在”永不开会话"的调用中执行；
  首次用到时懒探测，且失败不缓存（用户装好后无需重启 pi）。

## 已知限制

- 扩展入口本身没有本地类型检查（原因见上）；类型不匹配只会在 `tsc` 或运行时暴露，
  而 pi 用 jiti 加载不做类型检查。`make smoke-pi` 是当前的实际防线。
  （对比：DSH 包自带 `.d.ts` 与 `src/`，所以 DSH 侧**可以**真类型检查 ——
  `make typecheck-dsh`。同一个缺陷在两个框架上的代价并不对称。）
- `bench-core.ts` 与 `../dsh/bench-core.ts` 是**副本关系**（装载机制不同，无法共享模块）。
  改动必须双向同步，漂移由 **sha256 哈希钉测试** 抖红（见 `../dsh/README.md`）。
- 工具调用会启动一个 `bench` 子进程（约 30ms）。对交互式测试完全够用，
  但不适合高频循环调用。
- 斜杠命令在 `-p`（print）模式下 `hasUI=false`，通知不会显示；工具路径不受影响。