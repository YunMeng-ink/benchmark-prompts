// Package store 是唯一数据库出入口，封装 SQLite。上层只通过这里取数。
//
// 表结构见 internal/store/migrations/*.sql，权威说明见 docs/storage.md。
package store

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"embed"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"sort"
	"strconv"
	"strings"
	"time"

	_ "modernc.org/sqlite" // 纯 Go 驱动，无 CGO

	"github.com/example/benchmark-prompts/internal/model"
	"github.com/example/benchmark-prompts/internal/secretbox"
)

// ErrNotFound 表示行不存在，上层据此映射 HTTP 404。
var ErrNotFound = errors.New("store: not found")

// SchemaVersion 是当前 schema 版本，用于 /v1/meta.schema_version。
const SchemaVersion = 1

//go:embed migrations/*.sql
var migrationsFS embed.FS

const (
	publicStatuses = "deleted=0 AND status IN (?,?)"
	colsPrompt     = "id, content, tags, version, status, content_hash"
	colsSummary    = "id, tags, version, content_hash"
)

func publicArgs() []any {
	return []any{model.StatusApproved, model.StatusFeatured}
}

// Store 包装 *sql.DB。
type Store struct {
	db *sql.DB
}

// Open 打开（必要时创建）数据库并设置 PRAGMA。
func Open(path string) (*Store, error) {
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, fmt.Errorf("打开 SQLite 失败: %w", err)
	}
	// SQLite 写是串行的；限制为单连接可让 PRAGMA 生效范围确定、写不冲突。
	db.SetMaxOpenConns(1)

	s := &Store{db: db}
	ctx := context.Background()
	for _, p := range []string{
		"PRAGMA journal_mode=WAL",
		"PRAGMA synchronous=NORMAL",
		"PRAGMA busy_timeout=5000",
		"PRAGMA foreign_keys=ON",
	} {
		if _, err := db.ExecContext(ctx, p); err != nil {
			_ = db.Close()
			return nil, fmt.Errorf("执行 %q 失败: %w", p, err)
		}
	}
	return s, nil
}

// Close 关闭底层连接池。
func (s *Store) Close() error { return s.db.Close() }

// Backup 用 VACUUM INTO 生成一致性快照（供 -backup 子命令使用，
// 避免依赖外部 sqlite3 CLI，见 docs/deployment.md §3）。
func (s *Store) Backup(ctx context.Context, dest string) error {
	if _, err := s.db.ExecContext(ctx, "VACUUM INTO ?", dest); err != nil {
		return fmt.Errorf("备份到 %s 失败: %w", dest, err)
	}
	return nil
}

// Migrate 按文件名顺序应用 internal/store/migrations/*.sql，幂等。
func (s *Store) Migrate(ctx context.Context) error {
	if _, err := s.db.ExecContext(ctx,
		`CREATE TABLE IF NOT EXISTS schema_migrations (
		   version INTEGER PRIMARY KEY,
		   applied_at INTEGER NOT NULL
		 )`); err != nil {
		return fmt.Errorf("建 schema_migrations 失败: %w", err)
	}

	entries, err := fs.ReadDir(migrationsFS, "migrations")
	if err != nil {
		return fmt.Errorf("读取迁移目录失败: %w", err)
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".sql") {
			names = append(names, e.Name())
		}
	}
	sort.Strings(names)

	for _, name := range names {
		ver, err := leadingVersion(name)
		if err != nil {
			return err
		}
		var n int
		if err := s.db.QueryRowContext(ctx,
			"SELECT COUNT(*) FROM schema_migrations WHERE version=?", ver).Scan(&n); err != nil {
			return fmt.Errorf("查询迁移状态失败: %w", err)
		}
		if n > 0 {
			continue
		}
		body, err := migrationsFS.ReadFile("migrations/" + name)
		if err != nil {
			return fmt.Errorf("读取迁移 %s 失败: %w", name, err)
		}
		if err := s.applyMigration(ctx, ver, name, string(body)); err != nil {
			return err
		}
	}
	return nil
}

func (s *Store) applyMigration(ctx context.Context, ver int, name, body string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("开启迁移事务失败: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	if _, err := tx.ExecContext(ctx, body); err != nil {
		return fmt.Errorf("应用迁移 %s 失败: %w", name, err)
	}
	if _, err := tx.ExecContext(ctx,
		"INSERT INTO schema_migrations(version, applied_at) VALUES(?,?)",
		ver, time.Now().Unix()); err != nil {
		return fmt.Errorf("记录迁移 %s 失败: %w", name, err)
	}
	return tx.Commit()
}

