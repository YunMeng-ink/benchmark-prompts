package client

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	_ "modernc.org/sqlite" // 与服务端同一纯 Go 驱动，无 CGO
)

// Cache 是本地缓存：提示词正文 + kv（catalog_hash / device_id / ETag）。
//
// 用 SQLite 而不是 JSON 文件的原因：delta 是 upsert + delete 的集合操作，
// 文件方案要么整体重写、要么自研增量合并；用现成引擎更短也更不容易错。
type Cache struct {
	db   *sql.DB
	path string
}

const cacheSchema = `
CREATE TABLE IF NOT EXISTS prompt_cache (
  id           TEXT PRIMARY KEY,
  content      TEXT NOT NULL,
  tags         TEXT NOT NULL DEFAULT '',
  version      INTEGER NOT NULL,
  content_hash TEXT NOT NULL DEFAULT '',
  updated_at   INTEGER NOT NULL
);
CREATE TABLE IF NOT EXISTS kv (
  k TEXT PRIMARY KEY,
  v TEXT NOT NULL
);`

// OpenCache 打开（必要时创建）缓存库。path 为空则用 DefaultCachePath()。
func OpenCache(path string) (*Cache, error) {
	if path == "" {
		p, err := DefaultCachePath()
		if err != nil {
			return nil, err
		}
		path = p
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, fmt.Errorf("创建缓存目录失败: %w", err)
	}

	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, fmt.Errorf("打开本地缓存失败: %w", err)
	}
	db.SetMaxOpenConns(1)

	c := &Cache{db: db, path: path}
	ctx := context.Background()
	// journal_mode 用 DELETE：本地缓存是单连接、短命、单进程的 SQLite 文件，
	// WAL 的并发收益在这里不存在，而 WAL 运行期会多出 .db-wal/.db-shm 两个侧文件。
	// 服务端仍用 WAL —— 那里确有“服务进程 + 运维子命令”并发读写的场景。
	//
	// 注意：这不是 Windows 上那个 t.TempDir 偶发清理失败的“修复”。那个现象至今
	// 未能稳定复现（改前改后合计 34 轮 -race 全绿），原因未定位，别把它当已解决。
	for _, p := range []string{"PRAGMA journal_mode=DELETE", "PRAGMA synchronous=NORMAL", "PRAGMA busy_timeout=5000"} {
		if _, err := db.ExecContext(ctx, p); err != nil {
			_ = db.Close()
			return nil, fmt.Errorf("缓存 PRAGMA 失败: %w", err)
		}
	}
	if _, err := db.ExecContext(ctx, cacheSchema); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("初始化缓存表失败: %w", err)
	}
	return c, nil
}

// Path 返回缓存文件路径。
func (c *Cache) Path() string { return c.path }

// Close 关闭连接。
func (c *Cache) Close() error { return c.db.Close() }

// UpsertPrompt 写入/覆盖一条提示词，返回是否发生了实际变化（用于统计）。
func (c *Cache) UpsertPrompt(ctx context.Context, p *Prompt) (bool, error) {
	if p == nil || p.ID == "" {
		return false, errors.New("client: 空提示词不能写入缓存")
	}
	var (
		oldVer int64
		oldCh  string
	)
	err := c.db.QueryRowContext(ctx,
		"SELECT version, content_hash FROM prompt_cache WHERE id=?", p.ID).Scan(&oldVer, &oldCh)
	changed := true
	if err == nil {
		changed = oldVer != p.Version || oldCh != p.Hash
	} else if !errors.Is(err, sql.ErrNoRows) {
		return false, fmt.Errorf("查询缓存条目失败: %w", err)
	}

	if _, err := c.db.ExecContext(ctx,
		`INSERT INTO prompt_cache(id, content, tags, version, content_hash, updated_at)
		 VALUES(?,?,?,?,?,?)
		 ON CONFLICT(id) DO UPDATE SET
		   content=excluded.content, tags=excluded.tags, version=excluded.version,
		   content_hash=excluded.content_hash, updated_at=excluded.updated_at`,
		p.ID, p.Content, strings.Join(p.Tags, ","), p.Version, p.Hash, time.Now().Unix()); err != nil {
		return false, fmt.Errorf("写入缓存失败: %w", err)
	}
	// 正文变了，缓存的 ETag 必须一起作废
	if changed {
		_ = c.SetKV(ctx, etagKey(p.ID), "")
	}
	return changed, nil
}

