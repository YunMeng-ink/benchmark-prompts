// Package client 是 benchmark 提示词平台的官方 SDK。
//
// 设计约束：
//   - 本包的**生产代码不依赖 internal/**（仅测试文件例外）。它会被 DSH / Pi 插件
//     与第三方脚本直接 import，内部类型一旦泄漏就会把使用者锁死在我们的内部实现上。
//   - 结构体 JSON tag 必须与 docs/api.md §2 完全一致；契约测试负责盯住这一点。
//   - 传输层细节（gzip 解压、连接复用）交给 net/http 默认 Transport，
//     它会自动带上 Accept-Encoding: gzip 并透明解压，无需手工处理。
package client

// Prompt 一条完整提示词（含正文）。字段短名遵循 docs/api.md §2.1。
type Prompt struct {
	ID      string   `json:"id"`
	Content string   `json:"p"`
	Tags    []string `json:"t"`
	Version int64    `json:"v"`
	Status  string   `json:"s"`
	Hash    string   `json:"h"`
}

// PromptSummary 列表条目，不含正文（服务端用它省带宽）。
type PromptSummary struct {
	ID      string   `json:"id"`
	Tags    []string `json:"t"`
	Version int64    `json:"v"`
	Hash    string   `json:"h"`
}

// Meta /v1/meta 载荷。CatalogHash 是增量同步的游标基准。
type Meta struct {
	Total         int64  `json:"total"`
	CatalogHash   string `json:"catalog_hash"`
	SchemaVersion int    `json:"schema_version"`
	ServerTime    int64  `json:"server_time"`
}

// ListPage 一页列表 + 下一页游标。
type ListPage struct {
	Items   []PromptSummary `json:"items"`
	HasMore bool            `json:"has_more"`
	Cursor  string          `json:"-"`
}

// Delta 一次增量变更集。
type Delta struct {
	Changes []Prompt `json:"changes"`
	Deleted []string `json:"deleted"`
	Since   string   `json:"since"`
	HasMore bool     `json:"has_more"`
	Cursor  string   `json:"-"`
}

// ScoreResult 打分后的聚合结果。
type ScoreResult struct {
	Avg   float64 `json:"avg"`
	Count int64   `json:"count"`
}

// UploadResult 上传结果；Status 通常为 pending（待审核）。
type UploadResult struct {
	ID     string `json:"id"`
	Status string `json:"s"`
}

// SyncReport 一次同步的统计，供 CLI 打印。
type SyncReport struct {
	Upserted int      `json:"upserted"`
	Deleted  int      `json:"deleted"`
	Since    string   `json:"since"`
	Pages    int      `json:"pages"`
	FullSync bool     `json:"full_sync"` // true 表示 since 未命中，走了全量回退
	Changed  []string `json:"changed,omitempty"`
}

// Summary 把完整提示词降级为摘要（比较与展示用）。
func (p *Prompt) Summary() PromptSummary {
	return PromptSummary{ID: p.ID, Tags: p.Tags, Version: p.Version, Hash: p.Hash}
}

// Clone 返回深拷贝，避免调用方改到缓存里的切片。
func (p *Prompt) Clone() *Prompt {
	if p == nil {
		return nil
	}
	cp := *p
	cp.Tags = append([]string(nil), p.Tags...)
	return &cp
}
