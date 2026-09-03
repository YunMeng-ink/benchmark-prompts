package ratelimit

import (
	"testing"
	"time"
)

func TestAllowUntilLimit(t *testing.T) {
	l := New(time.Minute, map[string]map[string]int{
		TierAnonymous: {"meta": 3},
	})

	for i := 0; i < 3; i++ {
		if ok, wait := l.Allow(TierAnonymous, "meta", "ip-1"); !ok {
			t.Fatalf("第 %d 次应放行，却被打回（wait=%v）", i+1, wait)
		}
	}
	ok, wait := l.Allow(TierAnonymous, "meta", "ip-1")
	if ok {
		t.Fatalf("第 4 次必须被限流")
	}
	if wait <= 0 {
		t.Fatalf("被拒时必须给出等待时长，得到 %v", wait)
	}
	if wait > time.Minute {
		t.Fatalf("等待时长不应超过窗口，得到 %v", wait)
	}
}

func TestSubjectsAreIndependent(t *testing.T) {
	l := New(time.Minute, map[string]map[string]int{
		TierAnonymous: {"list": 1},
		TierAuthed:    {"list": 1},
	})

	if ok, _ := l.Allow(TierAnonymous, "list", "ip-A"); !ok {
		t.Fatalf("ip-A 首次应放行")
	}
	if ok, _ := l.Allow(TierAnonymous, "list", "ip-B"); !ok {
		t.Fatalf("ip-B 应独立于 ip-A")
	}
	if ok, _ := l.Allow(TierAuthed, "list", "ip-A"); !ok {
		t.Fatalf("authed 与 anon 应分属不同配额")
	}
	if ok, _ := l.Allow(TierAnonymous, "get", "ip-A"); !ok {
		t.Fatalf("不同 endpoint 应独立计数")
	}
}

func TestUnconfiguredEndpointIsNeverLimited(t *testing.T) {
	l := New(time.Minute, map[string]map[string]int{
		TierAnonymous: {"meta": 1},
	})
	for i := 0; i < 50; i++ {
		if ok, _ := l.Allow(TierAnonymous, "unlimited", "ip"); !ok {
			t.Fatalf("未配置配额的 endpoint 不应被限流")
		}
	}
	// tier 完全缺失同样不限流
	if ok, _ := l.Allow("ghost", "meta", "ip"); !ok {
		t.Fatalf("未知 tier 应放行")
	}
}

func TestWindowResets(t *testing.T) {
	l := New(20*time.Millisecond, map[string]map[string]int{
		TierAnonymous: {"delta": 1},
	})
	if ok, _ := l.Allow(TierAnonymous, "delta", "ip"); !ok {
		t.Fatalf("首次应放行")
	}
	if ok, _ := l.Allow(TierAnonymous, "delta", "ip"); ok {
		t.Fatalf("窗口内第二次应被拒")
	}
	time.Sleep(30 * time.Millisecond)
	if ok, _ := l.Allow(TierAnonymous, "delta", "ip"); !ok {
		t.Fatalf("窗口滑过后应恢复放行")
	}
}

func TestGCReclaimsExpiredSlots(t *testing.T) {
	l := New(10*time.Millisecond, map[string]map[string]int{
		TierAnonymous: {"meta": 1},
	})
	for _, ip := range []string{"a", "b", "c"} {
		if ok, _ := l.Allow(TierAnonymous, "meta", ip); !ok {
			t.Fatalf("%s 首次访问应放行", ip)
		}
	}
	l.mu.Lock()
	before := len(l.hits)
	l.mu.Unlock()
	if before != 3 {
		t.Fatalf("应有 3 个桶，得到 %d", before)
	}

	time.Sleep(25 * time.Millisecond)
	l.GC()

	l.mu.Lock()
	after := len(l.hits)
	l.mu.Unlock()
	if after != 0 {
		t.Fatalf("GC 应清掉过期桶，剩余 %d", after)
	}
}

func TestZeroWindowFallsBackToMinute(t *testing.T) {
	l := New(0, map[string]map[string]int{TierAnonymous: {"get": 1}})
	if l.win != time.Minute {
		t.Fatalf("window<=0 应回退为 1 分钟，得到 %v", l.win)
	}
}
