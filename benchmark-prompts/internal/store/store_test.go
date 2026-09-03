package store

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"github.com/example/benchmark-prompts/internal/model"
)

// makeMasterKey 生成确定性的 32 字节主密钥（仅测试用）。
func makeMasterKey() []byte {
	k := make([]byte, 32)
	for i := range k {
		k[i] = byte(i)
	}
	return k
}

func newTestStore(t *testing.T) *Store {
	t.Helper()
	st, err := Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	if err := st.Migrate(context.Background()); err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	return st
}

// publish 把提示词置为 approved（模拟审核动作）。
func publish(t *testing.T, st *Store, id string) {
	t.Helper()
	if err := st.SetStatus(context.Background(), id, model.StatusApproved); err != nil {
		t.Fatalf("SetStatus: %v", err)
	}
}

func TestMigrateIsIdempotent(t *testing.T) {
	st := newTestStore(t)
	if err := st.Migrate(context.Background()); err != nil {
		t.Fatalf("重复迁移应无副作用，得到: %v", err)
	}
}

func TestUploadIdempotencyAndContentDedup(t *testing.T) {
	ctx := context.Background()
	st := newTestStore(t)

	// 存储保留原始内部空白，hash 用折叠空白后的正文
	res1, err := st.CreatePendingPrompt(ctx, "  hello   world ", []string{"coding"}, "client-1")
	if err != nil {
		t.Fatalf("首次上传失败: %v", err)
	}
	if res1.Reused || res1.Status != model.StatusPending {
		t.Fatalf("首次上传应为新建+pending，得到 %+v", res1)
	}

	got, err := st.GetByID(ctx, res1.PromptID)
	if err != nil {
		t.Fatalf("GetByID: %v", err)
	}
	if got.Content != "hello   world" {
		t.Fatalf("正文内部空白不应被折叠，得到 %q", got.Content)
	}

	// 同一 clientId 重放 → 幂等返回原 id
	res2, err := st.CreatePendingPrompt(ctx, "totally different", nil, "client-1")
	if err != nil {
		t.Fatalf("幂等重放失败: %v", err)
	}
	if res2.PromptID != res1.PromptID || !res2.Reused {
		t.Fatalf("clientId 幂等失效: %+v", res2)
	}

	// 不同 clientId、内容等价 → 内容去重
	res3, err := st.CreatePendingPrompt(ctx, "hello world", nil, "client-2")
	if err != nil {
		t.Fatalf("去重上传失败: %v", err)
	}
	if res3.PromptID != res1.PromptID || !res3.Reused {
		t.Fatalf("content_hash 去重失效: %+v", res3)
	}
}

func TestListPagination(t *testing.T) {
	ctx := context.Background()
	st := newTestStore(t)

	// 内容必须互不相同，否则会被 content_hash 去重合成一条
	for i := 0; i < 5; i++ {
		res, err := st.CreatePendingPrompt(ctx, fmt.Sprintf("prompt #%d unique marker text", i), nil, "")
		if err != nil {
			t.Fatalf("seed 失败: %v", err)
		}
		publish(t, st, res.PromptID)
	}

	total, err := st.CountApproved(ctx)
	if err != nil {
		t.Fatalf("CountApproved: %v", err)
	}
	if total != 5 {
		t.Fatalf("应有 5 条公开提示词，得到 %d", total)
	}

	page, err := st.ListApproved(ctx, "", 2, 0)
	if err != nil {
		t.Fatalf("ListApproved: %v", err)
	}
	if len(page) != 2 {
		t.Fatalf("首页应有 2 条摘要，得到 %d", len(page))
	}
	for _, p := range page {
		if p.ID == "" {
			t.Fatalf("摘要缺少 id")
		}
	}
}

func TestRandomExclude(t *testing.T) {
	ctx := context.Background()
	st := newTestStore(t)

	res, err := st.CreatePendingPrompt(ctx, "only one prompt here", []string{"solo"}, "")
	if err != nil {
		t.Fatalf("seed: %v", err)
	}
	publish(t, st, res.PromptID)

	got, err := st.RandomApproved(ctx, "", nil)
	if err != nil {
		t.Fatalf("RandomApproved: %v", err)
	}
	if got.ID != res.PromptID {
		t.Fatalf("期望 %s，得到 %s", res.PromptID, got.ID)
	}

	if _, err := st.RandomApproved(ctx, "", []string{res.PromptID}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("排除唯一条目后应返回 ErrNotFound，得到 %v", err)
	}
}

