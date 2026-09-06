# 部署、运维与监控

## 1. 构建与发布产物

**不要手写 `go build` 发布** —— 本文早期版本给的就是不带 `-X` 注入的写法，
产物会全部报 `version: dev`，上线后无法回答“这台机器跑的是哪一版”。用下面的入口：

```bash
make version            # 看将要注入的 VERSION / COMMIT / DATE 与来源
make build-all          # 仅交叉编译（bench 5 平台 + server 3 平台）
make release            # 编译 + 打包 tar.gz + sha256sums.txt + RELEASE-INFO
make release-verify     # 验证：字节级注入证据 + 校验值 + 归档结构 + 真跑产物
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
再叠加“取其中一个真跑起来报告同一版本”作为机制正确性的强证据；
并对该检查做过**对照实验**：抽掉 `-X` 重建一个产物，验证器确实只对它报错。

### 用户侧安装校验

```bash
grep bench-v0.1.0-linux-amd64 sha256sums.txt | sha256sum -c -
tar -xzf bench-v0.1.0-linux-amd64.tar.gz
./bench-v0.1.0-linux-amd64/bench version      # 期望真实版本号，而不是 dev
```

## 2. 源站部署（Ubuntu 22.04 + nginx + certbot）

### 2.1 目标形态

只有 22/80/443 对外开放，因此 **TLS 在 nginx 终结，源站监听 `127.0.0.1:8080` 明文**，
数据放数据盘 `/data`。源站二进制在没配证书时走 `ListenAndServe`，不需要 `-dev`
（`-dev` 只是把日志换成文本，见 `server.md` §8）。

```
/data/bench/bin/bench-server          二进制（linux/amd64，静态，无 glibc 依赖）
/data/bench/bin/bench-backup.sh       备份脚本
/data/bench/config.yaml               配置，0640 root:bench
/data/bench/data/bench.db             SQLite（含 -wal/-shm）
/data/bench/web/                      前端产物（源站托管，CDN 缓存）
/data/bench/backups/                  每日一致性快照，0600
/etc/bench/server.env                 BENCH_SECRET_KEY，0600 root:root
/etc/systemd/system/bench-server.service
/etc/nginx/sites-available/bench
```

单元、env 模板、nginx 站点、备份 service/timer 的**权威内容在仓库 `deploy/` 目录**，
本文只写步骤与判据 —— 此前本节内联过一份单元草稿，它与实际交付的文件在路径和
单元名上都不一致（且缺 `EnvironmentFile`，照抄会启动失败），故改为指向。

| 文件 | 装到 |
|---|---|
| `deploy/bench-server.service` | `/etc/systemd/system/bench-server.service` |
| `deploy/bench-server.env.example` | `/etc/bench/server.env`（填值后 0600） |
| `deploy/nginx-bench.conf` | `/etc/nginx/sites-available/bench` |
| `deploy/bench-backup.service` `.timer` | `/etc/systemd/system/` |
| `deploy/bench-backup.sh` | `/data/bench/bin/bench-backup.sh`（0750） |

### 2.2 一次性初始化

```bash
# 0) 交叉编译产物（在开发机上做，产物静态、与目标 glibc 无关）
cd benchmark-prompts && make build-all        # dist/bench-server-linux-amd64

# 1) 用户与数据盘目录
sudo useradd --system --home /data/bench --shell /usr/sbin/nologin bench
sudo install -d -m 750 -o bench -g bench /data/bench/{bin,data,web,backups}
sudo install -d -m 755 -o root -g root /etc/bench

# 2) 二进制、脚本、前端产物
sudo install -m 755 -o root -g root dist/bench-server-linux-amd64 /data/bench/bin/bench-server
sudo install -m 750 -o root -g bench deploy/bench-backup.sh /data/bench/bin/bench-backup.sh
rsync -a --delete web/dist/ /data/bench/web/      # 需 sudo，目录属 root

# 3) 配置与密钥
sudo -e /data/bench/config.yaml                   # 以 config.example.yaml 为底
sudo install -m 600 -o root -g root /dev/null /etc/bench/server.env
printf 'BENCH_SECRET_KEY=%s
' "$(openssl rand -hex 32)" | sudo tee /etc/bench/server.env
sudo chown root:root /data/bench/config.yaml && sudo chmod 640 /data/bench/config.yaml

