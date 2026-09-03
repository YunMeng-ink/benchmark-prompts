package store

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/example/benchmark-prompts/internal/model"
)

// seedPublic 造一条已公开、内容唯一的提示词。
func seedPublic(t *testing.T, st *Store, marker string, tags []string) string {
	t.Helper()
	res, err := st.CreatePendingPrompt(context.Background(), "content "+marker, tags, "")
	if err != nil {
		t.Fatalf("seed: %v", err)
	}
	publish(t, st, res.PromptID)
	return res.PromptID
}

func TestBackupProducesIndependentReadableDB(t *testing.T) {
	ctx := context.Background()
	st := newTestStore(t)
	id := seedPublic(t, st, "backup probe", nil)

	dest := filepath.Join(t.TempDir(), "bk.db")
	if err := st.Backup(ctx, dest); err != nil {
		t.Fatalf("Backup: %v", err)
	}
	fi, err := os.Stat(dest)
	if err != nil || fi.Size() == 0 {
		t.Fatalf("备份文件应存在且非空: %v", err)
	}

	// VACUUM INTO 不允许覆盖已存在的路径，必须显式报错而不是静默覆盖
	if err := st.Backup(ctx, dest); err == nil {
		t.Fatalf("目标已存在时应报错")
	}

	bk, err := Open(dest)
	if err != nil {
		t.Fatalf("备份应能被独立打开: %v", err)
	}
	t.Cleanup(func() { _ = bk.Close() })

	n, err := bk.CountApproved(ctx)
	if err != nil {
		t.Fatalf("CountApproved on backup: %v", err)
	}
	if n != 1 {
		t.Fatalf("备份内应有 1 条，得到 %d", n)
	}
	if _, err := bk.GetApproved(ctx, id); err != nil {
		t.Fatalf("备份应能读回该条: %v", err)
	}
}

func TestListByStatus(t *testing.T) {
	ctx := context.Background()
	st := newTestStore(t)

	res, err := st.CreatePendingPrompt(ctx, "still pending item", nil, "")
	if err != nil {
		t.Fatalf("seed: %v", err)
	}

	pending, err := st.ListByStatus(ctx, model.StatusPending, 10)
	if err != nil {
		t.Fatalf("ListByStatus: %v", err)
	}
	if len(pending) != 1 || pending[0].ID != res.PromptID {
		t.Fatalf("待审核队列不符: %+v", pending)
	}

	publish(t, st, res.PromptID)
	pending2, err := st.ListByStatus(ctx, model.StatusPending, 10)
	if err != nil {
		t.Fatalf("ListByStatus: %v", err)
	}
	if len(pending2) != 0 {
		t.Fatalf("通过后应离开待审核队列，得到 %d 条", len(pending2))
	}
	pub, err := st.ListByStatus(ctx, model.StatusApproved, 10)
	if err != nil {
		t.Fatalf("ListByStatus: %v", err)
	}
	if len(pub) != 1 {
		t.Fatalf("approved 应有 1 条，得到 %d", len(pub))
	}
}

func TestPublicAllPagingIsStable(t *testing.T) {
	ctx := context.Background()
	st := newTestStore(t)
	for i := 0; i < 3; i++ {
		seedPublic(t, st, fmt.Sprintf("page %d", i), nil)
	}

	all, err := st.PublicAll(ctx, 10, 0)
	if err != nil {
		t.Fatalf("PublicAll: %v", err)
	}
	if len(all) != 3 {
		t.Fatalf("全量应有 3 条，得到 %d", len(all))
	}
	for i := 1; i < len(all); i++ {
		if all[i-1].ID > all[i].ID {
			t.Fatalf("全量必须按 id 稳定排序，%s 排在 %s 之前", all[i-1].ID, all[i].ID)
		}
	}

	p1, err := st.PublicAll(ctx, 2, 0)
	if err != nil {
		t.Fatalf("PublicAll p1: %v", err)
	}
	p2, err := st.PublicAll(ctx, 2, 2)
	if err != nil {
		t.Fatalf("PublicAll p2: %v", err)
	}
	if len(p1) != 2 || len(p2) != 1 {
		t.Fatalf("分页大小不符：%d / %d", len(p1), len(p2))
	}
	if p1[0].ID == p2[0].ID {
		t.Fatalf("两页不应重叠")
	}
	// 正文必须随全量返回（客户端要靠它落本地缓存）
	if p1[0].Content == "" {
		t.Fatalf("全量结果必须含正文")
	}
}

func TestPromptsUpdatedSince(t *testing.T) {
	ctx := context.Background()
	st := newTestStore(t)
	id := seedPublic(t, st, "temporal probe", nil)

	now := time.Now().Unix()

	got, err := st.PromptsUpdatedSince(ctx, now-5, 10, 0)
	if err != nil {
		t.Fatalf("PromptsUpdatedSince: %v", err)
	}
	found := false
	for _, p := range got {
		if p.ID == id {
			found = true
		}
	}
	if !found {
		t.Fatalf("刚变更的条目应出现在增量里")
	}

	none, err := st.PromptsUpdatedSince(ctx, now+3600, 10, 0)
	if err != nil {
		t.Fatalf("PromptsUpdatedSince future: %v", err)
	}
	if len(none) != 0 {
		t.Fatalf("未来时间点应无增量，得到 %d 条", len(none))
	}
}

