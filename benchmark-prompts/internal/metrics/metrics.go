// Package metrics 提供轻量内存指标与 2M 带宽看门狗。
//
// 项目规模不需要 Prometheus：一个原子计数器 + 滑动速率采样即可满足
// "接近带宽上限就降级低优先级端点" 这一硬约束（docs/deployment.md §6）。
package metrics

import (
	"context"
	"sync"
	"sync/atomic"
	"time"
)

// Registry 汇总内存指标。
type Registry struct {
	Requests  atomic.Int64
	Responses atomic.Int64
	BytesOut  atomic.Int64
	Hits304   atomic.Int64
	Limited   atomic.Int64
	Errors5xx atomic.Int64

	mu        sync.RWMutex
	egressBps float64
	degraded  bool
}

// SetRate 由看门狗写入当前平均出站速率与降级标志。
func (r *Registry) SetRate(bps float64, degraded bool) {
	r.mu.Lock()
	r.egressBps = bps
	r.degraded = degraded
	r.mu.Unlock()
}

// Degraded 报告是否处于带宽降级状态。
func (r *Registry) Degraded() bool {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.degraded
}

// Snapshot 输出指标快照，供 /-/metrics 采集。
func (r *Registry) Snapshot() map[string]any {
	r.mu.RLock()
	bps, deg := r.egressBps, r.degraded
	r.mu.RUnlock()

	return map[string]any{
		"requests":           r.Requests.Load(),
		"responses":          r.Responses.Load(),
		"bytes_out":          r.BytesOut.Load(),
		"egress_bps":         bps,
		"egress_mbps":        bps * 8 / 1e6,
		"bandwidth_degraded": deg,
		"hits_304":           r.Hits304.Load(),
		"rate_limited":       r.Limited.Load(),
		"errors_5xx":         r.Errors5xx.Load(),
	}
}

// Watchdog 周期性把累计出站字节换算为速率，超过阈值即置降级标志。
type Watchdog struct {
	reg      *Registry
	maxBps   float64
	interval time.Duration
	samples  []float64
	idx      int
	filled   int
	prev     int64
	prevAt   time.Time
}

// NewWatchdog 构造看门狗。maxMbps 为允许的最大出站带宽（Mbps），
// window 是滑动采样个数（interval*window ≈ 观察窗口长度）。
func NewWatchdog(reg *Registry, maxMbps float64, interval time.Duration, window int) *Watchdog {
	if window < 1 {
		window = 1
	}
	return &Watchdog{
		reg:      reg,
		maxBps:   maxMbps * 1e6 / 8,
		interval: interval,
		samples:  make([]float64, window),
	}
}

// Run 阻塞运行直到 ctx 被取消。
func (w *Watchdog) Run(ctx context.Context) {
	t := time.NewTicker(w.interval)
	defer t.Stop()

	w.prev = w.reg.BytesOut.Load()
	w.prevAt = time.Now()

	for {
		select {
		case <-ctx.Done():
			return
		case now := <-t.C:
			cur := w.reg.BytesOut.Load()
			secs := now.Sub(w.prevAt).Seconds()
			if secs <= 0 {
				secs = 1
			}
			rate := float64(cur-w.prev) / secs
			if rate < 0 {
				rate = 0 // 计数回绕兜底
			}
			w.prev, w.prevAt = cur, now

			w.samples[w.idx] = rate
			w.idx = (w.idx + 1) % len(w.samples)
			if w.filled < len(w.samples) {
				w.filled++
			}

			var sum float64
			for i := 0; i < w.filled; i++ {
				sum += w.samples[i]
			}
			avg := sum / float64(w.filled)
			w.reg.SetRate(avg, avg > w.maxBps)
		}
	}
}
