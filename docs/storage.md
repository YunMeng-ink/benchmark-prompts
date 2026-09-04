# 数据模型与存储

## 1. 存储选型

- **数据库**：SQLite（单文件 `/var/lib/bench/bench.db`），驱动 `modernc.org/sqlite`（纯 Go）。
- **迁移**：SQL 文件内嵌于 `internal/store/migrations/NNNN_*.sql`（`go:embed`，因此不依赖工作目录），按文件名顺序执行，`schema_migrations` 表记录版本；启动时若 `store.migrate=true` 自动迁移，幂等可重跑。
- **写并发**：SQLite 写串行，业务写极低频，足够；连接池只读，写走单连接 + WAL 模式。

```sql
PRAGMA journal_mode = WAL;
PRAGMA synchronous = NORMAL;
PRAGMA busy_timeout = 5000;
```

## 2. 完整 DDL

```sql
-- 0001_init.sql
CREATE TABLE prompts (
  id           TEXT PRIMARY KEY,          -- p_xxxxxxxx（服务端生成，8位 hex）
  content      TEXT NOT NULL,             -- 正文（UTF-8）
  tags         TEXT NOT NULL DEFAULT '',  -- 逗号分隔，避免子表
  version      INTEGER NOT NULL DEFAULT 1,-- 每次修改 +1，delta 依据
  content_hash TEXT NOT NULL,             -- sha256(空格归一化(content)) 的 hex
  status       TEXT NOT NULL,             -- pending/approved/rejected/featured
  deleted      INTEGER NOT NULL DEFAULT 0,-- 软删除标记（delta 用 deleted 列表）
  created_at   INTEGER NOT NULL,
  updated_at   INTEGER NOT NULL
);

CREATE TABLE scores (
  id         INTEGER PRIMARY KEY AUTOINCREMENT,
  prompt_id  TEXT NOT NULL REFERENCES prompts(id),
  value      INTEGER NOT NULL CHECK(value BETWEEN 1 AND 5),
  device_id  TEXT NOT NULL,
  created_at INTEGER NOT NULL,
  updated_at INTEGER NOT NULL,
  UNIQUE(prompt_id, device_id)            -- 幂等：重复评分覆盖
);

CREATE TABLE api_keys (
  key_hash    TEXT PRIMARY KEY,           -- sha256(key) 存储，不存明文（用于查找）
  secret_enc  TEXT NOT NULL,              -- HMAC secret：AES-GCM 加密存储（密钥来自环境变量），校验时解密做签名比对
                                          -- 自助注册的 Key 没有 secret，存空串（只有 Bearer 一条路）
  name        TEXT NOT NULL,              -- 身份名：运维签发用 label，自助签发用 self:<哈希前8>[:备注]
  enabled     INTEGER NOT NULL DEFAULT 1,
  created_at  INTEGER NOT NULL,
  scope       TEXT NOT NULL DEFAULT 'writer',  -- writer 可打分/上传；admin 额外可读 /-/metrics
  device_id   TEXT,                       -- 自助注册绑定的设备；运维签发为 NULL
  invite_id   INTEGER                     -- 消耗掉的邀请码 id，便于审计
);

-- 一设备一 Key（部分唯一索引：只约束自助注册的行，运维签发的 NULL 不参与）
CREATE UNIQUE INDEX ux_keys_device ON api_keys(device_id) WHERE device_id IS NOT NULL;

CREATE TABLE invite_codes (                -- 0002 迁移：自助注册的准入凭据
  id         INTEGER PRIMARY KEY AUTOINCREMENT,
  code_hash  TEXT NOT NULL UNIQUE,        -- 只存 sha256，与 api_keys.key_hash 同一策略
  label      TEXT NOT NULL,               -- 用途备注，如 "群发-2026-09"
  max_uses   INTEGER NOT NULL DEFAULT 1,  -- 一个码能换几把 Key
  used       INTEGER NOT NULL DEFAULT 0,
  expires_at INTEGER,                     -- NULL = 不过期（Unix 秒）
  enabled    INTEGER NOT NULL DEFAULT 1,
  created_at INTEGER NOT NULL
);

CREATE TABLE uploads (
  client_id   TEXT PRIMARY KEY,           -- 上传幂等键
  prompt_id   TEXT NOT NULL REFERENCES prompts(id),
  created_at  INTEGER NOT NULL
);

CREATE TABLE meta (
  key   TEXT PRIMARY KEY,
  value TEXT NOT NULL
);

CREATE TABLE catalog_snapshots (
  hash        TEXT PRIMARY KEY,           -- 每次目录变更后的新 catalog_hash
  computed_at INTEGER NOT NULL            -- 该 hash 生效时间，delta 用它定位 since
);

CREATE TABLE schema_migrations (
  version INTEGER PRIMARY KEY,
  applied_at INTEGER NOT NULL
);
```

