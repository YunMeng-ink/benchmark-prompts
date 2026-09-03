package api

import (
	"encoding/json"
	"errors"
	"io"
	"math"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/example/benchmark-prompts/internal/catalog"
	"github.com/example/benchmark-prompts/internal/model"
	"github.com/example/benchmark-prompts/internal/store"
)

// handleMeta 实现 GET /v1/meta。只读 hash，不写快照（读请求不得改变服务端状态）。
func (s *Server) handleMeta(w http.ResponseWriter, r *http.Request) {
	hash, err := s.cat.Hash(r.Context())
	if err != nil {
		s.renderErr(w, err)
		return
	}
	total, err := s.st.CountApproved(r.Context())
	if err != nil {
		s.renderErr(w, err)
		return
	}

	etag := quoteETag(hash)
	if matchETag(r, etag) {
		s.writeNotModified(w)
		return
	}
	w.Header().Set("ETag", etag)
	w.Header().Set("Cache-Control", "public, max-age=60")

	s.renderOK(w, http.StatusOK, model.Meta{
		Total:         total,
		CatalogHash:   hash,
		SchemaVersion: store.SchemaVersion,
		ServerTime:    time.Now().Unix(),
	}, nil)
}

// handleList 实现 GET /v1/prompts（返回不含正文的摘要）。
func (s *Server) handleList(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	limit, err := queryInt(q.Get("limit"), 20)
	if err != nil {
		s.renderErr(w, ErrBadRequest.WithMessage("limit 必须是整数"))
		return
	}
	limit = clampInt(limit, 1, 100)

	offset, err := catalog.DecodeCursor(q.Get("cursor"))
	if err != nil {
		s.renderErr(w, ErrBadRequest.WithMessage("%v", err))
		return
	}

	rows, err := s.st.ListApproved(r.Context(), q.Get("tag"), limit+1, offset)
	if err != nil {
		s.renderErr(w, err)
		return
	}
	hasMore := len(rows) > limit
	if hasMore {
		rows = rows[:limit]
	}

	var cursor *string
	if hasMore {
		c := catalog.EncodeCursor(offset + limit)
		cursor = &c
	}
	w.Header().Set("Cache-Control", "no-store")
	s.renderOK(w, http.StatusOK, map[string]any{"items": rows, "has_more": hasMore}, cursor)
}

// handleGet 实现 GET /v1/prompts/{id}。
func (s *Server) handleGet(w http.ResponseWriter, r *http.Request) {
	id := strings.TrimSpace(r.PathValue("id"))
	if id == "" {
		s.renderErr(w, ErrBadRequest.WithMessage("缺少 id"))
		return
	}

	p, err := s.st.GetApproved(r.Context(), id)
	if err != nil {
		s.renderErr(w, err)
		return
	}

	etag := quoteETag(id + ":v" + strconv.FormatInt(p.Version, 10))
	if matchETag(r, etag) {
		s.writeNotModified(w)
		return
	}
	w.Header().Set("ETag", etag)
	w.Header().Set("Cache-Control", "public, max-age=60")
	s.renderOK(w, http.StatusOK, p, nil)
}

