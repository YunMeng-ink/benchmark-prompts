package cache

import (
	"bytes"
	"testing"
	"time"
)

func TestGetSetDelete(t *testing.T) {
	c := New(4)
	c.Set("a", []byte("1"), time.Minute)

	got, ok := c.Get("a")
	if !ok || !bytes.Equal(got, []byte("1")) {
		t.Fatalf("读取不符: ok=%v got=%q", ok, got)
	}
	if _, ok := c.Get("missing"); ok {
		t.Fatalf("不存在的键不应命中")
	}

	c.Delete("a")
	if _, ok := c.Get("a"); ok {
		t.Fatalf("Delete 后不应命中")
	}
}

func TestOverwriteKeepsSize(t *testing.T) {
	c := New(2)
	c.Set("a", []byte("1"), 0)
	c.Set("a", []byte("2"), 0)

	if c.Len() != 1 {
		t.Fatalf("覆盖同键不应增加条目，得到 %d", c.Len())
	}
	got, ok := c.Get("a")
	if !ok || !bytes.Equal(got, []byte("2")) {
		t.Fatalf("覆盖后应读到新值，得到 %q ok=%v", got, ok)
	}
}

func TestEvictsLeastRecentlyUsed(t *testing.T) {
	c := New(2)
	c.Set("a", []byte("A"), 0)
	c.Set("b", []byte("B"), 0)

	// 触碰 a，使 b 成为最久未使用
	if _, ok := c.Get("a"); !ok {
		t.Fatalf("a 应命中")
	}
	c.Set("c", []byte("C"), 0)

	if _, ok := c.Get("b"); ok {
		t.Fatalf("b 是最久未使用，应被淘汰")
	}
	if _, ok := c.Get("a"); !ok {
		t.Fatalf("a 刚被访问，应保留")
	}
	if _, ok := c.Get("c"); !ok {
		t.Fatalf("c 刚写入，应保留")
	}
	if c.Len() != 2 {
		t.Fatalf("容量应为 2，得到 %d", c.Len())
	}
}

func TestTTLExpiry(t *testing.T) {
	c := New(2)
	c.Set("short", []byte("x"), 15*time.Millisecond)
	c.Set("forever", []byte("y"), 0)

	if _, ok := c.Get("short"); !ok {
		t.Fatalf("刚写入应立即命中")
	}
	time.Sleep(30 * time.Millisecond)

	if _, ok := c.Get("short"); ok {
		t.Fatalf("过期条目不应命中")
	}
	if _, ok := c.Get("forever"); !ok {
		t.Fatalf("ttl=0 表示不过期")
	}
	// 过期条目被读取时应顺手移除，避免占容量
	if c.Len() != 1 {
		t.Fatalf("过期条目应被清除，剩余 %d", c.Len())
	}
}

func TestZeroCapacityDoesNotPanic(t *testing.T) {
	c := New(0)
	c.Set("k", []byte("v"), 0)
	c.Set("k2", []byte("v2"), 0)

	if _, ok := c.Get("k"); ok {
		t.Fatalf("容量 1 时旧键应被挤掉")
	}
	if _, ok := c.Get("k2"); !ok {
		t.Fatalf("新键应可命中")
	}
}

func TestConcurrentAccess(t *testing.T) {
	c := New(64)
	done := make(chan struct{})
	for i := 0; i < 8; i++ {
		go func(seed int) {
			defer func() { done <- struct{}{} }()
			for j := 0; j < 200; j++ {
				key := string(rune('a' + j%26))
				c.Set(key, []byte{byte(seed), byte(j)}, time.Minute)
				_, _ = c.Get(key)
				if j%17 == 0 {
					c.Delete(key)
				}
				_ = c.Len()
			}
		}(i)
	}
	for i := 0; i < 8; i++ {
		<-done
	}
}
