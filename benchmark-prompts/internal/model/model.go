// Package model 只放领域数据结构与字段校验，不做任何 IO。
package model

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"regexp"
	"strings"
	"unicode/utf8"
)

// 提示词状态机（见 docs/storage.md §8）。
const (
	StatusPending  = "pending"
	StatusApproved = "approved"
	StatusRejected = "rejected"
	StatusFeatured = "featured"
)

const (
	// MaxContentLen 正文最大字符数（按 rune 计）。
	MaxContentLen = 8192
	// MaxTags 单条提示词最多标签数。
	MaxTags = 10
)

var tagRe = regexp.MustCompile(`^[a-z0-9_-]{1,32}$`)

// Prompt 完整提示词。JSON 短名遵循 docs/api.md §2.1。
type Prompt struct {
	ID      string   `json:"id"`
	Content string   `json:"p"`
	Tags    []string `json:"t"`
	Version int64    `json:"v"`
	Status  string   `json:"s"`
	Hash    string   `json:"h"`
}

// PromptSummary 列表/目录条目，不含正文以省带宽（docs/api.md §2.2）。
type PromptSummary struct {
	ID      string   `json:"id"`
	Tags    []string `json:"t"`
	Version int64    `json:"v"`
	Hash    string   `json:"h"`
}

// Meta /v1/meta 载荷。
type Meta struct {
	Total         int64  `json:"total"`
	CatalogHash   string `json:"catalog_hash"`
	SchemaVersion int    `json:"schema_version"`
	ServerTime    int64  `json:"server_time"`
}

// Delta /v1/prompts/delta 载荷。
type Delta struct {
	Changes []*Prompt `json:"changes"`
	Deleted []string  `json:"deleted"`
	Since   string    `json:"since"`
	HasMore bool      `json:"has_more"`
}

// TrimContent 入库前的最小处理：只去首尾空白。
//
// 注意：绝不能折叠正文内部空白/换行——benchmark 提示词常是多行结构，
// 折叠会破坏原始内容。内部空白归一化只用于计算 hash（见 ContentHash）。
func TrimContent(s string) string { return strings.TrimSpace(s) }

// ContentHash 计算正文 hash：先折叠内部连续空白为单空格，再 sha256。
// 折叠仅影响 hash（让"排版不同内容相同"的重复提交可被去重），不影响存储正文。
func ContentHash(content string) string {
	collapsed := strings.Join(strings.Fields(content), " ")
	sum := sha256.Sum256([]byte(collapsed))
	return hex.EncodeToString(sum[:])
}

// ShortHash 取 hash 前 8 位，作为 API 的 h 字段。
func ShortHash(full string) string {
	if len(full) >= 8 {
		return full[:8]
	}
	return full
}

// ValidateContent 校验正文长度。
func ValidateContent(s string, max int) error {
	if utf8.RuneCountInString(s) == 0 {
		return fmt.Errorf("content 不能为空")
	}
	if max > 0 && utf8.RuneCountInString(s) > max {
		return fmt.Errorf("content 超过 %d 字符上限", max)
	}
	return nil
}

// ValidateTags 校验标签集合。
func ValidateTags(tags []string) error {
	if len(tags) > MaxTags {
		return fmt.Errorf("标签最多 %d 个", MaxTags)
	}
	for _, t := range tags {
		if !tagRe.MatchString(t) {
			return fmt.Errorf("非法标签 %q（需匹配 ^[a-z0-9_-]{1,32}$）", t)
		}
	}
	return nil
}

// ParseTags 解析入库的逗号分隔标签。
func ParseTags(s string) []string {
	if s == "" {
		return []string{}
	}
	parts := strings.Split(s, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p != "" {
			out = append(out, p)
		}
	}
	return out
}

// FormatTags 序列化标签用于入库。
func FormatTags(tags []string) string { return strings.Join(tags, ",") }

// IsPublicStatus 只读端点是否可暴露该状态。
func IsPublicStatus(s string) bool {
	return s == StatusApproved || s == StatusFeatured
}
