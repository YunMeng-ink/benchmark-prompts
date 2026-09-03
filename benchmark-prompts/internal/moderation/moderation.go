// Package moderation 做上传内容的最低安全线：敏感词子串拦截 + 长度校验。
//
// 词库是纯文本、每行一词、# 开头为注释；文件缺失不视为错误（功能降级为不拦截）。
package moderation

import (
	"bufio"
	"fmt"
	"os"
	"strings"
	"sync"
)

// Checker 并发安全的敏感词检查器。
type Checker struct {
	mu      sync.RWMutex
	enabled bool
	words   []string
}

// New 构造检查器；wordFile 为空表示不加载词库。
func New(enabled bool, wordFile string) (*Checker, error) {
	c := &Checker{enabled: enabled}
	if wordFile != "" {
		if err := c.load(wordFile); err != nil {
			return nil, err
		}
	}
	return c, nil
}

func (c *Checker) load(path string) error {
	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil // 词库可选
		}
		return fmt.Errorf("读取敏感词库失败: %w", err)
	}
	defer f.Close()

	var words []string
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for sc.Scan() {
		w := strings.ToLower(strings.TrimSpace(sc.Text()))
		if w == "" || strings.HasPrefix(w, "#") {
			continue
		}
		words = append(words, w)
	}
	if err := sc.Err(); err != nil {
		return fmt.Errorf("扫描敏感词库失败: %w", err)
	}

	c.mu.Lock()
	c.words = words
	c.mu.Unlock()
	return nil
}

// Check 命中敏感词时返回错误（上层映射为 422）。
func (c *Checker) Check(text string) error {
	c.mu.RLock()
	enabled, words := c.enabled, c.words
	c.mu.RUnlock()

	if !enabled || len(words) == 0 {
		return nil
	}
	low := strings.ToLower(text)
	for _, w := range words {
		if strings.Contains(low, w) {
			return fmt.Errorf("内容包含违规词，已拒绝")
		}
	}
	return nil
}

// WordCount 返回已加载词数（供启动日志）。
func (c *Checker) WordCount() int {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return len(c.words)
}