**索引**：

```sql
CREATE INDEX idx_prompts_status ON prompts(status, id);
CREATE INDEX idx_prompts_updated ON prompts(updated_at);     -- delta 按时间扫描
CREATE INDEX idx_scores_prompt ON scores(prompt_id);          -- 聚合评分
```

## 3. 字段与校验规则

| 字段 | 规则 |
|------|------|
| `content` | 1–8192 字符；入库前做 Unicode 归一化（NFKC）+ 去首尾空白 |
| `content_hash` | `sha256(空格压缩(归一化正文))`，**去重与 ETag/`h` 字段依据** |
| `tags` | 单标签 `[a-z0-9_-]{1,32}`，最多 10 个，逗号分隔存 `coding,reasoning` |
| `status` 迁移 | `pending → approved/rejected`；`approved → featured`；任意 →（软删）`deleted=1` |
| `id` | 短随机 `p_` + 8 位 hex，冲突重试 |

## 4. 缓存设计

- **进程内 LRU**（`internal/cache`）：`hashicorp/golang-lru/v2`，容量 2048 条，以 `prompt:id` 为键缓存**已编译的响应字节**（已 gzip 则缓存 gzip 结果，避免重复压缩）。
- **缓存一致性**：写操作（评分/审核更新）使对应 `prompt:id` 与 `meta:catalog_hash` 失效。
- **目录 hash** 缓存于 `meta` 表 `catalog_hash` 键，任何 approved 集合变更时重算。

## 5. 目录 hash 计算（`internal/catalog`）

> 本包原定名 `internal/sync`，因与标准库 `sync` 包名冲突（import 后无法再用 `sync.Mutex`）而改名。

增量同步的根基，必须**确定性、可复现**：

```
catalog_hash = sha256(
  对全部 (status='approved' AND deleted=0) 的条目，
  按 id 排序，拼 <id>:<version>:<content_hash> 用 '\n' 连接
)
```

> 只用 `id+version+content_hash` 三要素即可：正文变化 → `content_hash` 变；元数据（tags）变化 → `version` 增。

## 6. Delta 变更集查询

客户端携带 `since`（上次的 `catalog_hash`）。服务端需把该 hash 解析为时间戳，再按时间取增量：

1. **hash → 时间戳**：查 `catalog_snapshots`（每次目录变更后写入 `hash→computed_at`）。命中则得到 `since_time`；**未命中（客户端快照过旧/未知）→ 回退全量同步**。
2. **取增量**：

```sql
-- 变更（upsert）
SELECT * FROM prompts WHERE updated_at > ? AND status IN ('approved','featured') AND deleted=0
  ORDER BY updated_at, id;
-- 删除/下架
SELECT id FROM prompts WHERE updated_at > ? AND (deleted=1 OR status='rejected');
```

3. 每次目录变更时：写入 `catalog_snapshots(new_hash, now)`，并按需裁剪（保留最近 30 天 / 最近 N 条），避免无限增长。

## 7. 去重与幂等

| 场景 | 键 | 行为 |
|------|----|------|
| 上传重复内容 | `content_hash` | 若已有 approved 相同正文，返回既有 `id`（不新建） |
| 上传重复请求 | `clientId` | 命中 `uploads` 表 → 幂等返回原 `id` |
| 评分重复 | `(prompt_id, device_id)` | `UNIQUE` 约束 + upsert，覆盖旧分值 |
| 目录 hash 碰撞 | 极低概率 | 以 `since` 找不到对应时间戳时兜底全量 |

## 8. 审核状态机（`internal/moderation`）

```
                upload(用户)
                     │
              ┌──────▼──────┐
              │   pending   │──(审核通过)──►  approved ──(精选)──► featured
              └──────┬──────┘
                     │(打回/违规)
                     ▼
                 rejected
              （approved/featured/rejected 任意──(下架)──► deleted=1）
```

- 只读端点**只暴露** `approved`/`featured`。
- 审核动作写审计日志（非本文档范围，可在 `prompts.updated_at` + `status` 体现）。

## 9. 备份

- 每日 `VACUUM INTO '/backup/bench-YYYYMMDD.db'`（WAL 下安全），轮转 7 份。
- 见 `deployment.md` §3。

## 10. 容量估算

| 项 | 数量级 |
|----|--------|
| 10 万条提示词 | ≈ 100–200 MB |
| 评分 100 万条 | ≈ 30–60 MB |
| 单文件峰值 | < 500 MB，SSD 无压力 |