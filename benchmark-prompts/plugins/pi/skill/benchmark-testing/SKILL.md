---
name: benchmark-testing
description: 用 bench 工具集对本地 LLM 跑 benchmark 提示词测试。当用户想"随机测一条模型"、"按 id 一键测试"、"给刚才的测试结果打分"、"同步提示词库"或"上传自己的测试题"时使用。覆盖选题、原样投喂、评分、复核的完整流程。
---

# 本地 LLM Benchmark 测试

用 `bench_*` 工具（背后是 `bench` CLI + 远端提示词库）给本地模型跑标准化测试。

## 前提

若任一 `bench_*` 工具报 `bench_missing`，说明 CLI 还没装好，按提示处理：

```bash
cd benchmark-prompts && make build-cli
bench config init --endpoint <你的源站地址>   # 或设 BENCH_BIN 指向 dist/bench
```

## 标准流程

### 1. 取一条题

| 用户意图 | 调用 |
|---|---|
| "随机来一道" | `bench_random()` |
| "换个没测过的" | `bench_random(fresh=true)` |
| "来道编程题" | `bench_random(tag="coding")` |
| 用户给了 id | `bench_get(id="p_xxxxxxxx")` |
| 想先看目录挑 | `bench_catalog(action="list", tag="coding")` 再 `bench_get` |

**重要：提示词必须原样投喂。** 不要改写、不要总结、不要"顺手加个格式要求"——
一旦改动，测试结果就不可比。工具返回的正文在 `----` 分隔线之间。

### 2. 交给被测模型

把正文发给用户正在测的那个模型（本地 Ollama / llama.cpp / 其他 CLI）。
如果用户已经自己贴好并给出了回答，直接进入第 3 步。

### 3. 评分

```
bench_score(id="刚那条的 id", value=1-5)
```

打分要基于**回答是否满足提示词本身的要求**，不要因为回答长就给高分。
评分维度建议心里过一遍：指令遵循、正确性、完整性、是否啰嗦。

工具会返回该题的累计均分与人数，可据此和用户讨论"这次比上次好在哪"。
注意：同一设备重复对同一题打分是**覆盖**旧分，不会重复计数。

### 4. 连续多题

- 每次都用 `bench_random(fresh=true)`，避免重复抽到刚测过的
- 抽题与打分交替进行，别攒到最后凭记忆打分
- 测完一批后，用 `bench_catalog(action="status")` 看库有没有更新

## 维护类操作

| 场景 | 调用 |
|---|---|
| 本地库可能落后 | `bench_catalog(action="sync")` |
| 看同步状态 | `bench_catalog(action="status")` |
| 网络不通仍要取题 | `bench_get(id=..., local=true)`（读已同步的缓存） |
| 用户贡献新题 | `bench_upload(content=..., tags=["coding"])` |

`bench_upload` 提交后状态是 `pending`，**要等服务端审核通过才会公开**，
所以刚上传的题不能用 `bench_get` 取到（会 404），这是正常的，别当成故障。

## 常见错误怎么读

| 报错 | 含义 | 处理 |
|---|---|---|
| `当前没有可用的提示词` | 库空，或 `fresh=true` 排掉了全部最近条目 | 去掉 `fresh` 重试，或先 `action=sync` |
| `鉴权失败` | 写操作缺 API Key | 提示用户 `bench config init --key ...`；只读不受影响 |
| `被限流，稍后重试` | 触发配额 | 等几十秒再试，别循环重试 |
| `网络或服务端故障` | 源站不可达 | 用 `bench_get(local=true)` 走离线缓存 |
| `bench_missing` | 没装 CLI | 见上面「前提」 |

## 用户可直接用的斜杠命令

不用经过 LLM，用户自己敲：`/bench-random [tag]`、`/bench-get <id>`、
`/bench-score <id> <1-5>`、`/bench-sync`、`/bench-status`、`/bench-list [tag]`。
其中取题类会把**完整正文填进输入框**，方便直接复制去喂被测模型。