# 4) systemd
sudo cp deploy/bench-server.service /etc/systemd/system/
sudo systemctl daemon-reload && sudo systemctl enable --now bench-server
systemctl status bench-server --no-pager | head -5
```

`config.yaml` 至少要有这四条（其余用默认值）：

```yaml
server:
  addr: "127.0.0.1:8080"
  static_dir: /data/bench/web
  trusted_proxies: ["127.0.0.0/8"]   # 见 §2.4，前面还有 CDN 时必须加段
store:
  path: /data/bench/data/bench.db
```

### 2.3 nginx 与证书

```bash
sudo cp deploy/nginx-bench.conf /etc/nginx/sites-available/bench
sudoedit /etc/nginx/sites-available/bench        # 把 bench.example.com 换成真实域名
sudo ln -s /etc/nginx/sites-available/bench /etc/nginx/sites-enabled/bench
sudo rm -f /etc/nginx/sites-enabled/default      # 否则默认站点会抢下 80 的匹配
sudo nginx -t && sudo systemctl reload nginx

sudo apt-get install -y certbot python3-certbot-nginx
sudo certbot --nginx -d bench.example.com --redirect -m you@example.com --agree-tos -n
sudo certbot renew --dry-run                     # 续期由它的 systemd timer 负责
```

`--nginx` 会把证书路径写进同一个站点文件，`--redirect` 加 80→443 跳转，
所以 §2.1 的模板在 certbot 之后以 certbot 的结果为准。

### 2.4 客户端 IP（反代下最容易配错的一处）

限流主体与审计日志的 `ip` 字段都取自客户端地址。规则是
**只有直连对端落在 `server.trusted_proxies` 内时才采信 `X-Forwarded-For`**，
采信时从右往左取第一个非可信跳。默认值仅回环，正好匹配“nginx 与源站同机”。

由此有两条会真实踩到的后果：

- nginx 没带 `X-Forwarded-For` → 全站访客都记成 `127.0.0.1`，共用同一个匿名桶，
  匿名配额瞬间打满（表现为人人 429）。
- 前面还有一层 CDN → CDN 出口 IP 也是“非可信跳”，会被当成客户端地址，
  于是同一 CDN 节点后的所有用户共享一个桶。要把 **CDN 厂商公布的回源网段**
  一并加进 `trusted_proxies`，链才会继续左移到真实客户端。

```yaml
trusted_proxies: ["127.0.0.0/8", "203.0.113.0/24"]   # 后者换成 CDN 回源段
```

填了这个列表就是**整体替换**默认值：回环不再自动可信，别漏写 `127.0.0.0/8`。

## 3. 备份

首选交付好的 systemd timer（时间、保留天数、原子落盘都已处理）：

```bash
sudo cp deploy/bench-backup.service deploy/bench-backup.timer /etc/systemd/system/
sudo systemctl daemon-reload && sudo systemctl enable --now bench-backup.timer
systemctl list-timers bench-backup.timer                      # 下次触发时间
sudo systemd-run --unit=backup-now --same-dir /data/bench/bin/bench-backup.sh   # 立刻验一次
ls -l /data/bench/backups
```

`deploy/bench-backup.sh` 的三点设计：写同目录临时文件再 `mv`（同盘 rename 原子，
目录里出现的快照一定完整）、保留 14 天（`BENCH_BACKUP_KEEP_DAYS` 可覆盖）、
落盘后 `chmod 600`。用 cron 也可以，语义相同：

```bash
15 3 * * * bench /data/bench/bin/bench-backup.sh
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

## 4. 监控（重点：源站出站带宽）

> **本表与 §6 是全仓库唯一的带宽阈值权威定义**（约定见 `README.md` §0.5）。
> 代码默认值 `bandwidth.max_mbps` 与 `config.example.yaml` 都按 §6 推导，
> 其他文档只写“见 `deployment.md` §6”，不再抄一份数。

| 指标 | 来源 | 告警阈值 |
|------|------|----------|
| 出站带宽速率 | 云监控 / 自建 node_exporter | > 8 Mbps 持续 5 分钟 |
| API 请求量 & P99 | 服务 `/ -/metrics` | P99 > 1s |
| 304 命中率 | 访问日志 | < 30%（说明客户端缓存没生效） |
| 429/限流次数 | 服务指标 | 突增 → 疑似被刷 |
| 错误率（5xx） | 访问日志 | > 5% |
| 审核队列积压 | `count(status=pending)` | > 100 |