func leadingVersion(name string) (int, error) {
	i := 0
	for i < len(name) && name[i] >= '0' && name[i] <= '9' {
		i++
	}
	if i == 0 {
		return 0, fmt.Errorf("迁移文件名缺少版本号前缀: %s", name)
	}
	v, err := strconv.Atoi(name[:i])
	if err != nil {
		return 0, fmt.Errorf("迁移版本号非法 %s: %w", name, err)
	}
	return v, nil
}

// ---- 读路径 ----

type scanner interface{ Scan(dest ...any) error }

func scanPrompt(sc scanner) (*model.Prompt, error) {
	var (
		id, content, tags, status, chash string
		ver                              int64
	)
	if err := sc.Scan(&id, &content, &tags, &ver, &status, &chash); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("扫描 prompt 失败: %w", err)
	}
	return &model.Prompt{
		ID:      id,
		Content: content,
		Tags:    model.ParseTags(tags),
		Version: ver,
		Status:  status,
		Hash:    model.ShortHash(chash),
	}, nil
}

func scanSummary(sc scanner) (*model.PromptSummary, error) {
	var (
		id, tags, chash string
		ver             int64
	)
	if err := sc.Scan(&id, &tags, &ver, &chash); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("扫描 summary 失败: %w", err)
	}
	return &model.PromptSummary{
		ID:      id,
		Tags:    model.ParseTags(tags),
		Version: ver,
		Hash:    model.ShortHash(chash),
	}, nil
}

// CountApproved 返回公开提示词总数。
func (s *Store) CountApproved(ctx context.Context) (int64, error) {
	var n int64
	q := "SELECT COUNT(*) FROM prompts WHERE " + publicStatuses
	if err := s.db.QueryRowContext(ctx, q, publicArgs()...).Scan(&n); err != nil {
		return 0, fmt.Errorf("统计公开提示词失败: %w", err)
	}
	return n, nil
}

