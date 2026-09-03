package catalog

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/example/benchmark-prompts/internal/model"
	"github.com/example/benchmark-prompts/internal/store"
)

func newStore(t *testing.T) *store.Store {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "catalog.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	if err := st.Migrate(context.Background()); err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	return st
}

// seed 造一条公开提示词并返回 id。
func seed(t *testing.T, st *store.Store, content string) string {
	t.Helper()
	res, err := st.CreatePendingPrompt(context.Background(), content, nil, "")
	if err != nil {
		t.Fatalf("CreatePendingPrompt: %v", err)
	}
	if err := st.SetStatus(context.Background(), res.PromptID, model.StatusApproved); err != nil {
		t.Fatalf("SetStatus: %v", err)
	}
	return res.PromptID
}

func TestHashIsDeterministicAndSensitive(t *testing.T) {
	ctx := context.Background()
	st := newStore(t)
	svc := New(st)

	h0, err := svc.Hash(ctx)
	if err != nil {
		t.Fatalf("Hash: %v", err)
	}
	h0b, err := svc.Hash(ctx)
	if err != nil {
		t.Fatalf("Hash: %v", err)
	}
	if h0 != h0b {
		t.Fatalf("同一数据两次 hash 必须一致：%s vs %s", h0, h0b)
	}

	id := seed(t, st, "first prompt")
	h1, err := svc.Hash(ctx)
	if err != nil {
		t.Fatalf("Hash: %v", err)
	}
	if h1 == h0 {
		t.Fatalf("新增公开条目后 hash 必须变化")
	}

	// approved → featured 只动状态与 version，也必须反映到 hash
	if err := st.SetStatus(ctx, id, model.StatusFeatured); err != nil {
		t.Fatalf("SetStatus: %v", err)
	}
	h2, err := svc.Hash(ctx)
	if err != nil {
		t.Fatalf("Hash: %v", err)
	}
	if h2 == h1 {
		t.Fatalf("version 递增后 hash 必须变化")
	}
}

func TestDeltaUnknownSinceFallsBackToFull(t *testing.T) {
	ctx := context.Background()
	st := newStore(t)
	svc := New(st)

	a := seed(t, st, "prompt alpha")
	b := seed(t, st, "prompt beta")

	res, err := svc.Delta(ctx, "no-such-hash", 10, 0)
	if err != nil {
		t.Fatalf("Delta: %v", err)
	}
	if len(res.Changes) != 2 {
		t.Fatalf("未知 since 应回退全量（2 条），得到 %d", len(res.Changes))
	}
	if res.Since == "" || res.Since == "no-such-hash" {
		t.Fatalf("必须返回新的 since，得到 %q", res.Since)
	}
	got := map[string]bool{}
	for _, p := range res.Changes {
		got[p.ID] = true
	}
	if !got[a] || !got[b] {
		t.Fatalf("全量结果缺少条目：%v", got)
	}
}

func TestDeltaAfterKnownSnapshotIncludesNewChange(t *testing.T) {
	ctx := context.Background()
	st := newStore(t)
	svc := New(st)

	seed(t, st, "already synced prompt")
	base, err := svc.Refresh(ctx)
	if err != nil {
		t.Fatalf("Refresh: %v", err)
	}

	fresh := seed(t, st, "brand new prompt")

	res, err := svc.Delta(ctx, base, 10, 0)
	if err != nil {
		t.Fatalf("Delta: %v", err)
	}
	// 注意：实现用 updated_at >= snapshot.computed_at，同一秒内的旧条目
	// 也可能被重复返回。客户端 upsert 幂等，因此"宁可多返回"是安全设计，
	// 所以这里断言"必须包含新条目"而不是断言精确条数。
	found := false
	for _, p := range res.Changes {
		if p.ID == fresh {
			found = true
		}
	}
	if !found {
		t.Fatalf("增量结果必须包含新条目 %s，得到 %+v", fresh, res.Changes)
	}
	if res.Since == base {
		t.Fatalf("since 必须推进为新 hash")
	}
}

func TestDeltaNoChangesWhenSinceIsCurrent(t *testing.T) {
	ctx := context.Background()
	st := newStore(t)
	svc := New(st)

	seed(t, st, "stable prompt")
	cur, err := svc.Refresh(ctx)
	if err != nil {
		t.Fatalf("Refresh: %v", err)
	}
	res, err := svc.Delta(ctx, cur, 10, 0)
	if err != nil {
		t.Fatalf("Delta: %v", err)
	}
	if len(res.Changes) != 0 || res.HasMore {
		t.Fatalf("since 已是最新时应返回空集，得到 changes=%d hasMore=%v", len(res.Changes), res.HasMore)
	}
}

func TestDeltaReportsDeleted(t *testing.T) {
	ctx := context.Background()
	st := newStore(t)
	svc := New(st)

	doomed := seed(t, st, "will be rejected")
	base, err := svc.Refresh(ctx)
	if err != nil {
		t.Fatalf("Refresh: %v", err)
	}
	if err := st.SetStatus(ctx, doomed, model.StatusRejected); err != nil {
		t.Fatalf("SetStatus: %v", err)
	}

	res, err := svc.Delta(ctx, base, 10, 0)
	if err != nil {
		t.Fatalf("Delta: %v", err)
	}
	found := false
	for _, id := range res.Deleted {
		if id == doomed {
			found = true
		}
	}
	if !found {
		t.Fatalf("下架条目必须出现在 deleted 中，得到 %v", res.Deleted)
	}
}

func TestDeltaPagesWithCursor(t *testing.T) {
	ctx := context.Background()
	st := newStore(t)
	svc := New(st)

	for _, c := range []string{"p1", "p2", "p3"} {
		seed(t, st, "content "+c)
	}

	first, err := svc.Delta(ctx, "", 2, 0)
	if err != nil {
		t.Fatalf("Delta page1: %v", err)
	}
	if !first.HasMore || first.NextCursor == "" {
		t.Fatalf("第一页应标记 has_more 并给出游标：%+v", first)
	}
	off, err := DecodeCursor(first.NextCursor)
	if err != nil {
		t.Fatalf("DecodeCursor: %v", err)
	}
	second, err := svc.Delta(ctx, "", 2, off)
	if err != nil {
		t.Fatalf("Delta page2: %v", err)
	}
	if len(second.Changes) != 1 {
		t.Fatalf("第二页应有 1 条，得到 %d", len(second.Changes))
	}
}

func TestCursorRoundTrip(t *testing.T) {
	for _, n := range []int{0, 1, 20, 99, 1 << 20} {
		enc := EncodeCursor(n)
		dec, err := DecodeCursor(enc)
		if err != nil {
			t.Fatalf("DecodeCursor(%q): %v", enc, err)
		}
		if dec != n {
			t.Fatalf("游标往返不一致：%d -> %q -> %d", n, enc, dec)
		}
	}
	if got, err := DecodeCursor(""); err != nil || got != 0 {
		t.Fatalf("空游标应表示首页，得到 %d err=%v", got, err)
	}
	if _, err := DecodeCursor("%%%bad%%%"); err == nil {
		t.Fatalf("非法游标必须报错")
	}
}