func TestDeletedSinceCoversUnpublishing(t *testing.T) {
	ctx := context.Background()
	st := newTestStore(t)
	id := seedPublic(t, st, "doomed item", nil)
	now := time.Now().Unix()

	before, err := st.DeletedSince(ctx, now-5)
	if err != nil {
		t.Fatalf("DeletedSince: %v", err)
	}
	for _, got := range before {
		if got == id {
			t.Fatalf("仍公开的条目不应出现在删除列表")
		}
	}

	if err := st.SetStatus(ctx, id, model.StatusRejected); err != nil {
		t.Fatalf("SetStatus: %v", err)
	}
	after, err := st.DeletedSince(ctx, now-5)
	if err != nil {
		t.Fatalf("DeletedSince after: %v", err)
	}
	found := false
	for _, got := range after {
		if got == id {
			found = true
		}
	}
	if !found {
		t.Fatalf("下架后必须进入删除列表，得到 %v", after)
	}
}

func TestRowsForHashIsSortedAndComplete(t *testing.T) {
	ctx := context.Background()
	st := newTestStore(t)
	for i := 0; i < 3; i++ {
		seedPublic(t, st, fmt.Sprintf("hash row %d", i), nil)
	}
	// pending 不得参与目录 hash
	if _, err := st.CreatePendingPrompt(ctx, "pending must not count", nil, ""); err != nil {
		t.Fatalf("seed pending: %v", err)
	}

	rows, err := st.RowsForHash(ctx)
	if err != nil {
		t.Fatalf("RowsForHash: %v", err)
	}
	if len(rows) != 3 {
		t.Fatalf("目录应只含 3 条公开条目，得到 %d", len(rows))
	}
	for i := 1; i < len(rows); i++ {
		if rows[i-1].ID > rows[i].ID {
			t.Fatalf("必须按 id 升序以保证 hash 确定性")
		}
	}
	for _, r := range rows {
		if len(r.ContentHash) != 64 {
			t.Fatalf("content_hash 应为完整 sha256 hex，得到 %q", r.ContentHash)
		}
		if r.Version < 1 {
			t.Fatalf("version 应 >=1，得到 %d", r.Version)
		}
	}
}

func TestRandomApprovedWith(t *testing.T) {
	ctx := context.Background()
	st := newTestStore(t)
	seedPublic(t, st, "tagged item", []string{"tagA"})
	seedPublic(t, st, "another tagged item", []string{"tagB"})

	got, err := st.RandomApproved(ctx, "tagA", nil)
	if err != nil {
		t.Fatalf("按标签随机失败: %v", err)
	}
	if len(got.Tags) != 1 || got.Tags[0] != "tagA" {
		t.Fatalf("标签过滤失效: %+v", got.Tags)
	}

	// 精确匹配，不得因子串包含而误命中
	if _, err := st.RandomApproved(ctx, "tag", nil); !errors.Is(err, ErrNotFound) {
		t.Fatalf("标签应精确匹配，得到 %v", err)
	}
	if _, err := st.RandomApproved(ctx, "nope", nil); !errors.Is(err, ErrNotFound) {
		t.Fatalf("无匹配标签应 ErrNotFound，得到 %v", err)
	}
}

func TestListApprovedTagIsExactMatch(t *testing.T) {
	ctx := context.Background()
	st := newTestStore(t)
	seedPublic(t, st, "one tagged item", []string{"coding"})

	hits, err := st.ListApproved(ctx, "coding", 10, 0)
	if err != nil {
		t.Fatalf("ListApproved: %v", err)
	}
	if len(hits) != 1 {
		t.Fatalf("应命中 1 条，得到 %d", len(hits))
	}
	miss, err := st.ListApproved(ctx, "cod", 10, 0)
	if err != nil {
		t.Fatalf("ListApproved: %v", err)
	}
	if len(miss) != 0 {
		t.Fatalf("标签子串不应匹配，得到 %d 条", len(miss))
	}
}

func TestSetStatusRejectsUnknownStatus(t *testing.T) {
	ctx := context.Background()
	st := newTestStore(t)
	id := seedPublic(t, st, "bad transition", nil)

	if err := st.SetStatus(ctx, id, "bogus"); err == nil {
		t.Fatalf("非法状态必须被拒绝")
	}
	if err := st.SetStatus(ctx, "p_missing", model.StatusApproved); !errors.Is(err, ErrNotFound) {
		t.Fatalf("不存在的 id 应 ErrNotFound，得到 %v", err)
	}
}
