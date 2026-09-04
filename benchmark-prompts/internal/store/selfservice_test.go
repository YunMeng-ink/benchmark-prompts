package store

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestMigrationUpgradesLegacyKeysToAdmin 复现「老库升级」现场：先只应用 0001、
// 插入一行运维 Key，再应用 0002。若 0002 里那句 UPDATE 缺失，新列的
// DEFAULT 'writer' 会把既有运维 Key 静默降级，升级后 /-/metrics 突然 401。
// 新建库测不到这件事，所以必须手工构造升级前的状态。
func TestMigrationUpgradesLegacyKeysToAdmin(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "upgrade.db")
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatalf("打开库失败: %v", err)
	}
	defer func() { _ = db.Close() }()

	for _, f := range []string{"migrations/0001_init.sql", "migrations/0002_selfservice.sql"} {
		body, err := os.ReadFile(f)
		if err != nil {
			t.Fatalf("读取 %s 失败（测试依赖包内迁移文件）: %v", f, err)
		}
		if f == "migrations/0002_selfservice.sql" {
			// 升级前：按 0001 的列集合插一行，模拟 v0.1.0 已有运维 Key。
			if _, err := db.Exec(
				`INSERT INTO api_keys(key_hash, secret_enc, name, enabled, created_at)
				 VALUES('deadbeef','x','alice',1,1700000000)`); err != nil {
				t.Fatalf("插入旧行失败: %v", err)
			}
		}
		if _, err := db.Exec(string(body)); err != nil {
			t.Fatalf("应用 %s 失败: %v", f, err)
		}
	}

	var scope string
	if err := db.QueryRow("SELECT scope FROM api_keys WHERE name='alice'").Scan(&scope); err != nil {
		t.Fatalf("读 scope 失败: %v", err)
	}
	if scope != ScopeAdmin {
		t.Fatalf("既有运维 Key 必须升为 admin，得到 %q", scope)
	}
}

func TestSelfRegisterFlow(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()

	code, err := st.CreateInvite(ctx, "测试码", 2, time.Hour)
	if err != nil {
		t.Fatalf("签发邀请码失败: %v", err)
	}
	if len(code) != 11 || code[5] != '-' {
		t.Fatalf("邀请码格式应为 XXXXX-XXXXX，得到 %q", code)
	}

	// 1. 正常注册：拿到 writer 作用域的 Key，且能按哈希查回。
	info, plain, err := st.RegisterSelfKey(ctx, code, "dev-1", "我的设备")
	if err != nil {
		t.Fatalf("注册失败: %v", err)
	}
	if len(plain) < 30 || plain[:3] != "bk_" {
		t.Fatalf("Key 形态不对: %q", plain)
	}
	if info.Scope != ScopeWriter {
		t.Fatalf("自助 Key 必须是 writer，得到 %q", info.Scope)
	}
	rec, err := st.LookupAPIKey(ctx, KeyHash(plain))
	if err != nil {
		t.Fatalf("按哈希查不到刚签发的 Key: %v", err)
	}
	if rec.Scope != ScopeWriter || rec.DeviceID != "dev-1" || !rec.Enabled {
		t.Fatalf("Key 记录不符: %+v", rec)
	}

	// 2. 一设备一 Key：重复注册必须被拒，而不是悄悄发第二把。
	if _, _, err := st.RegisterSelfKey(ctx, code, "dev-1", ""); !errors.Is(err, ErrDeviceTaken) {
		t.Fatalf("同设备重复注册应报 ErrDeviceTaken，得到 %v", err)
	}

	// 3. 邀请码按 max_uses 消费：第二台设备用掉最后一个名额，第三台就该失败。
	if _, _, err := st.RegisterSelfKey(ctx, code, "dev-2", ""); err != nil {
		t.Fatalf("第二个名额应可用: %v", err)
	}
	if _, _, err := st.RegisterSelfKey(ctx, code, "dev-3", ""); !errors.Is(err, ErrInviteInvalid) {
		t.Fatalf("码用尽应报 ErrInviteInvalid，得到 %v", err)
	}

	// 4. 不存在的码。
	if _, _, err := st.RegisterSelfKey(ctx, "NOSUCH-CODE", "dev-9", ""); !errors.Is(err, ErrInviteInvalid) {
		t.Fatalf("无效码应报 ErrInviteInvalid，得到 %v", err)
	}

	// 5. 失败的注册不得消费名额：先用一个已被占用的设备去撞（回滚），
	//    再用新设备去用同一个单名额码——若失败那次已扣数，这里就会用尽。
	fresh, err := st.CreateInvite(ctx, "单名额", 1, 0)
	if err != nil {
		t.Fatalf("签发失败: %v", err)
	}
	if _, _, err := st.RegisterSelfKey(ctx, fresh, "dev-1", ""); !errors.Is(err, ErrDeviceTaken) {
		t.Fatalf("已被占用的设备应报 ErrDeviceTaken，得到 %v", err)
	}
	if _, _, err := st.RegisterSelfKey(ctx, fresh, "dev-x", ""); err != nil {
		t.Fatalf("失败那次不应扣名额，新设备应能用掉它: %v", err)
	}
	if _, _, err := st.RegisterSelfKey(ctx, fresh, "dev-y", ""); !errors.Is(err, ErrInviteInvalid) {
		t.Fatalf("名额现已用尽，应报 ErrInviteInvalid，得到 %v", err)
	}

	invites, err := st.ListInvites(ctx)
	if err != nil {
		t.Fatalf("列邀请码失败: %v", err)
	}
	if len(invites) < 2 {
		t.Fatalf("至少应有 2 个码，得到 %d", len(invites))
	}
}