// handleScoreStats 实现 GET /v1/prompts/{id}/score —— 只读打分统计。
// 前端「查看打分」需要它：avg/count 原本只在 POST /v1/scores 的响应里出现。
// 不设 ETag：分数随每次提交变化，而聚合查询极轻，no-store 更诚实。
func (s *Server) handleScoreStats(w http.ResponseWriter, r *http.Request) {
	id := strings.TrimSpace(r.PathValue("id"))
	// 未公开与不存在同样返 404，不泄露“这条存在但隐藏”这个信息。
	if err := s.st.AssertPublic(r.Context(), id); err != nil {
		s.renderErr(w, ErrNotFound.WithMessage("提示词不存在或未公开"))
		return
	}
	avg, count, err := s.st.ScoreStats(r.Context(), id)
	if err != nil {
		s.renderErr(w, err)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	s.renderOK(w, http.StatusOK, map[string]any{
		"id":    id,
		"avg":   math.Round(avg*100) / 100,
		"count": count,
	}, nil)
}

// handleRandom 实现 GET /v1/prompts/random。必须在 {id} 之前注册，
// 但 Go 1.22+ ServeMux 会让静态段优先于通配段，因此不会被 {id} 吞掉。
func (s *Server) handleRandom(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	var exclude []string
	if e := q.Get("exclude"); e != "" {
		exclude = splitList(e, 100)
	}

	p, err := s.st.RandomApproved(r.Context(), q.Get("tag"), exclude)
	switch {
	case errors.Is(err, store.ErrNotFound):
		s.renderErr(w, ErrNotFound.WithMessage("当前没有可用的提示词"))
		return
	case err != nil:
		s.renderErr(w, err)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	s.renderOK(w, http.StatusOK, p, nil)
}

// handleDelta 实现 GET /v1/prompts/delta。
func (s *Server) handleDelta(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	limit, err := queryInt(q.Get("limit"), 50)
	if err != nil {
		s.renderErr(w, ErrBadRequest.WithMessage("limit 必须是整数"))
		return
	}
	limit = clampInt(limit, 1, 200)

	offset, err := catalog.DecodeCursor(q.Get("cursor"))
	if err != nil {
		s.renderErr(w, ErrBadRequest.WithMessage("%v", err))
		return
	}

	res, err := s.cat.Delta(r.Context(), q.Get("since"), limit, offset)
	if err != nil {
		s.renderErr(w, err)
		return
	}

	var cursor *string
	if res.HasMore {
		c := res.NextCursor
		cursor = &c
	}
	w.Header().Set("Cache-Control", "no-store")
	s.renderOK(w, http.StatusOK, map[string]any{
		"changes":  res.Changes,
		"deleted":  res.Deleted,
		"since":    res.Since,
		"has_more": res.HasMore,
	}, cursor)
}

// handleScore 实现 POST /v1/scores（幂等覆盖）。
func (s *Server) handleScore(w http.ResponseWriter, r *http.Request) {
	var req struct {
		ID       string `json:"id"`
		Value    int    `json:"value"`
		DeviceID string `json:"deviceId"`
	}
	if err := decodeJSON(r, &req); err != nil {
		s.renderErr(w, err)
		return
	}
	if strings.TrimSpace(req.ID) == "" {
		s.renderErr(w, ErrValidation.WithMessage("id 不能为空"))
		return
	}
	if req.Value < 1 || req.Value > 5 {
		s.renderErr(w, ErrValidation.WithMessage("value 必须是 1-5 的整数"))
		return
	}
	if strings.TrimSpace(req.DeviceID) == "" {
		s.renderErr(w, ErrValidation.WithMessage("deviceId 不能为空"))
		return
	}

	avg, count, err := s.st.UpsertScore(r.Context(), strings.TrimSpace(req.ID), req.Value, req.DeviceID)
	switch {
	case errors.Is(err, store.ErrNotFound):
		s.renderErr(w, ErrNotFound.WithMessage("提示词不存在或未公开"))
		return
	case err != nil:
		s.renderErr(w, err)
		return
	}

	s.cache.Delete(promptCacheKey(req.ID))
	w.Header().Set("Cache-Control", "no-store")
	s.renderOK(w, http.StatusOK, map[string]any{
		"avg":   math.Round(avg*100) / 100,
		"count": count,
	}, nil)
}

// handleUpload 实现 POST /v1/prompts（进审核队列）。
func (s *Server) handleUpload(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Content  string   `json:"p"`
		Tags     []string `json:"t"`
		ClientID string   `json:"clientId"`
	}
	if err := decodeJSON(r, &req); err != nil {
		s.renderErr(w, err)
		return
	}

	content := model.TrimContent(req.Content)
	if err := model.ValidateContent(content, s.cfg.Moderation.MaxPromptLen); err != nil {
		s.renderErr(w, ErrValidation.WithMessage("%v", err))
		return
	}
	if err := model.ValidateTags(req.Tags); err != nil {
		s.renderErr(w, ErrValidation.WithMessage("%v", err))
		return
	}
	if err := s.mod.Check(content); err != nil {
		s.renderErr(w, ErrValidation.WithMessage("%v", err))
		return
	}

	res, err := s.st.CreatePendingPrompt(r.Context(), content, req.Tags, req.ClientID)
	if err != nil {
		s.renderErr(w, err)
		return
	}

	// 新建=202（待审核）；命中幂等/去重=200
	status := http.StatusAccepted
	if res.Reused {
		status = http.StatusOK
	}
	w.Header().Set("Cache-Control", "no-store")
	s.renderOK(w, status, map[string]any{"id": res.PromptID, "s": res.Status}, nil)
}

// handleHealthz 供探活使用，不进信封（便于 curl -f 判断）。
func (s *Server) handleHealthz(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = io.WriteString(w, "ok\n")
}

// handleMetrics 输出内部指标快照（非公开契约，需鉴权）。
func (s *Server) handleMetrics(w http.ResponseWriter, r *http.Request) {
	s.writeJSONRaw(w, http.StatusOK, s.metrics.Snapshot())
}

// handlePreflight 响应 CORS 预检；响应头已由 securityHeadersMW 写好。
func (s *Server) handlePreflight(w http.ResponseWriter, r *http.Request) {
	w.WriteHeader(http.StatusNoContent)
}

// ---- 小工具 ----

func (s *Server) writeJSONRaw(w http.ResponseWriter, status int, v any) {
	buf, err := json.Marshal(v)
	if err != nil {
		s.log.Error("序列化指标失败", "err", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_, _ = w.Write(buf)
}

// decodeJSON 解析请求体。优先用 authMW 已读取并放进 context 的字节，
// 因为 HMAC 验签必须先读 body，而 body 只能读一次。
func decodeJSON(r *http.Request, dst any) error {
	body := bodyBytes(r)
	if len(body) == 0 {
		return ErrBadRequest.WithMessage("请求体不能为空")
	}
	if err := json.Unmarshal(body, dst); err != nil {
		return ErrBadRequest.WithMessage("请求体不是合法 JSON: %v", err)
	}
	return nil
}

func queryInt(s string, def int) (int, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return def, nil
	}
	n, err := strconv.Atoi(s)
	if err != nil {
		return 0, err
	}
	return n, nil
}

func clampInt(v, lo, hi int) int {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}

func splitList(s string, max int) []string {
	parts := strings.Split(s, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		out = append(out, p)
		if len(out) >= max {
			break
		}
	}
	return out
}

func promptCacheKey(id string) string { return "prompt:" + id }