// ListApproved 分页返回摘要（不含正文）。调用方传 limit+1 以便判断 has_more。
func (s *Store) ListApproved(ctx context.Context, tag string, limit, offset int) ([]*model.PromptSummary, error) {
	q := "SELECT " + colsSummary + " FROM prompts WHERE " + publicStatuses
	args := publicArgs()
	if tag != "" {
		q += ` AND (',' || tags || ',') LIKE ?`
		args = append(args, "%,"+tag+",%")
	}
	q += " ORDER BY id LIMIT ? OFFSET ?"
	args = append(args, limit, offset)

	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("列表查询失败: %w", err)
	}
	defer rows.Close()

	out := make([]*model.PromptSummary, 0, limit)
	for rows.Next() {
		p, err := scanSummary(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// GetByID 按 id 取任意状态的提示词（审核/维护与测试场景使用）。
func (s *Store) GetByID(ctx context.Context, id string) (*model.Prompt, error) {
	q := "SELECT " + colsPrompt + " FROM prompts WHERE id=?"
	return scanPrompt(s.db.QueryRowContext(ctx, q, id))
}

// GetApproved 按 id 取一条公开提示词；不存在返回 ErrNotFound。
func (s *Store) GetApproved(ctx context.Context, id string) (*model.Prompt, error) {
	q := "SELECT " + colsPrompt + " FROM prompts WHERE id=? AND " + publicStatuses
	args := append([]any{id}, publicArgs()...)
	return scanPrompt(s.db.QueryRowContext(ctx, q, args...))
}

// RandomApproved 随机取一条公开提示词，可排除指定 id。
func (s *Store) RandomApproved(ctx context.Context, tag string, exclude []string) (*model.Prompt, error) {
	q := "SELECT " + colsPrompt + " FROM prompts WHERE " + publicStatuses
	args := publicArgs()
	if tag != "" {
		q += ` AND (',' || tags || ',') LIKE ?`
		args = append(args, "%,"+tag+",%")
	}
	if len(exclude) > 0 {
		q += " AND id NOT IN (" + placeholders(len(exclude)) + ")"
		for _, e := range exclude {
			args = append(args, e)
		}
	}
	q += " ORDER BY RANDOM() LIMIT 1"
	return scanPrompt(s.db.QueryRowContext(ctx, q, args...))
}

// HashedRow 目录 hash 的最小输入三元组。
type HashedRow struct {
	ID          string
	Version     int64
	ContentHash string
}

// RowsForHash 返回全部公开条目的 (id, version, content_hash)，按 id 升序。
// 顺序确定 + 字段确定 ⇒ hash 可复现（docs/storage.md §5）。
func (s *Store) RowsForHash(ctx context.Context) ([]HashedRow, error) {
	q := "SELECT id, version, content_hash FROM prompts WHERE " + publicStatuses + " ORDER BY id"
	rows, err := s.db.QueryContext(ctx, q, publicArgs()...)
	if err != nil {
		return nil, fmt.Errorf("读取目录失败: %w", err)
	}
	defer rows.Close()

	var out []HashedRow
	for rows.Next() {
		var r HashedRow
		if err := rows.Scan(&r.ID, &r.Version, &r.ContentHash); err != nil {
			return nil, fmt.Errorf("扫描目录行失败: %w", err)
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// PromptsUpdatedSince 返回 since 之后变更过的公开提示词。
func (s *Store) PromptsUpdatedSince(ctx context.Context, since int64, limit, offset int) ([]*model.Prompt, error) {
	q := "SELECT " + colsPrompt + " FROM prompts WHERE updated_at >= ? AND " + publicStatuses +
		" ORDER BY updated_at, id LIMIT ? OFFSET ?"
	args := append([]any{since}, publicArgs()...)
	args = append(args, limit, offset)

	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("增量查询失败: %w", err)
	}
	defer rows.Close()

	out := make([]*model.Prompt, 0, limit)
	for rows.Next() {
		p, err := scanPrompt(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// PublicAll 全量分页（首次同步用）。
func (s *Store) PublicAll(ctx context.Context, limit, offset int) ([]*model.Prompt, error) {
	q := "SELECT " + colsPrompt + " FROM prompts WHERE " + publicStatuses + " ORDER BY id LIMIT ? OFFSET ?"
	args := append(publicArgs(), limit, offset)

	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("全量查询失败: %w", err)
	}
	defer rows.Close()

	out := make([]*model.Prompt, 0, limit)
	for rows.Next() {
		p, err := scanPrompt(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// ListByStatus 按状态列出（审核队列与运维子命令使用）。
func (s *Store) ListByStatus(ctx context.Context, status string, limit int) ([]*model.Prompt, error) {
	q := "SELECT " + colsPrompt + " FROM prompts WHERE status=? AND deleted=0 ORDER BY updated_at, id LIMIT ?"
	rows, err := s.db.QueryContext(ctx, q, status, limit)
	if err != nil {
		return nil, fmt.Errorf("按状态查询失败: %w", err)
	}
	defer rows.Close()

	out := make([]*model.Prompt, 0, 16)
	for rows.Next() {
		p, serr := scanPrompt(rows)
		if serr != nil {
			return nil, serr
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// DeletedSince 返回 since 之后“客户端应当删除”的 id。
//
// 判定为 NOT(approved|featured)，因此 pending 也会被列出：客户端本来
// 就不可能持有 pending 条目，多列几行是无害且幂等的；反之若只列 rejected，
// 则“已公开条目被回退为 pending”这种少但严重的场景会漏删，导致脏数据残留。
func (s *Store) DeletedSince(ctx context.Context, since int64) ([]string, error) {
	q := `SELECT id FROM prompts
	      WHERE updated_at >= ? AND (deleted=1 OR status NOT IN (?,?))`
	rows, err := s.db.QueryContext(ctx, q, since, model.StatusApproved, model.StatusFeatured)
	if err != nil {
		return nil, fmt.Errorf("删除增量查询失败: %w", err)
	}
	defer rows.Close()

	out := make([]string, 0, 8)
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("扫描删除 id 失败: %w", err)
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

// ---- catalog_snapshots：delta 的 hash → 时间戳 解析（docs/storage.md §6）----

// RecordSnapshot 登记一个新目录 hash；已存在则忽略。
func (s *Store) RecordSnapshot(ctx context.Context, hash string, at int64) error {
	_, err := s.db.ExecContext(ctx,
		`INSERT OR IGNORE INTO catalog_snapshots(hash, computed_at) VALUES(?,?)`, hash, at)
	if err != nil {
		return fmt.Errorf("登记目录快照失败: %w", err)
	}
	return nil
}

// LookupSnapshot 把客户端 since 解析为时间戳；第二个返回值是是否命中。
func (s *Store) LookupSnapshot(ctx context.Context, hash string) (int64, bool, error) {
	var at int64
	err := s.db.QueryRowContext(ctx,
		"SELECT computed_at FROM catalog_snapshots WHERE hash=?", hash).Scan(&at)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, false, nil
	}
	if err != nil {
		return 0, false, fmt.Errorf("查询目录快照失败: %w", err)
	}
	return at, true, nil
}

// PruneSnapshots 删除 keepSeconds 之前的快照（保留最新一条以防 since 悬空）。
func (s *Store) PruneSnapshots(ctx context.Context, keepSeconds int64) error {
	cutoff := time.Now().Unix() - keepSeconds
	_, err := s.db.ExecContext(ctx,
		`DELETE FROM catalog_snapshots
		 WHERE computed_at < ?
		   AND hash <> (SELECT hash FROM catalog_snapshots ORDER BY computed_at DESC LIMIT 1)`,
		cutoff)
	if err != nil {
		return fmt.Errorf("裁剪目录快照失败: %w", err)
	}
	return nil
}

// ---- 写路径 ----

// UpsertScore 写入评分；同一 (prompt, device) 幂等覆盖，返回聚合结果。
func (s *Store) UpsertScore(ctx context.Context, promptID string, value int, deviceID string) (avg float64, count int64, err error) {
	if err := s.AssertPublic(ctx, promptID); err != nil {
		return 0, 0, err
	}
	now := time.Now().Unix()
	if _, err := s.db.ExecContext(ctx,
		`INSERT INTO scores(prompt_id, value, device_id, created_at, updated_at)
		 VALUES(?,?,?,?,?)
		 ON CONFLICT(prompt_id, device_id)
		   DO UPDATE SET value=excluded.value, updated_at=excluded.updated_at`,
		promptID, value, deviceID, now, now); err != nil {
		return 0, 0, fmt.Errorf("写入评分失败: %w", err)
	}
	return s.ScoreStats(ctx, promptID)
}

// ScoreStats 返回平均分与评分人数。
func (s *Store) ScoreStats(ctx context.Context, promptID string) (float64, int64, error) {
	var (
		avg sql.NullFloat64
		n   int64
	)
	if err := s.db.QueryRowContext(ctx,
		"SELECT AVG(value), COUNT(*) FROM scores WHERE prompt_id=?", promptID).
		Scan(&avg, &n); err != nil {
		return 0, 0, fmt.Errorf("统计评分失败: %w", err)
	}
	return avg.Float64, n, nil
}

// AssertPublic 校验 id 是否存在且处于公开状态；不存在或未公开一律返回 ErrNotFound。
// （导出给 api 层做只读统计端点的廉价守卫，避免为查存在性而读整个正文。）
func (s *Store) AssertPublic(ctx context.Context, id string) error {
	var one int
	err := s.db.QueryRowContext(ctx,
		"SELECT 1 FROM prompts WHERE id=? AND "+publicStatuses,
		append([]any{id}, publicArgs()...)...).Scan(&one)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrNotFound
	}
	if err != nil {
		return fmt.Errorf("校验提示词可见性失败: %w", err)
	}
	return nil
}

// UploadResult 是上传/去重后的结果。
type UploadResult struct {
	PromptID string
	Status   string
	Reused   bool // true 表示命中幂等或内容去重，未新建
}

// CreatePendingPrompt 落一条 pending 提示词，串起两层幂等：
//  1. clientId 命中 uploads  → 直接返回既有 id
//  2. content_hash 命中已有正文 → 复用既有 id（内容去重）
func (s *Store) CreatePendingPrompt(ctx context.Context, content string, tags []string, clientID string) (*UploadResult, error) {
	// 在入库边界统一裁剪，而不只依赖 handler：任何新写入方（审核脚本、
	// 批量导入、未来 CLI）都不会绕过这条一致性保证。内部空白仍然保留。
	content = model.TrimContent(content)
	chash := model.ContentHash(content)

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("开启上传事务失败: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	if clientID != "" {
		var existing string
		err := tx.QueryRowContext(ctx,
			"SELECT prompt_id FROM uploads WHERE client_id=?", clientID).Scan(&existing)
		if err == nil {
			status, serr := statusOf(ctx, tx, existing)
			if serr != nil {
				return nil, serr
			}
			if err := tx.Commit(); err != nil {
				return nil, fmt.Errorf("提交幂等返回失败: %w", err)
			}
			return &UploadResult{PromptID: existing, Status: status, Reused: true}, nil
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return nil, fmt.Errorf("查询幂等键失败: %w", err)
		}
	}

	var (
		pid    string
		status string
	)
	err = tx.QueryRowContext(ctx,
		`SELECT id, status FROM prompts WHERE content_hash=? AND deleted=0
		 ORDER BY (status IN (?,?)) DESC, id LIMIT 1`,
		append([]any{chash}, publicArgs()...)...).Scan(&pid, &status)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("内容去重查询失败: %w", err)
	}
	if err == nil {
		if clientID != "" {
			if _, err := tx.ExecContext(ctx,
				"INSERT OR IGNORE INTO uploads(client_id, prompt_id, created_at) VALUES(?,?,?)",
				clientID, pid, time.Now().Unix()); err != nil {
				return nil, fmt.Errorf("记录幂等键失败: %w", err)
			}
		}
		if err := tx.Commit(); err != nil {
			return nil, fmt.Errorf("提交去重返回失败: %w", err)
		}
		return &UploadResult{PromptID: pid, Status: status, Reused: true}, nil
	}

	// 新建
	newID, err := NewID()
	if err != nil {
		return nil, err
	}
	now := time.Now().Unix()
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO prompts(id, content, tags, version, content_hash, status, deleted, created_at, updated_at)
		 VALUES(?,?,?,?,?,?,0,?,?)`,
		newID, content, model.FormatTags(tags), 1, chash, model.StatusPending, now, now); err != nil {
		return nil, fmt.Errorf("插入提示词失败: %w", err)
	}
	if clientID != "" {
		if _, err := tx.ExecContext(ctx,
			"INSERT INTO uploads(client_id, prompt_id, created_at) VALUES(?,?,?)",
			clientID, newID, now); err != nil {
			return nil, fmt.Errorf("记录幂等键失败: %w", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("提交上传失败: %w", err)
	}
	return &UploadResult{PromptID: newID, Status: model.StatusPending, Reused: false}, nil
}

func statusOf(ctx context.Context, tx *sql.Tx, id string) (string, error) {
	var st string
	if err := tx.QueryRowContext(ctx, "SELECT status FROM prompts WHERE id=?", id).Scan(&st); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return "", ErrNotFound
		}
		return "", fmt.Errorf("查询状态失败: %w", err)
	}
	return st, nil
}

// SetStatus 变更提示词状态（审核动作），同时递增 version 以驱动 delta 与 ETag 失效。
func (s *Store) SetStatus(ctx context.Context, id, status string) error {
	switch status {
	case model.StatusPending, model.StatusApproved, model.StatusRejected, model.StatusFeatured:
	default:
		return fmt.Errorf("非法状态 %q", status)
	}
	res, err := s.db.ExecContext(ctx,
		`UPDATE prompts SET status=?, version=version+1, updated_at=? WHERE id=?`,
		status, time.Now().Unix(), id)
	if err != nil {
		return fmt.Errorf("更新状态失败: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// NewID 生成 p_ + 8 位 hex 的短 ID。
func NewID() (string, error) {
	b := make([]byte, 4)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("生成 ID 失败: %w", err)
	}
	return "p_" + hex.EncodeToString(b), nil
}

// ---- API Key（供 auth 使用）----

// APIKeyRecord 是一行 api_keys。
type APIKeyRecord struct {
	Name      string
	SecretEnc string
	Enabled   bool
	Scope     string // ScopeWriter / ScopeAdmin
	DeviceID  string // 自助注册的 Key 绑定设备；运维签发为空
}

// Key 作用域。writer 可打分与上传；admin 额外可访问运维端点（/-/metrics）。
const (
	ScopeWriter = "writer"
	ScopeAdmin  = "admin"
)

// LookupAPIKey 按 key 的 sha256 查找。
func (s *Store) LookupAPIKey(ctx context.Context, keyHash string) (*APIKeyRecord, error) {
	var (
		name, secretEnc string
		enabled         int
	)
	var scope, deviceID sql.NullString
	err := s.db.QueryRowContext(ctx,
		"SELECT name, secret_enc, enabled, scope, device_id FROM api_keys WHERE key_hash=?", keyHash).
		Scan(&name, &secretEnc, &enabled, &scope, &deviceID)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("查询 API Key 失败: %w", err)
	}
	return &APIKeyRecord{
		Name:      name,
		SecretEnc: secretEnc,
		Enabled:   enabled != 0,
		Scope:     scope.String,
		DeviceID:  deviceID.String,
	}, nil
}

// KeyHash 计算 api key 的查找哈希。
func KeyHash(plain string) string {
	sum := sha256.Sum256([]byte(plain))
	return hex.EncodeToString(sum[:])
}

// PutAPIKey 落一条 Key：明文 key 只存哈希，HMAC secret 存 AES-GCM 密文。
func (s *Store) PutAPIKey(ctx context.Context, plainKey, name, plainSecret string, masterKey []byte) error {
	enc, err := secretbox.Seal(masterKey, plainSecret)
	if err != nil {
		return fmt.Errorf("加密 secret 失败: %w", err)
	}
	// -put-key 是运维动作，签出的 Key 带管理能力；自助注册的走 RegisterSelfKey（writer）。
	_, err = s.db.ExecContext(ctx,
		`INSERT INTO api_keys(key_hash, secret_enc, name, enabled, created_at, scope)
		 VALUES(?,?,?,?,1,?)
		 ON CONFLICT(key_hash) DO UPDATE SET secret_enc=excluded.secret_enc,
		   name=excluded.name, enabled=1, scope=excluded.scope`,
		KeyHash(plainKey), enc, name, time.Now().Unix(), ScopeAdmin)
	if err != nil {
		return fmt.Errorf("写入 API Key 失败: %w", err)
	}
	return nil
}

func placeholders(n int) string {
	if n <= 0 {
		return ""
	}
	return strings.TrimSuffix(strings.Repeat("?,", n), ",")
}

// ---- 自助注册：邀请码与 Key 签发（0002 迁移）----

// ErrInviteInvalid 表示邀请码不存在、已停用、已过期或用尽。
// 一律返回同一个错误：不区分原因，探测者就无法判断某个码是否存在。
var ErrInviteInvalid = errors.New("store: invite invalid")

// ErrDeviceTaken 表示该设备已领过自助 Key（一设备一 Key）。
var ErrDeviceTaken = errors.New("store: device already registered")

// NewAPIKey 生成一把高熵 Bearer Key。自助签发不带 HMAC secret，
// 因为 secret 一旦进浏览器/命令行历史就等于公开；Bearer 已足够写权限。
func NewAPIKey() (string, error) {
	b := make([]byte, 20)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("生成 API Key 失败: %w", err)
	}
	return "bk_" + hex.EncodeToString(b), nil
}

// NewInviteCode 生成易读邀请码：10 位有效字符，去掉 I/O/0/1 等易混字形，中间加分隔符。
func NewInviteCode() (string, error) {
	const alphabet = "23456789ABCDEFGHJKLMNPQRSTUVWXYZ"
	b := make([]byte, 10)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("生成邀请码失败: %w", err)
	}
	out := make([]byte, 0, 11)
	for i, v := range b {
		if i == 5 {
			out = append(out, '-')
		}
		out = append(out, alphabet[int(v)%len(alphabet)])
	}
	return string(out), nil
}

// InviteRef 是邀请码的可展示状态（不含明文码，也不含完整哈希）。
type InviteRef struct {
	ID        int64
	Ref       string
	Label     string
	MaxUses   int
	Used      int
	ExpiresAt int64 // 0 = 不过期
	Enabled   bool
}

// APIKeyInfo 是一把 Key 的可展示信息。明文 Key 不可恢复（只存哈希）。
type APIKeyInfo struct {
	Ref       string // key_hash 前 12 位，作为吊销用的句柄
	Name      string
	Scope     string
	DeviceID  string
	Enabled   bool
	CreatedAt int64
}

// CreateInvite 登记一个邀请码，返回明文码。ttl<=0 表示不过期。
func (s *Store) CreateInvite(ctx context.Context, label string, maxUses int, ttl time.Duration) (string, error) {
	label = strings.TrimSpace(label)
	if label == "" {
		label = "invite"
	}
	if maxUses < 1 {
		maxUses = 1
	}
	code, err := NewInviteCode()
	if err != nil {
		return "", err
	}
	var expires sql.NullInt64
	if ttl > 0 {
		expires = sql.NullInt64{Int64: time.Now().Add(ttl).Unix(), Valid: true}
	}
	if _, err := s.db.ExecContext(ctx,
		`INSERT INTO invite_codes(code_hash, label, max_uses, used, expires_at, enabled, created_at)
		 VALUES(?,?,?,0,?,1,?)`,
		KeyHash(code), label, maxUses, expires, time.Now().Unix()); err != nil {
		return "", fmt.Errorf("登记邀请码失败: %w", err)
	}
	return code, nil
}

// ListInvites 列出邀请码状态（按登记顺序）。
func (s *Store) ListInvites(ctx context.Context) ([]InviteRef, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, substr(code_hash,1,12), label, max_uses, used, IFNULL(expires_at,0), enabled
		 FROM invite_codes ORDER BY id DESC LIMIT 200`)
	if err != nil {
		return nil, fmt.Errorf("查询邀请码失败: %w", err)
	}
	defer rows.Close()

	var out []InviteRef
	for rows.Next() {
		var r InviteRef
		var enabled int
		if err := rows.Scan(&r.ID, &r.Ref, &r.Label, &r.MaxUses, &r.Used, &r.ExpiresAt, &enabled); err != nil {
			return nil, fmt.Errorf("读取邀请码失败: %w", err)
		}
		r.Enabled = enabled != 0
		out = append(out, r)
	}
	return out, rows.Err()
}

// RegisterSelfKey 用邀请码换一把绑定设备的 writer Key。
//
// 全程单事务：查设备唯一 → 校验码 → 消费一次 → 落 Key。任一步失败都回滚，
// 避免出现「码被吃掉却没发出 Key」这种无法补救的状态。消费用带条件的 UPDATE
// 并以 RowsAffected 判定，并发下不会超发。
func (s *Store) RegisterSelfKey(ctx context.Context, code, deviceID, label string) (*APIKeyInfo, string, error) {
	code = strings.TrimSpace(code)
	deviceID = strings.TrimSpace(deviceID)

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, "", fmt.Errorf("开启注册事务失败: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	var (
		id        int64
		maxUses   int
		used      int
		enabled   int
		expiresAt sql.NullInt64
	)
	err = tx.QueryRowContext(ctx,
		`SELECT id, max_uses, used, enabled, expires_at FROM invite_codes WHERE code_hash=?`,
		KeyHash(code)).Scan(&id, &maxUses, &used, &enabled, &expiresAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, "", ErrInviteInvalid
	}
	if err != nil {
		return nil, "", fmt.Errorf("查询邀请码失败: %w", err)
	}

	now := time.Now().Unix()
	if enabled == 0 || used >= maxUses || (expiresAt.Valid && expiresAt.Int64 > 0 && expiresAt.Int64 <= now) {
		return nil, "", ErrInviteInvalid
	}

	// 先确认设备没领过，再消费名额：顺序反了会让“码打错了”的设备
	// 收到“该设备已领过 Key”这种误导性的回答。
	var dup int
	err = tx.QueryRowContext(ctx, "SELECT 1 FROM api_keys WHERE device_id=? LIMIT 1", deviceID).Scan(&dup)
	switch {
	case err == nil:
		return nil, "", ErrDeviceTaken
	case !errors.Is(err, sql.ErrNoRows):
		return nil, "", fmt.Errorf("检查设备绑定失败: %w", err)
	}

	res, err := tx.ExecContext(ctx,
		`UPDATE invite_codes SET used = used + 1
		 WHERE id=? AND used < max_uses AND enabled = 1
		   AND (expires_at IS NULL OR expires_at > ?)`, id, now)
	if err != nil {
		return nil, "", fmt.Errorf("消费邀请码失败: %w", err)
	}
	if n, _ := res.RowsAffected(); n != 1 {
		return nil, "", ErrInviteInvalid // 并发下被抢完
	}

	plain, err := NewAPIKey()
	if err != nil {
		return nil, "", err
	}
	hash := KeyHash(plain)
	name := "self:" + hash[:8]
	if l := strings.TrimSpace(label); l != "" {
		name += ":" + l
	}
	if len(name) > 64 {
		name = name[:64]
	}

	info := &APIKeyInfo{Ref: hash[:12], Name: name, Scope: ScopeWriter, DeviceID: deviceID, Enabled: true, CreatedAt: now}
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO api_keys(key_hash, secret_enc, name, enabled, created_at, scope, device_id, invite_id)
		 VALUES(?,?,?,1,?,?,?,?)`,
		hash, "", name, now, ScopeWriter, deviceID, id); err != nil {
		return nil, "", fmt.Errorf("写入 API Key 失败: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return nil, "", fmt.Errorf("提交注册失败: %w", err)
	}
	return info, plain, nil
}

// SelfKey 返回某把 Key 的自视信息（供 GET /v1/keys/self）。一次查完，不借道 LookupAPIKey。
func (s *Store) SelfKey(ctx context.Context, keyHash string) (*APIKeyInfo, error) {
	var (
		name, scope string
		deviceID    sql.NullString
		enabled     int
		created     int64
	)
	err := s.db.QueryRowContext(ctx,
		`SELECT name, scope, device_id, enabled, created_at FROM api_keys WHERE key_hash=?`, keyHash).
		Scan(&name, &scope, &deviceID, &enabled, &created)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("读取 Key 自视信息失败: %w", err)
	}
	return &APIKeyInfo{
		Ref:       keyHash[:12],
		Name:      name,
		Scope:     scope,
		DeviceID:  deviceID.String,
		Enabled:   enabled != 0,
		CreatedAt: created,
	}, nil
}

// DisableAPIKey 停用一把 Key（幂等：已停用也返回成功）。
func (s *Store) DisableAPIKey(ctx context.Context, keyHash string) error {
	res, err := s.db.ExecContext(ctx, "UPDATE api_keys SET enabled=0 WHERE key_hash=?", keyHash)
	if err != nil {
		return fmt.Errorf("停用 API Key 失败: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// ListAPIKeys 列出全部 Key（只给哈希前缀，明文不可恢复）。
func (s *Store) ListAPIKeys(ctx context.Context) ([]APIKeyInfo, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT substr(key_hash,1,12), name, scope, IFNULL(device_id,''), enabled, created_at
		 FROM api_keys ORDER BY created_at DESC LIMIT 500`)
	if err != nil {
		return nil, fmt.Errorf("查询 API Key 列表失败: %w", err)
	}
	defer rows.Close()

	var out []APIKeyInfo
	for rows.Next() {
		var k APIKeyInfo
		var enabled int
		if err := rows.Scan(&k.Ref, &k.Name, &k.Scope, &k.DeviceID, &enabled, &k.CreatedAt); err != nil {
			return nil, fmt.Errorf("读取 API Key 失败: %w", err)
		}
		k.Enabled = enabled != 0
		out = append(out, k)
	}
	return out, rows.Err()
}

// RevokeAPIKeyByRef 按哈希前缀（≥8 位）或 name 精确吊销。
// 命中 0 条返回 ErrNotFound，命中多条返回错误——宁可让人看清再动手。
func (s *Store) RevokeAPIKeyByRef(ctx context.Context, ref string) (int64, error) {
	ref = strings.TrimSpace(ref)
	if ref == "" {
		return 0, fmt.Errorf("吊销目标不能为空")
	}
	// 句柄两种形态：key_hash 的十六进制前缀（≥8 位），或 name（如 self:ab12cd34）。
	// 判据只有一条——是不是够长的十六进制串，避免用「是否以 bk_ 开头」这类巧合。
	isHashPrefix := len(ref) >= 8 && strings.IndexFunc(ref, func(r rune) bool {
		return !((r >= '0' && r <= '9') || (r >= 'a' && r <= 'f'))
	}) < 0

	var res sql.Result
	var err error
	if isHashPrefix {
		res, err = s.db.ExecContext(ctx, "UPDATE api_keys SET enabled=0 WHERE key_hash LIKE ?", ref+"%")
	} else {
		res, err = s.db.ExecContext(ctx, "UPDATE api_keys SET enabled=0 WHERE name=?", ref)
	}
	if err != nil {
		return 0, fmt.Errorf("吊销失败: %w", err)
	}
	n, _ := res.RowsAffected()
	switch {
	case n == 0:
		return 0, ErrNotFound
	case n > 1:
		return n, fmt.Errorf("%s 命中 %d 把 Key，请给更精确的句柄", ref, n)
	}
	return n, nil
}
