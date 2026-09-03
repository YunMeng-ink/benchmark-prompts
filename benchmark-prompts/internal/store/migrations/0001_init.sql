-- 0001_init.sql
-- 权威定义见 docs/storage.md

CREATE TABLE prompts (
  id           TEXT PRIMARY KEY,
  content      TEXT NOT NULL,
  tags         TEXT NOT NULL DEFAULT '',
  version      INTEGER NOT NULL DEFAULT 1,
  content_hash TEXT NOT NULL,
  status       TEXT NOT NULL,
  deleted      INTEGER NOT NULL DEFAULT 0,
  created_at   INTEGER NOT NULL,
  updated_at   INTEGER NOT NULL
);

CREATE INDEX idx_prompts_status  ON prompts(status, id);
CREATE INDEX idx_prompts_updated ON prompts(updated_at);
CREATE INDEX idx_prompts_chash   ON prompts(content_hash);

CREATE TABLE scores (
  id         INTEGER PRIMARY KEY AUTOINCREMENT,
  prompt_id  TEXT NOT NULL REFERENCES prompts(id),
  value      INTEGER NOT NULL CHECK (value BETWEEN 1 AND 5),
  device_id  TEXT NOT NULL,
  created_at INTEGER NOT NULL,
  updated_at INTEGER NOT NULL,
  UNIQUE (prompt_id, device_id)
);

CREATE INDEX idx_scores_prompt ON scores(prompt_id);

CREATE TABLE api_keys (
  key_hash   TEXT PRIMARY KEY,
  secret_enc TEXT NOT NULL,
  name       TEXT NOT NULL,
  enabled    INTEGER NOT NULL DEFAULT 1,
  created_at INTEGER NOT NULL
);

CREATE TABLE uploads (
  client_id  TEXT PRIMARY KEY,
  prompt_id  TEXT NOT NULL REFERENCES prompts(id),
  created_at INTEGER NOT NULL
);

CREATE TABLE meta (
  key   TEXT PRIMARY KEY,
  value TEXT NOT NULL
);

-- delta 增量同步的 hash → 时间戳 解析表（见 docs/storage.md §6）
CREATE TABLE catalog_snapshots (
  hash        TEXT PRIMARY KEY,
  computed_at INTEGER NOT NULL
);