// DeletePrompts 删除若干条目，返回实际删除数。
func (c *Cache) DeletePrompts(ctx context.Context, ids []string) (int, error) {
	if len(ids) == 0 {
		return 0, nil
	}
	tx, err := c.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, fmt.Errorf("开启缓存事务失败: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	n := 0
	for _, id := range ids {
		res, err := tx.ExecContext(ctx, "DELETE FROM prompt_cache WHERE id=?", id)
		if err != nil {
			return 0, fmt.Errorf("删除缓存条目失败: %w", err)
		}
		if k, _ := res.RowsAffected(); k > 0 {
			n++
		}
		if _, err := tx.ExecContext(ctx, "DELETE FROM kv WHERE k=?", etagKey(id)); err != nil {
			return 0, fmt.Errorf("清理缓存 ETag 失败: %w", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("提交缓存删除失败: %w", err)
	}
	return n, nil
}

// GetPrompt 读一条本地缓存；没有则返回 ErrNotFound（client 包自己的哨兵）。
func (c *Cache) GetPrompt(ctx context.Context, id string) (*Prompt, error) {
	var (
		content, tags, ch string
		ver               int64
	)
	err := c.db.QueryRowContext(ctx,
		"SELECT content, tags, version, content_hash FROM prompt_cache WHERE id=?", id).
		Scan(&content, &tags, &ver, &ch)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("%w: %s", ErrCacheMiss, id)
	}
	if err != nil {
		return nil, fmt.Errorf("读取缓存失败: %w", err)
	}
	return &Prompt{
		ID:      id,
		Content: content,
		Tags:    splitTags(tags),
		Version: ver,
		Hash:    ch,
	}, nil
}

// AllIDs 返回本地已有条目 id（升序，用于 random 去重）。
func (c *Cache) AllIDs(ctx context.Context) ([]string, error) {
	rows, err := c.db.QueryContext(ctx, "SELECT id FROM prompt_cache ORDER BY id")
	if err != nil {
		return nil, fmt.Errorf("枚举缓存失败: %w", err)
	}
	defer rows.Close()

	var out []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("扫描缓存 id 失败: %w", err)
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

// Len 返回缓存条目数。
func (c *Cache) Len(ctx context.Context) (int, error) {
	var n int
	if err := c.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM prompt_cache").Scan(&n); err != nil {
		return 0, fmt.Errorf("统计缓存失败: %w", err)
	}
	return n, nil
}

// GetKV / SetKV 是通用元数据存储（catalog_hash、device_id、etag:*）。
func (c *Cache) GetKV(ctx context.Context, k string) (string, error) {
	var v string
	err := c.db.QueryRowContext(ctx, "SELECT v FROM kv WHERE k=?", k).Scan(&v)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("读取 kv 失败: %w", err)
	}
	return v, nil
}

// SetKV 写入元数据；v 为空表示删除该键。
func (c *Cache) SetKV(ctx context.Context, k, v string) error {
	if v == "" {
		if _, err := c.db.ExecContext(ctx, "DELETE FROM kv WHERE k=?", k); err != nil {
			return fmt.Errorf("删除 kv 失败: %w", err)
		}
		return nil
	}
	if _, err := c.db.ExecContext(ctx,
		`INSERT INTO kv(k, v) VALUES(?,?)
		 ON CONFLICT(k) DO UPDATE SET v=excluded.v`, k, v); err != nil {
		return fmt.Errorf("写入 kv 失败: %w", err)
	}
	return nil
}

// CatalogHash 读取上次同步到的目录 hash。
func (c *Cache) CatalogHash(ctx context.Context) (string, error) {
	return c.GetKV(ctx, kvCatalogHash)
}

// SetCatalogHash 记录本次同步到的目录 hash。
func (c *Cache) SetCatalogHash(ctx context.Context, h string) error {
	return c.SetKV(ctx, kvCatalogHash, h)
}

// DeviceID 取本地设备指纹；不存在时生成并持久化（评分去重用）。
func (c *Cache) DeviceID(ctx context.Context) (string, error) {
	v, err := c.GetKV(ctx, kvDeviceID)
	if err != nil {
		return "", err
	}
	if v != "" {
		return v, nil
	}
	v = "d_" + newID(8)
	if err := c.SetKV(ctx, kvDeviceID, v); err != nil {
		return "", err
	}
	return v, nil
}

// GetETag / SetETag 缓存 HTTP ETag，用于 If-None-Match。
func (c *Cache) GetETag(ctx context.Context, key string) (string, error) {
	return c.GetKV(ctx, etagKey(key))
}

// recentKeep 是“最近抽过”窗口的长度。
const recentKeep = 50

// PushRecent 把一个刚抽到的 id 记入滚动窗口（最新在前，超出部分丢弃）。
//
// 为什么不用“全部本地缓存 id”：`bench sync` 会把整个目录灌进缓存，
// 若 --fresh 排除全部缓存条目，它等价于“抽不到任何东西”，几乎必然 404。
// 用户真正想要的“别重复”是“别重复我**刚抽过**的”。
func (c *Cache) PushRecent(ctx context.Context, id string) error {
	if strings.TrimSpace(id) == "" {
		return nil
	}
	current, err := c.RecentIDs(ctx)
	if err != nil {
		return err
	}
	out := make([]string, 0, len(current)+1)
	out = append(out, id)
	for _, x := range current {
		if x != id {
			out = append(out, x)
		}
		if len(out) >= recentKeep {
			break
		}
	}
	return c.SetKV(ctx, kvRecent, strings.Join(out, ","))
}

// RecentIDs 返回最近抽过的 id（最新在前）。
func (c *Cache) RecentIDs(ctx context.Context) ([]string, error) {
	v, err := c.GetKV(ctx, kvRecent)
	if err != nil {
		return nil, err
	}
	return splitTags(v), nil
}

// ClearRecent 清空“最近抽过”窗口。
func (c *Cache) ClearRecent(ctx context.Context) error {
	return c.SetKV(ctx, kvRecent, "")
}

// SetETag 记录某个键的 ETag。
func (c *Cache) SetETag(ctx context.Context, key, etag string) error {
	return c.SetKV(ctx, etagKey(key), etag)
}

// Purge 清空全部缓存（保留 device_id，否则评分去重会失效）。
func (c *Cache) Purge(ctx context.Context) error {
	if _, err := c.db.ExecContext(ctx, `
		DELETE FROM prompt_cache;
		DELETE FROM kv WHERE k <> ?`, kvDeviceID); err != nil {
		return fmt.Errorf("清空缓存失败: %w", err)
	}
	return nil
}

// ErrCacheMiss 表示本地缓存没有该条目。
var ErrCacheMiss = errors.New("client: 本地缓存未命中")

const (
	kvCatalogHash = "catalog_hash"
	kvDeviceID    = "device_id"
	kvRecent      = "recent_random"
)

func etagKey(id string) string { return "etag:" + id }

func splitTags(s string) []string {
	if s == "" {
		return nil
	}
	parts := strings.Split(s, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}
