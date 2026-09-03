package model

import (
	"strings"
	"testing"
)

func TestTrimContentKeepsInternalWhitespace(t *testing.T) {
	got := TrimContent("\n\n  line one\n  line two\t\n")
	if got != "line one\n  line two" {
		t.Fatalf("只应去掉首尾空白，得到 %q", got)
	}
}

func TestContentHashIgnoresLayoutButNotContent(t *testing.T) {
	h1 := ContentHash("hello   world")
	h2 := ContentHash("  hello world \n")
	if h1 != h2 {
		t.Fatalf("排版差异应得到同一 hash（去重依据）：%s vs %s", h1, h2)
	}
	if h1 == ContentHash("hello world!") {
		t.Fatalf("内容不同必须得到不同 hash")
	}
	if len(h1) != 64 {
		t.Fatalf("sha256 hex 长度应为 64，得到 %d", len(h1))
	}
	// hash 计算绝不能改动要存储的正文（多行提示词依赖换行）
	if strings.Contains(ContentHash("a\nb"), "\n") {
		t.Fatalf("hash 输出不应含裸换行")
	}
}

func TestShortHash(t *testing.T) {
	if got := ShortHash("abcdefghij"); got != "abcdefgh" {
		t.Fatalf("应取前 8 位，得到 %q", got)
	}
	if got := ShortHash("abc"); got != "abc" {
		t.Fatalf("不足 8 位应原样返回，得到 %q", got)
	}
	if got := ShortHash(""); got != "" {
		t.Fatalf("空串应仍为空，得到 %q", got)
	}
}

func TestValidateContentCountsRunesNotBytes(t *testing.T) {
	if err := ValidateContent("", 100); err == nil {
		t.Fatalf("空正文必须报错")
	}
	// 5 个汉字 = 15 字节，但只算 5 字符
	if err := ValidateContent("五个汉字呀", 5); err != nil {
		t.Fatalf("按 rune 计数应通过: %v", err)
	}
	if err := ValidateContent("六个汉字呀！", 5); err == nil {
		t.Fatalf("超过上限必须报错")
	}
}

func TestTagsRoundTrip(t *testing.T) {
	if got := ParseTags(""); len(got) != 0 {
		t.Fatalf("空串应解析为空，得到 %v", got)
	}
	got := ParseTags("coding, , reasoning,,")
	if len(got) != 2 || got[0] != "coding" || got[1] != "reasoning" {
		t.Fatalf("应丢弃空项并裁剪空白，得到 %v", got)
	}
	if FormatTags([]string{"a", "b"}) != "a,b" {
		t.Fatalf("序列化不符")
	}
	if FormatTags(ParseTags("a,b,c")) != "a,b,c" {
		t.Fatalf("往返不一致")
	}
}

func TestValidateTags(t *testing.T) {
	if err := ValidateTags([]string{"coding", "llm_reasoning-1"}); err != nil {
		t.Fatalf("合法标签被拒: %v", err)
	}
	for _, bad := range [][]string{
		{"BAD"}, {"含中文"}, {"with space"}, {""}, {"toolongtagtoolongtagtoolongtagtoolong"},
	} {
		if err := ValidateTags(bad); err == nil {
			t.Fatalf("非法标签 %v 应被拒", bad)
		}
	}

	var many []string
	for i := 0; i < MaxTags+1; i++ {
		many = append(many, "t"+string(rune('a'+i)))
	}
	if err := ValidateTags(many); err == nil {
		t.Fatalf("超过 %d 个标签应被拒", MaxTags)
	}
}

func TestIsPublicStatus(t *testing.T) {
	if !IsPublicStatus(StatusApproved) || !IsPublicStatus(StatusFeatured) {
		t.Fatalf("approved/featured 必须对外可见")
	}
	if IsPublicStatus(StatusPending) || IsPublicStatus(StatusRejected) {
		t.Fatalf("pending/rejected 不得对外可见")
	}
}
