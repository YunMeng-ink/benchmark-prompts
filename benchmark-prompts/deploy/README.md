# deploy/ —— 源站部署资产

面向的目标形态：**Ubuntu 22.04，入站只开 22/80/443，nginx 反代 + certbot 续期，
数据放数据盘 `/data`**，TLS 在 nginx 终结、源站只听 `127.0.0.1:8080`。

**步骤、判据、上线清单的唯一权威是 [docs/deployment.md](../../docs/deployment.md)**
（§2 部署与 nginx、§3 备份、§6 带宽阈值、§7 前端与 CDN、§8 检查清单）。
本目录只放**会被 `cp` 到系统位置的文件本体**，不重复写流程 —— 两处各写一份必然漂移，
deployment.md 早期内联的那份单元草稿就与实际交付件在路径和单元名上不一致过。

| 文件 | 装到 | 作用 |
|---|---|---|
| `bench-server.service` | `/etc/systemd/system/` | 源站进程。`ProtectSystem=strict` + 白名单 `ReadWritePaths=/data/bench`，`MemoryMax=768M` |
| `bench-server.env.example` | `/etc/bench/server.env`（0600 root:root） | 只放 `BENCH_SECRET_KEY`。缺它源站启动即失败 |
| `nginx-bench.conf` | `/etc/nginx/sites-available/bench` | 80 跳转 + 443 反代。带 `X-Forwarded-For`，**没带会让全站访客共用一个限流桶** |
| `bench-backup.sh` | `/data/bench/bin/`（0750） | `VACUUM INTO` 一致性快照，原子改名 + 14 天保留 + 0600 |
| `bench-backup.service` `.timer` | `/etc/systemd/system/` | 每日 03:15 触发上面的脚本，`Persistent=true` 补跑错过的 |
| `trusted-proxies.cdn.yaml` | 内容并入 `/data/bench/config.yaml` | CDN 回源网段（234 条，精确覆盖）。缺了它，同一 CDN 节点后的访客会共用一个限流桶 |

## 三条最容易配错的

1. **`server.trusted_proxies`**：只有直连对端在该网段内才采信转发头。默认仅回环；
   显式填写是**整体替换**，漏掉 `127.0.0.0/8` 就等于谁都不信 → 全站并成一个桶。
   前面还有 CDN 时，要把厂商公布的回源网段一并加进来。
   规则细节见 [docs/server.md](../../docs/server.md) §4.1。
2. **nginx 必须带 `X-Forwarded-For`**：用 `$proxy_add_x_forwarded_for`，不是 `$remote_addr`
   （后者在有 CDN 时只会记到 CDN 出口 IP）。
3. **别在 nginx/CDN 上另设一套缓存头**：入口 HTML 要 `must-revalidate` 语义，
   被兜住就会「发版了但页面没变」。缓存分级由源站 handler 决定并透传。

## 交叉编译

产物是 `CGO_ENABLED=0` 的纯 Go 静态二进制（SQLite 驱动 `modernc.org/sqlite` 无 C 依赖），
**与目标机的 glibc 版本无关**，所以 22.04 / 24.04 / 更老内核都能直接跑同一个文件：

```bash
make build-all          # dist/bench-server-linux-amd64 等，同时产出 bench CLI
```

有 Linux（含 WSL）时在上传前用**即将部署的那个 ELF**跑一遍真实端到端：

```bash
bash scripts/verify-linux.sh
```

它验 API、静态托管的三级缓存头、`/v1` 不被静态兜底吞掉、以及可信/不可信对端下
的客户端 IP 采信 —— 这些都是 Windows 上的冒烟脚本证明不了的（ELF 跑不起来）。