func TestInviteExpiryAndDisable(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()

	// 已过期的码：直接构造一行 expires_at 在过去的记录。
	code, err := st.CreateInvite(ctx, "过期码", 1, time.Second)
	if err != nil {
		t.Fatalf("签发失败: %v", err)
	}
	if _, err := st.db.ExecContext(ctx,
		"UPDATE invite_codes SET expires_at=? WHERE code_hash=?", time.Now().Add(-time.Hour).Unix(), KeyHash(code)); err != nil {
		t.Fatalf("改过期时间失败: %v", err)
	}
	if _, _, err := st.RegisterSelfKey(ctx, code, "dev-e", ""); !errors.Is(err, ErrInviteInvalid) {
		t.Fatalf("过期码应被拒，得到 %v", err)
	}

	// 停用的码。
	code2, err := st.CreateInvite(ctx, "停用码", 1, 0)
	if err != nil {
		t.Fatalf("签发失败: %v", err)
	}
	if _, err := st.db.ExecContext(ctx, "UPDATE invite_codes SET enabled=0 WHERE code_hash=?", KeyHash(code2)); err != nil {
		t.Fatalf("停用失败: %v", err)
	}
	if _, _, err := st.RegisterSelfKey(ctx, code2, "dev-d", ""); !errors.Is(err, ErrInviteInvalid) {
		t.Fatalf("停用码应被拒，得到 %v", err)
	}
}

func TestKeyRevokePaths(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()

	code, err := st.CreateInvite(ctx, "吊销用", 3, 0)
	if err != nil {
		t.Fatalf("签发失败: %v", err)
	}
	info, plain, err := st.RegisterSelfKey(ctx, code, "dev-r", "笔记本")
	if err != nil {
		t.Fatalf("注册失败: %v", err)
	}
	if !strings.Contains(info.Name, "笔记本") || info.DeviceID != "dev-r" || !info.Enabled {
		t.Fatalf("注册返回值不符: %+v", info)
	}
	hash := KeyHash(plain)

	// 自视信息：不泄露明文，只给句柄与元数据。
	self, err := st.SelfKey(ctx, hash)
	if err != nil {
		t.Fatalf("SelfKey 失败: %v", err)
	}
	if self.Ref != hash[:12] || self.DeviceID != "dev-r" || self.Scope != ScopeWriter {
		t.Fatalf("自视信息不符: %+v", self)
	}

	// 使用者自助停用。
	if err := st.DisableAPIKey(ctx, hash); err != nil {
		t.Fatalf("停用失败: %v", err)
	}
	if rec, err := st.LookupAPIKey(ctx, hash); err != nil || rec.Enabled {
		t.Fatalf("停用后 Enabled 应为 false，得到 %+v err=%v", rec, err)
	}
	// 幂等：再停一次仍成功。
	if err := st.DisableAPIKey(ctx, hash); err != nil {
		t.Fatalf("重复停用应幂等: %v", err)
	}
	if _, err := st.SelfKey(ctx, "0000000000000000"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("不存在的哈希应 ErrNotFound，得到 %v", err)
	}

	// 运维按 name 吊销另一把。
	info2, _, err := st.RegisterSelfKey(ctx, code, "dev-r2", "")
	if err != nil {
		t.Fatalf("注册第二把失败: %v", err)
	}
	if n, err := st.RevokeAPIKeyByRef(ctx, info2.Name); err != nil || n != 1 {
		t.Fatalf("按 name 吊销应命中 1 条，得到 n=%d err=%v", n, err)
	}
	// 按哈希前缀吊销第三把。
	_, plain3, err := st.RegisterSelfKey(ctx, code, "dev-r3", "")
	if err != nil {
		t.Fatalf("注册第三把失败: %v", err)
	}
	prefix := KeyHash(plain3)[:12]
	if n, err := st.RevokeAPIKeyByRef(ctx, prefix); err != nil || n != 1 {
		t.Fatalf("按前缀吊销应命中 1 条，得到 n=%d err=%v", n, err)
	}
	if _, err := st.RevokeAPIKeyByRef(ctx, "不存在的名"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("未命中应 ErrNotFound，得到 %v", err)
	}

	// 列表可核对总数与状态。
	keys, err := st.ListAPIKeys(ctx)
	if err != nil {
		t.Fatalf("列 Key 失败: %v", err)
	}
	if len(keys) != 3 {
		t.Fatalf("应有 3 把自助 Key，得到 %d", len(keys))
	}
	for _, k := range keys[1:] { // 最新在前，第 2、3 把已吊销
		if k.Enabled {
			t.Fatalf("吊销后仍显示可用: %+v", k)
		}
	}
}
