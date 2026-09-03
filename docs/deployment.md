# 部署、运维与监控

## 1. 构建与发布产物

**不要手写 `go build` 发布** —— 本文早期版本给的就是不带 `-X` 注入的写法，
产物会全部报 `version: dev`，上线后无法回答“这台机器跑的是哪一版”。用下面的入口：

```bash
make version            # 看将要注入的 VERSION / COMMIT / DATE 与来源
make build-all          # 仅交叉编译（bench 5 平台 + server 3 平台）
make release            # 编译 + 打包 tar.gz + sha256sums.txt + RELEASE-INFO
make release-verify     # 验证：字节级注入证据 + 校验值 + 归档结构 + 本机真跑
```

版本单一来源：**`VERSION` 文件**；在 git 环境里自动用 `git describe` 覆盖。
注入目标是 `internal/buildinfo`（Version / Commit / Date），两个二进制共用同一个包。

### 产物矩阵

| 产物 | 平台 | 备注 |
|---|---|---|
| `bench-$V-*.tar.gz` | linux amd64/arm64、darwin amd64/arm64、windows amd64 | 归档内就叫 `bench`（+`.exe`），附 `VERSION` |
| `bench-server-$V-*.tar.gz` | linux amd64/arm64、darwin arm64 | 附 `config.example.yaml` |
| `dist/sha256sums.txt` | — | 对**归档**而非裸二进制计算（用户下载的是归档） |
| `dist/RELEASE-INFO` | — | 记 version/commit/build_date/平台清单，验证器读它 |

### 为什么验证器能确认“跳平台产物的版本是对的”

曾想用 `go version -m` 读 build info —— **实测不成立**：go 1.27 的 build info
只记 `-buildmode`/`-compiler`/`-trimpath` 与 `GO*` 环境变量，**不记 `-ldflags`**。

改用字节级证据：`-X` 注入的 **构建时间戳**（秒级、本次唯一）必须出现在目标二进制里。
再叠加“本机那一个真跑起来报告同一版本”作为机制正确性的强证据；
并对该检查做过**对照实验**：抽掉 `-X` 重建一个产物，验证器确实只对它报错。

### 用户侧安装校验

```bash
grep bench-v0.1.0-linux-amd64 sha256sums.txt | sha256sum -c -
tar -xzf bench-v0.1.0-linux-amd64.tar.gz
./bench-v0.1.0-linux-amd64/bench version      # 期望真实版本号，而不是 dev
```

## 2. 源站部署（systemd）

```
/etc/bench/config.yaml        # 配置（含 TLS 路径）
/var/lib/bench/bench.db       # SQLite
/etc/bench/tls.crt|key        # 证书
/usr/local/bin/bench-server   # 二进制
```

```ini
# /etc/systemd/system/bench.service
[Unit]
Description=Benchmark Prompts API
After=network.target

[Service]
User=bench
ExecStart=/usr/local/bin/bench-server -config /etc/bench/config.yaml
Restart=on-failure
RestartSec=3
LimitNOFILE=65536
NoNewPrivileges=true
ProtectSystem=strict
ReadWritePaths=/var/lib/bench

[Install]
WantedBy=multi-user.target
```

```bash
systemctl enable --now bench
journalctl -u bench -f      # 看日志（结构化 slog）
```

## 3. 备份

```bash
# /etc/cron.d/bench-backup（每日 03:00；用服务端自带 backup 子命令，纯 Go 驱动、不依赖 sqlite3 CLI）
0 3 * * * bench /usr/local/bin/bench-server -backup /backup/bench-$(date +\%F).db
# 清理 7 天前
0 4 * * * bench find /backup -name 'bench-*.db' -mtime +7 -delete
```

> `bench-server -backup <path>` 为服务端维护子命令：内部用同一纯 Go SQLite 驱动执行 `VACUUM INTO`，避免额外安装 sqlite3 CLI。目标路径已存在时**会报错而不是静默覆盖**（已被单测固定）。

其余运维子命令（同一二进制，无需另建后台系统）：

```bash
bench-server -config c.yaml -put-key "alice:<key>:<secret>"  # 登记 API Key
bench-server -config c.yaml -review                         # 查看待审核队列
bench-server -config c.yaml -approve p_xxxxxxxx             # 审核通过
bench-server -config c.yaml -reject  p_xxxxxxxx             # 审核打回
```

这些命令可与运行中的服务**共用同一个 DB 文件**（WAL + `busy_timeout=5000`），
冒烟测试已跨进程验证。主密钥仅从环境变量 `BENCH_SECRET_KEY`（32 字节 hex）读取。

```bash
```

## 4. 监控（重点：2M 带宽）

| 指标 | 来源 | 告警阈值 |
|------|------|----------|
| 出站带宽速率 | 云监控 / 自建 node_exporter | > 1.5 Mbps 持续 5 分钟 |
| API 请求量 & P99 | 服务 `/ -/metrics` | P99 > 1s |
| 304 命中率 | 访问日志 | < 30%（说明客户端缓存没生效） |
| 429/限流次数 | 服务指标 | 突增 → 疑似被刷 |
| 错误率（5xx） | 访问日志 | > 5% |
| 审核队列积压 | `count(status=pending)` | > 100 |

- 轻量方案：Prometheus + Grafana 太重（2H2G 不必）；用 **云监控 + 服务自曝 `/ -/metrics` + 简单告警脚本**即可。
- 带宽告警是第一优先级：2M 是硬约束，被刷爆会全站不可用。

## 5. 安全加固

1. **最小权限**：服务以专用 `bench` 用户运行，`NoNewPrivileges`、`ProtectSystem=strict`。
2. **只开 443**：防火墙仅放行 443；`/ -/metrics` 仅内网/管理 Key。
3. **密钥管理**：API key 存 `sha256`；HMMS `secret` 加密存储；生产用环境变量注入。
4. **TLS**：强制 HTTPS，HSTS 头。
5. **限流**：见 server.md §4 + 分级配额（api.md §6）。
6. **输入消毒**：正文、标签、ID 全量校验，防 SQL 注入（用参数化查询）、防 XSS（前端转义）。
7. **依赖审计**：`govulncheck` 定期扫。

## 6. 按 2M 带宽的容量策略

| 压力等级 | 表现 | 应对 |
|----------|------|------|
| 正常 | < 200 KB/s | 无需干预 |
| 偏高 | 200–450 KB/s | 观察，延长 list/delta 冷却 |
| 预警 | > 450 KB/s | 触发看门狗降级 delta/list |
| 过载 | 接近 1.6 Mbps | 只保 get/random/meta，其余 503 |

## 7. 上线检查清单

- [ ] 部署前先 `make release && make release-verify`，**产物未经验证不上传**
- [ ] 部署后跑 `bench-server -version`，确认版本号与构建时间与本次发布一致
      （而不是靠“我刚刚传的文件应该是新的”这种记忆）
- [ ] 前端静态资源已上 CDN 且源站不服务 web/*
- [ ] gzip 生效（curl -H 'Accept-Encoding: gzip' -I 验证 Content-Encoding）
- [ ] meta/get 返回 ETag，且 If-None-Match → 304
- [ ] 写入端点鉴权 + 限流生效
- [ ] 备份 cron 已配
- [ ] 带宽告警已接
- [ ] TLS/HSTS 生效