- 轻量方案：Prometheus + Grafana 太重（2H2G 不必）；用 **云监控 + 服务自曝 `/ -/metrics` + 简单告警脚本**即可。
- **带宽告警仍是第一优先级**：刷满会触发看门狗降级，但降级本身就是可用性损伤，
  不能拿“反正有看门狗”当不设告警的理由。

## 5. 安全加固

1. **最小权限**：服务以专用 `bench` 用户运行，`NoNewPrivileges`、`ProtectSystem=strict`。
2. **入站只有 22/80/443**：80 仅用于 ACME 与跳转，业务流量走 443；
   源站监听 `127.0.0.1`，外部无法绕过 nginx 直连；`/-/metrics` 需要 `admin` 作用域的 Key。
3. **密钥管理**：API key 存 `sha256`；HMAC `secret` 加密存储；生产用环境变量注入。
   凭据发放走邀请码，运维者**不需要**再逐条 `-put-key`：

   ```bash
   # 主密钥必须在（签发 Key/邀请码都要加密能力）
   export BENCH_SECRET_KEY=<随机 64 hex>
   bench-server -config c.yaml -gen-invite "群发:20:30"   # label:次数:有效天数
   bench-server -config c.yaml -list-invites              # 看谁用了几个
   bench-server -config c.yaml -list-keys                 # 只给哈希前缀，明文不可恢复
   bench-server -config c.yaml -revoke-key cb4f408e3095   # 出问题时按句柄吊销
   ```

   自助注册的 Key 一律是 `writer`：能打分/上传，**读不到 `/-/metrics`**。
   需要管理能力的 Key 只能由运维者 `-put-key` 签发（scope=admin）。
   邀请码与 Key 的明文都只在签发那一刻出现一次，丢失只能重发。
4. **TLS**：nginx 终结，certbot 申请并自动续期；站点文件带 HSTS 头。
   源站不持有证书，也就没有“证书过期导致源站起不来”这类故障。
5. **限流**：见 `server.md` §4 + 分级配额（`api.md` §6）。
6. **输入消毒**：正文、标签、ID 全量校验，防 SQL 注入（用参数化查询）、防 XSS（前端转义）。
7. **依赖审计**：`govulncheck` 定期扫。

## 6. 按套餐带宽的容量策略

当前套餐 **10 Mbps ≈ 1250 KB/s**（十迕换算）。看门狗阀值取 0.8 倍：
`max_mbps: 8.0`（= `internal/config` 默认值，也是 `config.example.yaml` 里的值）。

| 压力等级 | 出站速率 | 应对 |
|----------|----------|------|
| 正常 | < 250 KB/s（< 2 Mbps） | 无需干预 |
| 偏高 | 250–600 KB/s（2–5 Mbps） | 观察，延长 list/delta 冷却 |
| 预警 | 600–1000 KB/s（5–8 Mbps） | 人工关注；看门狗尚未触发 |
| 降级 | ≥ 1000 KB/s（≥ 8 Mbps） | 看门狗触发：对 delta/list 降级限流 |
| 过载 | 接近 1250 KB/s（10 Mbps 打满） | 云侧限速生效，全站不可用风险 |

> 换套餐只改两处：本节数字 + `bandwidth.max_mbps`。其他文档不得出现阀值常量。

## 7. 前端：源站托管 + CDN 缓存分发（唯一权威）

前端产物由**源站**提供，CDN 挡在前面缓存。命中不回源，未命中才消耗源站出口，
所以 CDN 命中率是这套形态的关键指标。

1. 构建：`make web-build`（顺带打印体积报告）。
2. 同步到源站：把 `web/dist/` 整目录放到服务器上的静态目录（如
   `/var/lib/bench/web`），并在 `config.yaml` 里指向它：

   ```yaml
   server:
     static_dir: /var/lib/bench/web
   ```

   留空即回到"源站只出 API"的形态（前端整体放对象存储/纯 CDN 时用）。
