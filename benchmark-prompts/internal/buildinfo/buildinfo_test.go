package buildinfo

import (
	"strings"
	"testing"
)

// Map 的键是对外契约的一部分（bench version --json 会被插件与运维脚本解析），
// 这里钉住键名，防止哪天改名把契约一起改掉。
func TestMapKeysAreStable(t *testing.T) {
	oldV, oldC, oldD := Version, Commit, Date
	defer func() { Version, Commit, Date = oldV, oldC, oldD }()

	Version, Commit, Date = "v9.9.9", "abc1234", "2026-01-02T03:04:05Z"

	m := Map()
	if got := m["version"]; got != "v9.9.9" {
		t.Errorf("version 键=%v，期望 v9.9.9", got)
	}
	if got := m["commit"]; got != "abc1234" {
		t.Errorf("commit 键=%v，期望 abc1234", got)
	}
	if got := m["date"]; got != "2026-01-02T03:04:05Z" {
		t.Errorf("date 键=%v", got)
	}

	// Date 为空时必须整个键缺席，而不是输出空串——解析方靠"有没有 date"
	// 区分"正式发布构建"与"go run 直跑"。
	Date = ""
	if _, ok := Map()["date"]; ok {
		t.Error("Date 为空时不应输出 date 键")
	}
}

func TestStringIncludesAllParts(t *testing.T) {
	oldV, oldC, oldD := Version, Commit, Date
	defer func() { Version, Commit, Date = oldV, oldC, oldD }()

	Version, Commit, Date = "v1.2.3", "deadbeef", "2026-05-06T07:08:09Z"
	s := String()
	for _, want := range []string{"v1.2.3", "deadbeef", "2026-05-06T07:08:09Z"} {
		if !strings.Contains(s, want) {
			t.Errorf("String()=%q 缺少 %q", s, want)
		}
	}
}
