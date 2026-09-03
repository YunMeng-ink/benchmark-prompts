# benchmark-prompts

个人测试用 **benchmark 提示词**平台：一个源站 API、一个 `bench` CLI、
以及 **Pi** 与 **DSH（DeepSeek Harness）** 两个 Agent 框架的薄适配插件。

面向想在本地快速测一测模型能力的人：一句话抽题、原样投喂、给个 1–5 分。

- **源站**：香港轻量服务器 2H2G + 10M 带宽 + 亚太 CDN（无备案）。
  带宽是有限资源，所以前端全量走 CDN（零回源）、API 用 gzip + ETag/304 + 增量同步。
  阈值与容量策略只在 `docs/deployment.md` §6 定义，别处不重复这个数。
- **技术栈**：Go + SQLite（仅 2 个直接依赖），纯 Go 驱动无需 CGO，交叉编译即得全平台单文件。
- **多框架适配的解法**：能力全部下沉到 `bench` CLI，插件里**零业务逻辑**，
  只负责“调命令 + 展示结果”。这是唯一能跨语言（Go ↔ TS）复用的边界。

## 快速上手

```bash
# 下载对应平台的产物（解压后二进制就叫 bench）
tar -xzf bench-v0.1.0-linux-amd64.tar.gz
./bench-v0.1.0-linux-amd64/bench version        # 应报告真实版本号，而不是 dev

# 配置源站（只读能力匿名可用；打分/上传需要 Key）
bench config init --endpoint https://<源站地址> [--key <K> --secret <S>]

# 抽一条来测
bench random --json
```

校验下载完整性：

```bash
grep bench-v0.1.0-linux-amd64 sha256sums.txt | sha256sum -c -
```

## 在 Agent 框架里用

| 框架 | 位置 | 能力 |
|---|---|---|
| [pi](https://github.com/badlogic/pi) | [`benchmark-prompts/plugins/pi/`](./benchmark-prompts/plugins/pi/) | 5 工具 + 6 斜杠命令 + skill |
| DSH（DeepSeek Harness） | [`benchmark-prompts/plugins/dsh/`](./benchmark-prompts/plugins/dsh/) | 同上（Cordis 插件） |

两个框架**共用同一份 `SKILL.md`**，装好插件即可用一句话触发“一键/随机测试”。

## 仓库结构

```
docs/                  开发文档（先读 docs/README.md，内含“谁是事实来源”层级）
benchmark-prompts/     代码：cmd/ internal/ pkg/client/ plugins/ web/ scripts/
```

想读文档的话，顺序是
[docs/README.md](./docs/README.md) →
[api.md](./docs/api.md)（冻结契约）→
[architecture.md](./docs/architecture.md) →
[client.md](./docs/client.md) →
[plugins.md](./docs/plugins.md)。

从零构建与验证请看
[benchmark-prompts/README.md](./benchmark-prompts/README.md)
（`make check` 一条命令跑完 Go + TS 全部门禁）。

## 自测与发布

```bash
cd benchmark-prompts
make check             # gofmt / vet / staticcheck / build / race / biome / TS 单测
make smoke             # 服务端端到端（真实 HTTP + 真实 SQLite）
make smoke-cli         # CLI 二进制端到端
make release           # 全平台交叉编译 + tar.gz + sha256sums.txt
make release-verify    # 验证产物（字节级版本注入证据 + 校验值 + 真跑）
```

## License

[MIT](./benchmark-prompts/LICENSE)