3. 缓存策略由源站直接下发 `Cache-Control`，分三级（实现见 `server.md` §8）：

   | 路径 | `Cache-Control` | 理由 |
   |---|---|---|
   | `/_astro/*`（文件名带内容 hash） | `public, max-age=31536000, immutable` | 内容变了文件名就变，可永久缓存 |
   | `/`、`/index.html` | `public, max-age=0, must-revalidate` + 弱 ETag | 发版必须立刻生效；未变时靠 304 省字节 |
   | `/runtime-config.js` 等 | `public, max-age=300, must-revalidate` | 部署期可直接改文件，但要能较快生效 |

   CDN 侧对这三类**必须透传**源站的 `Cache-Control`，不要在 CDN 上另设一套
   更长的入口缓存——那会让发版看起来"没生效"。
4. CDN 配置要点：
   - 回源地址 = 源站；`/_astro/*` 按目录设长缓存，`/` 与 `/runtime-config.js` 设
     不缓存或极短缓存。
   - 开启 gzip/br 与 `Vary: Accept-Encoding`（源站已带该头，别在 CDN 丢掉）。
   - **`/v1/*` 与 `/-/*` 不要缓存**（源站已给 `no-store`/短 TTL，但 CDN 规则要显式排除）。
   - 发版后若入口未刷新，用 CDN 的 URL/目录刷新，而不是改文件名。
5. 前端与 API 同域（都走这个域名）时浏览器**不发跨域请求**，CORS 不再是必要条件。
   若 CDN 域名与 API 域名不同，才需要把前端域名加进 `cors.allowed_origins`
   （精确值，别留 `*`，因为前端能带 Key 写入）。
6. 自检：
   - `curl -I https://域名/` → 200、`Content-Encoding: gzip`、`Cache-Control` 含 `must-revalidate`
   - `curl -I https://域名/_astro/<某个hash>.js` → `immutable`
   - 带 `If-None-Match` 再请求首页 → 304
   - `curl https://域名/v1/不存在` → 仍是 `not_found` 信封，**不是 HTML**

`make smoke-web` 用真源站把这四条连同资产图一起断言。

## 8. 上线检查清单

上传前（开发机）：

- [ ] `make release && make release-verify` 通过 —— **产物未经验证不上传**
- [ ] `make smoke-web` 通过（含源站托管与缓存头三级）
- [ ] 有 Linux 环境时跑 `bash scripts/verify-linux.sh`，它用**即将上传的那个 ELF**
      在真实 Linux 上验 API + 静态托管 + IP 采信

上传后（源站）。每条都是能直接贴的命令，判据写在看不到就该停手的项上：

- [ ] `bench-server -version` 的版本号与构建时间与本次发布一致
      （不靠“刚传的文件应该是新的”这一印象）
- [ ] `systemctl is-active bench-server` = `active`，且
      `journalctl -u bench-server -b --no-pager | grep 服务启动` 里 `addr=127.0.0.1:8080`
- [ ] 源站托管前端生效：`curl -sI 127.0.0.1:8080/ | grep -E 'HTTP|Cache-Control'`
      返回 200 + `must-revalidate`
- [ ] 静态兜底没吞 API：`curl -s 127.0.0.1:8080/v1/does-not-exist` 返回
      `not_found` **信封**，不是 HTML
- [ ] `curl -sI https://域名/` 与 `curl -sI https://域名/_astro/任一片段.js`
      分别带 `must-revalidate` 与 `immutable`
- [ ] gzip：`curl -sI -H 'Accept-Encoding: gzip' https://域名/v1/prompts | grep -i content-encoding`
- [ ] ETag/304：取 `meta` 的 ETag 后 `curl -sI -H 'If-None-Match: <etag>'` 得 304
- [ ] **真实客户端 IP**：`curl -s https://域名/v1/prompts >/dev/null` 之后
      `journalctl -u bench-server -n 5 --no-pager | grep -o 'ip=[^ ]*'`
      不能是 `127.0.0.1`（是则 nginx 没带 `X-Forwarded-For`，见 §2.4）
- [ ] 写入端点鉴权 + 限流生效；`/-/metrics` 用 `writer` Key 应 403
- [ ] `bench-backup.timer` 在 `list-timers` 里，且手工跑一次能出 0600 快照
- [ ] `certbot renew --dry-run` 通过
- [ ] 带宽看门狗告警已接（阈值权威在 §6）

CDN 在前面时另外确认：回源网段已加进 `trusted_proxies`（§2.4）、
入口 HTML 不被 CDN 长缓存、断源时 CDN 能继续供静态骨架。
