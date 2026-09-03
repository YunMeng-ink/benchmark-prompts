package client

import (
	"context"
	"net/http"
	"net/url"
	"strconv"
	"time"
)

// DefaultPageSize 是同步与列表的默认每页条数。
const DefaultPageSize = 100

// maxSyncPages 是单次同步的翻页上限，防止服务端游标异常时无限打转
// —— 在带宽受限的源站上，一个失控循环就是事故。
const maxSyncPages = 2000

// Delta 拉取一页增量变更集。
//
// since 传上次同步到的 catalog_hash；留空或传一个服务端已不认识的 hash 时，
// 服务端会回退为全量分页。cursor 传上一页返回的 Delta.Cursor。
func (c *Client) Delta(ctx context.Context, since string, limit int, cursor string) (*Delta, error) {
	q := url.Values{}
	if since != "" {
		q.Set("since", since)
	}
	if limit > 0 {
		q.Set("limit", strconv.Itoa(limit))
	}
	if cursor != "" {
		q.Set("cursor", cursor)
	}

	resp, err := c.send(ctx, &request{
		method: http.MethodGet, path: "/v1/prompts/delta", query: q, idempotent: true,
	})
	if err != nil {
		return nil, err
	}
	d, err := decodeData[Delta](resp)
	if err != nil {
		return nil, err
	}
	if d.Changes == nil {
		d.Changes = []Prompt{}
	}
	if d.Deleted == nil {
		d.Deleted = []string{}
	}
	if resp.cursor != nil {
		d.Cursor = *resp.cursor
	}
	return d, nil
}

// Sync 把服务端目录增量应用到本地缓存，并把 catalog_hash 推进到最新。
//
// 幂等：可反复调用；无变化时只花一个请求的流量（这正是增量同步存在的意义）。
//
// 关键约束：**翻页期间 since 必须固定为调用前的值**。服务端返回的 Since 是
// "本次结果对应的新 hash"，一旦把它回喂给下一页，服务端会认为客户端已是最新
// 而返回空集，导致翻页漏数据。因此新 hash 只在整轮翻页结束后才落盘。
func (c *Client) Sync(ctx context.Context) (*SyncReport, error) {
	if c.cache == nil {
		return nil, &Error{
			Code:    CodeBadRequest,
			Message: "同步需要本地缓存，但客户端以 NoCache 模式创建",
		}
	}

	base, err := c.cache.CatalogHash(ctx)
	if err != nil {
		return nil, err
	}
	report := &SyncReport{Since: base, FullSync: base == ""}

	cursor := ""
	next := base
	for page := 0; ; page++ {
		if page >= maxSyncPages {
			return nil, &Error{
				Code:    CodeBadResponse,
				Message: "同步翻页次数超限，疑似游标异常，已中止以保护带宽",
			}
		}
		d, err := c.Delta(ctx, base, DefaultPageSize, cursor)
		if err != nil {
			return nil, err
		}
		report.Pages++

		for i := range d.Changes {
			p := d.Changes[i]
			changed, uerr := c.cache.UpsertPrompt(ctx, &p)
			if uerr != nil {
				return nil, uerr
			}
			if changed {
				report.Upserted++
				if len(report.Changed) < 50 { // 只留摘要，避免内存与输出失控
					report.Changed = append(report.Changed, p.ID)
				}
			}
		}
		deleted, derr := c.cache.DeletePrompts(ctx, d.Deleted)
		if derr != nil {
			return nil, derr
		}
		report.Deleted += deleted

		if d.Since != "" {
			next = d.Since
		}
		if !d.HasMore || d.Cursor == "" {
			break
		}
		cursor = d.Cursor
	}

	if next == "" {
		return nil, &Error{Code: CodeBadResponse, Message: "服务端未返回新的 catalog_hash"}
	}
	if err := c.cache.SetCatalogHash(ctx, next); err != nil {
		return nil, err
	}
	report.Since = next
	return report, nil
}

// Cached 只读本地缓存（离线场景）。未命中返回包装了 ErrCacheMiss 的错误。
func (c *Client) Cached(ctx context.Context, id string) (*Prompt, error) {
	if c.cache == nil {
		return nil, &Error{Code: CodeBadRequest, Message: "客户端以 NoCache 模式创建，无本地缓存"}
	}
	return c.cache.GetPrompt(ctx, id)
}

// LocalCount 返回本地缓存条目数，供 CLI 打印同步后状态。
func (c *Client) LocalCount(ctx context.Context) (int, error) {
	if c.cache == nil {
		return 0, nil
	}
	return c.cache.Len(ctx)
}

// Status 汇总本地同步状态，供 `bench meta` 判断"是否落后于服务端"。
type Status struct {
	LocalCount  int       `json:"local_count"`
	CatalogHash string    `json:"catalog_hash"`
	ServerTotal int64     `json:"server_total"`
	ServerHash  string    `json:"server_hash"`
	UpToDate    bool      `json:"up_to_date"`
	LastCheck   time.Time `json:"last_check"`
}

// CheckStatus 拉一次 meta（通常命中 304）并与本地 hash 比较。
func (c *Client) CheckStatus(ctx context.Context) (*Status, error) {
	st := &Status{LastCheck: c.now().UTC()}
	if c.cache != nil {
		n, err := c.cache.Len(ctx)
		if err != nil {
			return nil, err
		}
		st.LocalCount = n
		h, err := c.cache.CatalogHash(ctx)
		if err != nil {
			return nil, err
		}
		st.CatalogHash = h
	}
	m, err := c.Meta(ctx)
	if err != nil {
		return nil, err
	}
	st.ServerTotal = m.Total
	st.ServerHash = m.CatalogHash
	st.UpToDate = st.CatalogHash != "" && st.CatalogHash == m.CatalogHash
	return st, nil
}
