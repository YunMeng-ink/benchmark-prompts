-- 0002_selfservice.sql
-- 自助注册：邀请码 + 设备绑定 + Key 作用域。权威说明见 docs/storage.md。

-- Key 作用域：writer 可打分/上传；admin 额外可访问运维端点（/-/metrics）。
ALTER TABLE api_keys ADD COLUMN scope TEXT NOT NULL DEFAULT 'writer';

-- 自助注册的 Key 绑定到申请时提交的 deviceId；运维 -put-key 签发的为 NULL。
ALTER TABLE api_keys ADD COLUMN device_id TEXT;

-- 记录消耗掉的邀请码，便于事后审计（谁凭哪个码拿到写权限）。
ALTER TABLE api_keys ADD COLUMN invite_id INTEGER;

-- 关键：既有运维 Key 必须升为 admin。上一行的 DEFAULT 'writer' 会把它们静默降级，
-- 导致升级后 /-/metrics 突然 401。新列刚加，此刻所有既有行的 device_id 都是 NULL。
UPDATE api_keys SET scope = 'admin' WHERE device_id IS NULL;

-- 一台设备只能有一把自助 Key（重复申请返回 conflict，而不是悄悄发第二把）。
CREATE UNIQUE INDEX ux_keys_device ON api_keys(device_id) WHERE device_id IS NOT NULL;

CREATE TABLE invite_codes (
  id         INTEGER PRIMARY KEY AUTOINCREMENT,
  code_hash  TEXT NOT NULL UNIQUE,       -- 只存 sha256，与 api_keys.key_hash 同一策略
  label      TEXT NOT NULL,              -- 用途备注，如 "群发-2026-09"
  max_uses   INTEGER NOT NULL DEFAULT 1, -- 一个码能换几把 Key
  used       INTEGER NOT NULL DEFAULT 0,
  expires_at INTEGER,                    -- NULL = 不过期（Unix 秒）
  enabled    INTEGER NOT NULL DEFAULT 1,
  created_at INTEGER NOT NULL
);

CREATE INDEX idx_invite_enabled ON invite_codes(enabled, expires_at);
