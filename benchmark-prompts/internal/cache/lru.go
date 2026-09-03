// Package cache 提供一个带 TTL 的进程内 LRU，用于缓存"已序列化响应字节"。
package cache

import (
	"container/list"
	"sync"
	"time"
)

type entry struct {
	key     string
	value   []byte
	expires time.Time
}

// LRU 并发安全的最近最少使用缓存。
//
// 缓存值直接存最终响应字节（含 gzip 结果），避免同一提示词被反复压缩
// —— 这是源站带宽约束下最重要的省 CPU/省延迟手段。
type LRU struct {
	mu    sync.Mutex
	max   int
	items map[string]*list.Element
	order *list.List
}

// New 创建容量为 max 的 LRU；max<=0 时退化为 1。
func New(max int) *LRU {
	if max < 1 {
		max = 1
	}
	return &LRU{
		max:   max,
		items: make(map[string]*list.Element, max),
		order: list.New(),
	}
}

// Get 读取键值；过期条目会被丢弃。
func (c *LRU) Get(key string) ([]byte, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()

	el, ok := c.items[key]
	if !ok {
		return nil, false
	}
	e := el.Value.(*entry)
	if !e.expires.IsZero() && time.Now().After(e.expires) {
		c.removeLocked(el)
		return nil, false
	}
	c.order.MoveToFront(el)
	return e.value, true
}

// Set 写入键值；ttl<=0 表示不过期。
func (c *LRU) Set(key string, value []byte, ttl time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()

	var exp time.Time
	if ttl > 0 {
		exp = time.Now().Add(ttl)
	}

	if el, ok := c.items[key]; ok {
		el.Value.(*entry).value = value
		el.Value.(*entry).expires = exp
		c.order.MoveToFront(el)
		return
	}

	c.items[key] = c.order.PushFront(&entry{key: key, value: value, expires: exp})
	if c.order.Len() > c.max {
		c.removeOldestLocked()
	}
}

// Delete 失效一个键。
func (c *LRU) Delete(key string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if el, ok := c.items[key]; ok {
		c.removeLocked(el)
	}
}

// Len 返回当前条目数。
func (c *LRU) Len() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.order.Len()
}

func (c *LRU) removeOldestLocked() {
	if el := c.order.Back(); el != nil {
		c.removeLocked(el)
	}
}

func (c *LRU) removeLocked(el *list.Element) {
	c.order.Remove(el)
	delete(c.items, el.Value.(*entry).key)
}
