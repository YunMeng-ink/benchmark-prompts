// Package catalog 负责目录 hash 与 delta 变更集计算（增量同步的根基）。
//
// 为什么不叫 internal/sync：会与标准库 sync 包名冲突，import 后无法再使用
// sync.Mutex。故按语义命名为 catalog —— docs/architecture.md 已同步更正。
package catalog

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/example/benchmark-prompts/internal/model"
	"github.com/example/benchmark-prompts/internal/store"
)

// Service 计算并登记目录快照。
type Service struct {
	st *store.Store
}

// New 构造目录服务。
func New(st *store.Store) *Service { return &Service{st: st} }

// Result 是一次 delta 计算结果。
type Result struct {
	Changes    []*model.Prompt
	Deleted    []string
	Since      string
	HasMore    bool
	NextCursor string
}

// Hash 计算确定性目录 hash：
//
//	sha256( 按 id 升序拼接 "id:version:content_hash"，以 \n 分隔 )
//
// 只用三要素即可覆盖所有内容变化：正文变→content_hash 变；元数据变→version 变。
func (s *Service) Hash(ctx context.Context) (string, error) {
	rows, err := s.st.RowsForHash(ctx)
	if err != nil {
		return "", err
	}
	var b strings.Builder
	for i, r := range rows {
		if i > 0 {
			b.WriteByte('\n')
		}
		b.WriteString(r.ID)
		b.WriteByte(':')
		b.WriteString(strconv.FormatInt(r.Version, 10))
		b.WriteByte(':')
		b.WriteString(r.ContentHash)
	}
	sum := sha256.Sum256([]byte(b.String()))
	return hex.EncodeToString(sum[:]), nil
}

// Refresh 重算 hash 并登记快照（同一 hash 只会登记一次），返回当前 hash。
func (s *Service) Refresh(ctx context.Context) (string, error) {
	h, err := s.Hash(ctx)
	if err != nil {
		return "", err
	}
	if err := s.st.RecordSnapshot(ctx, h, time.Now().Unix()); err != nil {
		return "", err
	}
	return h, nil
}

// Delta 计算增量变更集。
//
// since 为空、等于当前 hash、或在 catalog_snapshots 查不到时，分别处理为
// "无变化 / 无变化 / 回退全量分页"。limit 为每页条数（不含判断位）。
func (s *Service) Delta(ctx context.Context, since string, limit, offset int) (*Result, error) {
	current, err := s.Refresh(ctx)
	if err != nil {
		return nil, err
	}
	res := &Result{Changes: []*model.Prompt{}, Deleted: []string{}, Since: current}

	if since == current {
		return res, nil
	}

	var sinceTS int64
	resolved := false
	if since != "" {
		ts, ok, lerr := s.st.LookupSnapshot(ctx, since)
		if lerr != nil {
			return nil, lerr
		}
		if ok {
			sinceTS, resolved = ts, true
		}
	}

	if resolved {
		// 增量：注意用 >= 而非 >。
		// 同一秒内"登记快照"与"内容变更"可能落在同一时间戳，用 > 会漏掉该变更；
		// 客户端 upsert/delete 均为幂等，多返回一点是安全的。
		prompts, err := s.st.PromptsUpdatedSince(ctx, sinceTS, limit+1, offset)
		if err != nil {
			return nil, err
		}
		res.HasMore = len(prompts) > limit
		res.Changes = trim(prompts, limit)
		if offset == 0 {
			deleted, derr := s.st.DeletedSince(ctx, sinceTS)
			if derr != nil {
				return nil, derr
			}
			res.Deleted = deleted
		}
	} else {
		// 回退全量：首次同步或 since 过旧。
		prompts, err := s.st.PublicAll(ctx, limit+1, offset)
		if err != nil {
			return nil, err
		}
		res.HasMore = len(prompts) > limit
		res.Changes = trim(prompts, limit)
	}

	if res.HasMore {
		res.NextCursor = EncodeCursor(offset + limit)
	}
	return res, nil
}

func trim(ps []*model.Prompt, limit int) []*model.Prompt {
	if len(ps) <= limit {
		return ps
	}
	return ps[:limit]
}

// EncodeCursor 生成不透明游标（客户端不得解析其内容）。
func EncodeCursor(offset int) string {
	return base64.RawURLEncoding.EncodeToString([]byte(strconv.Itoa(offset)))
}

// DecodeCursor 解析游标；空串表示首页。
func DecodeCursor(s string) (int, error) {
	if s == "" {
		return 0, nil
	}
	raw, err := base64.RawURLEncoding.DecodeString(s)
	if err != nil {
		return 0, fmt.Errorf("游标格式非法: %w", err)
	}
	n, err := strconv.Atoi(string(raw))
	if err != nil || n < 0 {
		return 0, fmt.Errorf("游标数值非法")
	}
	return n, nil
}