func TestScoreIsIdempotentPerDevice(t *testing.T) {
	ctx := context.Background()
	st := newTestStore(t)

	res, err := st.CreatePendingPrompt(ctx, "score me please", nil, "")
	if err != nil {
		t.Fatalf("seed: %v", err)
	}
	publish(t, st, res.PromptID)

	if _, _, err := st.UpsertScore(ctx, res.PromptID, 5, "dev-A"); err != nil {
		t.Fatalf("UpsertScore: %v", err)
	}
	avg, count, err := st.UpsertScore(ctx, res.PromptID, 3, "dev-A")
	if err != nil {
		t.Fatalf("重复评分应被覆盖: %v", err)
	}
	if count != 1 {
		t.Fatalf("同一 deviceId 只应计 1 票，得到 %d", count)
	}
	if avg != 3 {
		t.Fatalf("重复评分应覆盖为 3，得到 %v", avg)
	}

	avg, count, err = st.UpsertScore(ctx, res.PromptID, 5, "dev-B")
	if err != nil {
		t.Fatalf("第二个设备评分失败: %v", err)
	}
	if count != 2 || avg != 4 {
		t.Fatalf("期望 avg=4 count=2，得到 avg=%v count=%d", avg, count)
	}

	if _, _, err := st.UpsertScore(ctx, "p_missing", 5, "dev-C"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("对不存在的提示词评分应 ErrNotFound，得到 %v", err)
	}
}

func TestSetStatusBumpsVersion(t *testing.T) {
	ctx := context.Background()
	st := newTestStore(t)

	res, err := st.CreatePendingPrompt(ctx, "version bump target", nil, "")
	if err != nil {
		t.Fatalf("seed: %v", err)
	}
	before, err := st.GetByID(ctx, res.PromptID)
	if err != nil {
		t.Fatalf("GetByID: %v", err)
	}
	if before.Version != 1 {
		t.Fatalf("新建 version 应为 1，得到 %d", before.Version)
	}

	publish(t, st, res.PromptID)
	after, err := st.GetApproved(ctx, res.PromptID)
	if err != nil {
		t.Fatalf("GetApproved: %v", err)
	}
	if after.Version <= before.Version {
		t.Fatalf("状态变更必须递增 version（驱动 ETag 与 delta），得到 %d", after.Version)
	}
}

func TestSnapshotLookupAndPrune(t *testing.T) {
	ctx := context.Background()
	st := newTestStore(t)

	const h = "abc123hash"
	old := time.Now().Unix() - 40*24*3600
	if err := st.RecordSnapshot(ctx, h, old); err != nil {
		t.Fatalf("RecordSnapshot: %v", err)
	}
	if err := st.RecordSnapshot(ctx, h, time.Now().Unix()); err != nil {
		t.Fatalf("重复登记应被忽略: %v", err)
	}
	ts, ok, err := st.LookupSnapshot(ctx, h)
	if err != nil {
		t.Fatalf("LookupSnapshot: %v", err)
	}
	if !ok {
		t.Fatalf("快照应能命中")
	}
	if ts != old {
		t.Fatalf("重复登记不应覆盖首次时间戳，得到 %d 期望 %d", ts, old)
	}

	// 裁剪会保留最新一条，因此先登记一个更新的
	if err := st.RecordSnapshot(ctx, "newer", time.Now().Unix()); err != nil {
		t.Fatalf("RecordSnapshot: %v", err)
	}
	if err := st.PruneSnapshots(ctx, 30*24*3600); err != nil {
		t.Fatalf("PruneSnapshots: %v", err)
	}
	if _, ok, err := st.LookupSnapshot(ctx, h); err != nil || ok {
		t.Fatalf("过期快照应被裁剪，命中=%v err=%v", ok, err)
	}
	if _, ok, err := st.LookupSnapshot(ctx, "newer"); err != nil || !ok {
		t.Fatalf("最新快照必须保留，命中=%v err=%v", ok, err)
	}
}

func TestAPIKeyRoundTrip(t *testing.T) {
	ctx := context.Background()
	st := newTestStore(t)
	master := makeMasterKey()

	if err := st.PutAPIKey(ctx, "plain-key-123", "alice", "plain-secret", master); err != nil {
		t.Fatalf("PutAPIKey: %v", err)
	}
	rec, err := st.LookupAPIKey(ctx, KeyHash("plain-key-123"))
	if err != nil {
		t.Fatalf("LookupAPIKey: %v", err)
	}
	if rec.Name != "alice" || !rec.Enabled {
		t.Fatalf("记录不符: %+v", rec)
	}
	if _, err := st.LookupAPIKey(ctx, KeyHash("wrong-key")); !errors.Is(err, ErrNotFound) {
		t.Fatalf("错误 key 应 ErrNotFound，得到 %v", err)
	}
}
