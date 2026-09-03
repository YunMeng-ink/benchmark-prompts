// Package ratelimit 提供固定窗口计数限流（按 tier + endpoint + subject 分桶）。
package ratelimit

import (
	"sync"
	"time"
)

// 限流分桶名。
const (
	TierAnonymous = "anon"
	TierAuthed    = "auth"
)

type slot struct {
	count int
	until time.Time
}

// Limiter 是并发安全的固定窗口限流器。
type Limiter struct {
	mu     sync.Mutex
	win    time.Duration
	limits map[string]map[string]int
	hits   map[string]*slot
}

// New 创建限流器。limits 形如 {"anon": {"meta":10, "list":60}, "auth": {...}}，
// 数值与 docs/api.md §6 的配额表一致。
func New(win time.Duration, limits map[string]map[string]int) *Limiter {
	if win <= 0 {
		win = time.Minute
	}
	return &Limiter{win: win, limits: limits, hits: make(map[string]*slot)}
}

// Allow 判定是否放行；被拒时第二个返回值是建议等待时长。
func (l *Limiter) Allow(tier, endpoint, subject string) (bool, time.Duration) {
	limit := l.limits[tier][endpoint]
	if limit <= 0 {
		return true, 0 // 未配置即不限流
	}

	key := tier + "|" + endpoint + "|" + subject
	now := time.Now()

	l.mu.Lock()
	defer l.mu.Unlock()

	s, ok := l.hits[key]
	if !ok || now.After(s.until) {
		l.hits[key] = &slot{count: 1, until: now.Add(l.win)}
		return true, 0
	}
	if s.count >= limit {
		return false, time.Until(s.until)
	}
	s.count++
	return true, 0
}

// GC 丢弃已过期的桶，防止长期运行下 map 无界增长。
func (l *Limiter) GC() {
	now := time.Now()
	l.mu.Lock()
	defer l.mu.Unlock()
	for k, s := range l.hits {
		if now.After(s.until) {
			delete(l.hits, k)
		}
	}
}
