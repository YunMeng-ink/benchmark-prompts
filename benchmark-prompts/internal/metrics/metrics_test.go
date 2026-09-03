package metrics

import (
	"context"
	"testing"
	"time"
)

func TestSnapshotShape(t *testing.T) {
	r := &Registry{}
	r.Requests.Add(3)
	r.Responses.Add(3)
	r.BytesOut.Add(1000)
	r.Hits304.Add(2)
	r.Limited.Add(1)
	r.Errors5xx.Add(4)
	r.SetRate(128*1024, true)

	s := r.Snapshot()
	checks := map[string]int64{
		"requests": 3, "responses": 3, "bytes_out": 1000,
		"hits_304": 2, "rate_limited": 1, "errors_5xx": 4,
	}
	for k, want := range checks {
		got, ok := s[k].(int64)
		if !ok {
			t.Fatalf("%s 类型应为 int64，实际 %T", k, s[k])
		}
		if got != want {
			t.Fatalf("%s 期望 %d 得到 %d", k, want, got)
		}
	}
	if deg, ok := s["bandwidth_degraded"].(bool); !ok || !deg {
		t.Fatalf("bandwidth_degraded 应为 true，得到 %v", s["bandwidth_degraded"])
	}
	// 128 KiB/s ≈ 1.0486 Mbps
	bps, ok := s["egress_bps"].(float64)
	if !ok {
		t.Fatalf("egress_bps 类型应为 float64，实际 %T", s["egress_bps"])
	}
	if bps < 131000 || bps > 131140 {
		t.Fatalf("egress_bps 不符: %v", bps)
	}
	if !r.Degraded() {
		t.Fatalf("Degraded 应为 true")
	}
}

func TestWatchdogFlagsExcessTraffic(t *testing.T) {
	r := &Registry{}
	// 阈值 0.001 Mbps ≈ 125 B/s，制造远超它的流量
	wd := NewWatchdog(r, 0.001, 10*time.Millisecond, 3)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go wd.Run(ctx)

	for i := 0; i < 60; i++ {
		r.BytesOut.Add(5000)
		time.Sleep(3 * time.Millisecond)
	}

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if r.Degraded() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("持续超阈值后必须进入降级状态")
}

func TestWatchdogStaysQuietBelowThreshold(t *testing.T) {
	r := &Registry{}
	wd := NewWatchdog(r, 100, 10*time.Millisecond, 3) // 阈值 100 Mbps，几乎不可能触发

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go wd.Run(ctx)

	for i := 0; i < 30; i++ {
		r.BytesOut.Add(100)
		time.Sleep(3 * time.Millisecond)
	}
	time.Sleep(60 * time.Millisecond)

	if r.Degraded() {
		t.Fatalf("远低于阈值时不应降级")
	}
}

func TestWatchdogWindowIsAtLeastOne(t *testing.T) {
	wd := NewWatchdog(&Registry{}, 1, time.Millisecond, 0)
	if len(wd.samples) != 1 {
		t.Fatalf("window<=0 应回退为 1，得到 %d", len(wd.samples))
	}
}
