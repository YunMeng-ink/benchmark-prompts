package moderation

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func wordFile(t *testing.T, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "banned.txt")
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatalf("写词库失败: %v", err)
	}
	return p
}

func TestDisabledNeverBlocks(t *testing.T) {
	c, err := New(false, wordFile(t, "bad\n"))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := c.Check("this contains bad word"); err != nil {
		t.Fatalf("关闭审核时不应拦截: %v", err)
	}
}

func TestBlocksListedWordsCaseInsensitively(t *testing.T) {
	c, err := New(true, wordFile(t, "# 注释行\n\nBad-Word\n另一个词\n"))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if c.WordCount() != 2 {
		t.Fatalf("注释与空行应被跳过，实际加载 %d 词", c.WordCount())
	}
	if err := c.Check("句中夹了 BAD-WORD 大写形式"); err == nil {
		t.Fatalf("大小写不同也应命中")
	}
	if err := c.Check("这里出现另一个词了"); err == nil {
		t.Fatalf("中文词未命中")
	}
	if err := c.Check("完全正常的提示词内容"); err != nil {
		t.Fatalf("正常内容被误拦: %v", err)
	}
}

func TestMissingWordFileDegradesInsteadOfFailing(t *testing.T) {
	c, err := New(true, filepath.Join(t.TempDir(), "absent.txt"))
	if err != nil {
		t.Fatalf("词库缺失应降级为不拦截，得到: %v", err)
	}
	if c.WordCount() != 0 {
		t.Fatalf("缺失时词数应为 0")
	}
	if err := c.Check("anything"); err != nil {
		t.Fatalf("缺失词库时不应拦截: %v", err)
	}
}

func TestEmptyWordFileAllowsEverything(t *testing.T) {
	c, err := New(true, wordFile(t, "\n   \n\n"))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if c.WordCount() != 0 {
		t.Fatalf("空词库词数应为 0")
	}
	if err := c.Check("x"); err != nil {
		t.Fatalf("空词库不应拦截: %v", err)
	}
}

func TestNoWordFileConfigured(t *testing.T) {
	c, err := New(true, "")
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := c.Check("anything"); err != nil {
		t.Fatalf("未配置词库时不应拦截: %v", err)
	}
}

func TestLongLineIsScanned(t *testing.T) {
	needle := "违禁" + strings.Repeat("长", 5000)
	c, err := New(true, wordFile(t, needle+"\n"))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if c.WordCount() != 1 {
		t.Fatalf("长行应被完整加载，得到 %d 词", c.WordCount())
	}
	if err := c.Check("前缀 " + needle + " 后缀"); err == nil {
		t.Fatalf("超长行内容应能命中")
	}
}
