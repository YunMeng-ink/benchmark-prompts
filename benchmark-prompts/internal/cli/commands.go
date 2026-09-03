package cli

import (
	"context"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"

	"github.com/example/benchmark-prompts/internal/buildinfo"
	"github.com/example/benchmark-prompts/pkg/client"
)

func (a *App) cmdVersion(g *globals) error {
	// 字段在 api.md 的 "只增不改不删" 约束内追加：version 仍在，新增 commit/date。
	return a.emit(g, buildinfo.Map(), func() {
		writeLine(a.out, "bench "+buildinfo.String())
	})
}

// cmdMeta 报告服务端与本地缓存的差距。它只花一个请求（通常命中 304）。
func (a *App) cmdMeta(ctx context.Context, g *globals) error {
	c, err := a.clientFor(g)
	if err != nil {
		return err
	}
	// 必须显式 Close：缓存是 SQLite 句柄。CLI 进程短命，靠 OS 回收也能走，
	// 但 Windows 上未关的句柄会让临时目录删不掉 —— 测试因此在 Windows 上偶发失败。
	defer func() { _ = c.Close() }()
	defer c.Close()

	st, err := c.CheckStatus(ctx)
	if err != nil {
		return err
	}
	return a.emit(g, st, func() {
		writeLine(a.out, fmt.Sprintf("服务端  : %d 条公开 / catalog_hash=%s", st.ServerTotal, short(st.ServerHash)))
		writeLine(a.out, fmt.Sprintf("本地缓存 : %d 条 / catalog_hash=%s", st.LocalCount, short(st.CatalogHash)))
		if st.UpToDate {
			writeLine(a.out, "状态     : 已是最新")
		} else {
			writeLine(a.out, "状态     : 落后于服务端，运行 bench sync 增量同步")
		}
	})
}

func (a *App) cmdSync(ctx context.Context, g *globals) error {
	c, err := a.clientFor(g)
	if err != nil {
		return err
	}
	// 必须显式 Close：缓存是 SQLite 句柄。CLI 进程短命，靠 OS 回收也能走，
	// 但 Windows 上未关的句柄会让临时目录删不掉 —— 测试因此在 Windows 上偶发失败。
	defer func() { _ = c.Close() }()
	defer c.Close()

	rep, err := c.Sync(ctx)
	if err != nil {
		return err
	}
	n, err := c.LocalCount(ctx)
	if err != nil {
		return err
	}
	return a.emit(g, map[string]any{"report": rep, "local_count": n}, func() {
		mode := "增量"
		if rep.FullSync {
			mode = "全量（首次或游标过旧）"
		}
		writeLine(a.out, fmt.Sprintf("同步完成(%s)：新增/变更 %d，删除 %d，翻页 %d 次，本地共 %d 条",
			mode, rep.Upserted, rep.Deleted, rep.Pages, n))
		writeLine(a.out, "catalog_hash="+short(rep.Since))
	})
}

// cmdGet 一键测试。--local 时完全不碰网络，因此也不要求已配置 endpoint。
func (a *App) cmdGet(ctx context.Context, g *globals, args []string) error {
	if len(args) != 1 || strings.TrimSpace(args[0]) == "" {
		return fmt.Errorf("用法: bench get <id>")
	}
	id := strings.TrimSpace(args[0])

	if g.local {
		path, err := g.cachePath()
		if err != nil {
			return err
		}
		cc, err := client.OpenCache(path)
		if err != nil {
			return err
		}
		defer cc.Close()

		p, err := cc.GetPrompt(ctx, id)
		if err != nil {
			return fmt.Errorf("本地缓存没有 %s，请先 bench sync（%w）", id, err)
		}
		return a.emitPrompt(g, p)
	}

	c, err := a.clientFor(g)
	if err != nil {
		return err
	}
	// 必须显式 Close：缓存是 SQLite 句柄。CLI 进程短命，靠 OS 回收也能走，
	// 但 Windows 上未关的句柄会让临时目录删不掉 —— 测试因此在 Windows 上偶发失败。
	defer func() { _ = c.Close() }()
	defer c.Close()

	p, err := c.Get(ctx, id)
	if err != nil {
		return err
	}
	return a.emitPrompt(g, p)
}

// cmdRandom 随机测试。--fresh 用本地已有 id 填充 exclude，避免连续抽重。
func (a *App) cmdRandom(ctx context.Context, g *globals, _ []string) error {
	c, err := a.clientFor(g)
	if err != nil {
		return err
	}
	// 必须显式 Close：缓存是 SQLite 句柄。CLI 进程短命，靠 OS 回收也能走，
	// 但 Windows 上未关的句柄会让临时目录删不掉 —— 测试因此在 Windows 上偶发失败。
	defer func() { _ = c.Close() }()
	defer c.Close()

	exclude := splitCSV(g.exclude)
	if g.fresh && c.Cache() != nil {
		// 排除“最近抽过的”而不是“本地已缓存的”：sync 会把整个目录灌进缓存，
		// 排除全部缓存条目等于永远抽不到东西。
		ids, err := c.Cache().RecentIDs(ctx)
		if err != nil {
			return err
		}
		exclude = append(exclude, ids...)
	}
	// 服务端上限 100，超出时保留尾部（最近写入的更可能是刚见过的）
	if len(exclude) > 100 {
		exclude = exclude[len(exclude)-100:]
	}

	p, err := c.Random(ctx, g.tag, exclude)
	if err != nil {
		return err
	}
	return a.emitPrompt(g, p)
}

// maxAllPages 限制 --all 的翻页规模，防止一条命令把源站带宽打满。
const maxAllPages = 50

func (a *App) cmdList(ctx context.Context, g *globals) error {
	c, err := a.clientFor(g)
	if err != nil {
		return err
	}
	// 必须显式 Close：缓存是 SQLite 句柄。CLI 进程短命，靠 OS 回收也能走，
	// 但 Windows 上未关的句柄会让临时目录删不掉 —— 测试因此在 Windows 上偶发失败。
	defer func() { _ = c.Close() }()
	defer c.Close()

	limit := g.limit
	if limit <= 0 {
		limit = 20
	}

	var (
		items  []client.PromptSummary
		cursor string
		pages  int
	)
	for {
		page, err := c.List(ctx, g.tag, limit, cursor)
		if err != nil {
			return err
		}
		items = append(items, page.Items...)
		pages++
		if !g.all || !page.HasMore || page.Cursor == "" {
			break
		}
		if pages >= maxAllPages {
			a.notef(g, "已达 --all 翻页上限 %d 页（%d 条），停止继续拉取以保护带宽\n", maxAllPages, len(items))
			break
		}
		cursor = page.Cursor
	}

	return a.emit(g, map[string]any{"items": items, "count": len(items)}, func() {
		if len(items) == 0 {
			writeLine(a.out, "（无匹配提示词）")
			return
		}
		writeLine(a.out, "ID           V   HASH      TAGS")
		for _, it := range items {
			writeLine(a.out, fmt.Sprintf("%-12s %-3d %-9s %s",
				it.ID, it.Version, it.Hash, strings.Join(it.Tags, ",")))
		}
	})
}

func (a *App) cmdScore(ctx context.Context, g *globals, args []string) error {
	if len(args) != 2 {
		return fmt.Errorf("用法: bench score <id> <1-5>")
	}
	value, err := strconv.Atoi(strings.TrimSpace(args[1]))
	if err != nil {
		return fmt.Errorf("分值必须是 1-5 的整数，得到 %q", args[1])
	}

	c, err := a.clientFor(g)
	if err != nil {
		return err
	}
	// 必须显式 Close：缓存是 SQLite 句柄。CLI 进程短命，靠 OS 回收也能走，
	// 但 Windows 上未关的句柄会让临时目录删不掉 —— 测试因此在 Windows 上偶发失败。
	defer func() { _ = c.Close() }()
	defer c.Close()

	res, err := c.Score(ctx, args[0], value)
	if err != nil {
		return err
	}
	return a.emit(g, res, func() {
		writeLine(a.out, fmt.Sprintf("已记录 %d 分；当前均分 %.2f（%d 人评分）", value, res.Avg, res.Count))
	})
}

// cmdUpload 支持 -c / -f / 管道三种输入，并提示用 --client-id 获得可重放性。
func (a *App) cmdUpload(ctx context.Context, g *globals) error {
	content := g.content
	switch {
	case strings.TrimSpace(content) != "":
	case g.file != "":
		b, err := os.ReadFile(g.file)
		if err != nil {
			return fmt.Errorf("读取文件失败: %w", err)
		}
		content = string(b)
	case stdinHasPipe():
		b, err := io.ReadAll(io.LimitReader(os.Stdin, 1<<20))
		if err != nil {
			return fmt.Errorf("读取标准输入失败: %w", err)
		}
		content = string(b)
	default:
		return fmt.Errorf("正文为空；用 -c 文本、-f 文件，或用管道喂入")
	}

	c, err := a.clientFor(g)
	if err != nil {
		return err
	}
	// 必须显式 Close：缓存是 SQLite 句柄。CLI 进程短命，靠 OS 回收也能走，
	// 但 Windows 上未关的句柄会让临时目录删不掉 —— 测试因此在 Windows 上偶发失败。
	defer func() { _ = c.Close() }()
	defer c.Close()

	tags := splitCSV(g.tags)
	var res *client.UploadResult
	if g.clientID != "" {
		res, err = c.UploadWithClientID(ctx, content, tags, g.clientID)
	} else {
		res, err = c.Upload(ctx, content, tags)
	}
	if err != nil {
		return err
	}
	return a.emit(g, res, func() {
		writeLine(a.out, fmt.Sprintf("已提交 id=%s 状态=%s（审核通过后才公开）", res.ID, res.Status))
		if g.clientID == "" {
			a.notef(g, "提示：加 --client-id 后，超时/断网可安全重放同一份上传而不产生重复条目\n")
		}
	})
}

func (a *App) cmdConfig(ctx context.Context, g *globals, args []string) error {
	sub := "show"
	rest := args
	if len(args) > 0 {
		sub, rest = args[0], args[1:]
	}
	path, err := g.configPath()
	if err != nil {
		return err
	}

	switch sub {
	case "init":
		cfg, err := client.LoadConfig(path)
		if err != nil {
			return err
		}
		if g.endpoint != "" {
			ep, err := client.NormalizeEndpoint(g.endpoint)
			if err != nil {
				return err
			}
			cfg.Endpoint = ep
		}
		if g.key != "" {
			cfg.APIKey = g.key
		}
		if g.secret != "" {
			cfg.Secret = g.secret
		}
		if strings.TrimSpace(cfg.Endpoint) == "" {
			return fmt.Errorf("必须提供 --endpoint <url>")
		}
		if err := cfg.Save(path); err != nil {
			return err
		}
		return a.emit(g, map[string]any{"path": path, "endpoint": cfg.Endpoint, "has_key": cfg.APIKey != ""}, func() {
			writeLine(a.out, "配置已写入 "+path)
			writeLine(a.out, "  endpoint = "+cfg.Endpoint)
			writeLine(a.out, "  api_key  = "+mask(cfg.APIKey))
		})

	case "show":
		cfg, err := client.LoadConfig(path)
		if err != nil {
			return err
		}
		cfg.ApplyEnv()
		return a.emit(g, map[string]any{
			"path":       path,
			"endpoint":   cfg.Endpoint,
			"has_key":    cfg.APIKey != "",
			"has_secret": cfg.Secret != "",
			"device_id":  cfg.DeviceID,
		}, func() {
			writeLine(a.out, "配置文件: "+path)
			writeLine(a.out, "  endpoint  = "+orNone(cfg.Endpoint))
			writeLine(a.out, "  api_key   = "+mask(cfg.APIKey))
			writeLine(a.out, "  secret    = "+mask(cfg.Secret))
			writeLine(a.out, "  device_id = "+orNone(cfg.DeviceID))
		})

	case "set":
		if len(rest) != 2 {
			return fmt.Errorf("用法: bench config set <endpoint|api_key|secret|device_id> <值>")
		}
		cfg, err := client.LoadConfig(path)
		if err != nil {
			return err
		}
		switch rest[0] {
		case "endpoint":
			ep, err := client.NormalizeEndpoint(rest[1])
			if err != nil {
				return err
			}
			cfg.Endpoint = ep
		case "api_key":
			cfg.APIKey = rest[1]
		case "secret":
			cfg.Secret = rest[1]
		case "device_id":
			cfg.DeviceID = rest[1]
		default:
			return fmt.Errorf("未知配置项 %q", rest[0])
		}
		if err := cfg.Save(path); err != nil {
			return err
		}
		return a.emit(g, map[string]any{"path": path, "key": rest[0]}, func() {
			writeLine(a.out, "已更新 "+rest[0])
		})

	default:
		return fmt.Errorf("未知 config 子命令 %q（可用：init / show / set）", sub)
	}
}

func (a *App) cmdReset(ctx context.Context, g *globals) error {
	path, err := g.cachePath()
	if err != nil {
		return err
	}
	cc, err := client.OpenCache(path)
	if err != nil {
		return err
	}
	defer cc.Close()

	before, err := cc.Len(ctx)
	if err != nil {
		return err
	}
	if err := cc.Purge(ctx); err != nil {
		return err
	}
	return a.emit(g, map[string]any{"path": path, "removed": before}, func() {
		writeLine(a.out, fmt.Sprintf("已清空 %s（%d 条），device_id 已保留", path, before))
	})
}

// emitPrompt 输出提示词。
//
// 正文走 stdout、元信息走 stderr：这样 `bench get p_x | llm-cli` 这类管道
// 不会被 id/tags 污染，而人眼看仍能看到来源。
func (a *App) emitPrompt(g *globals, p *client.Prompt) error {
	return a.emit(g, p, func() {
		a.notef(g, "# %s  v%d  tags=%s  hash=%s\n", p.ID, p.Version, strings.Join(p.Tags, ","), p.Hash)
		writeLine(a.out, p.Content)
	})
}

// ---- 小工具 ----

func short(h string) string {
	if len(h) > 12 {
		return h[:12] + "…"
	}
	if h == "" {
		return "(空)"
	}
	return h
}

func splitCSV(s string) []string {
	if strings.TrimSpace(s) == "" {
		return nil
	}
	parts := strings.Split(s, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// mask 只露出凭据首尾各 2 字符，避免 key 被复制进日志。
func mask(s string) string {
	if s == "" {
		return "(未设置)"
	}
	r := []rune(s)
	if len(r) <= 4 {
		return "****"
	}
	return string(r[:2]) + "…" + string(r[len(r)-2:]) + fmt.Sprintf(" (共%d字符)", len(r))
}

func orNone(s string) string {
	if s == "" {
		return "(未设置)"
	}
	return s
}

// stdinHasPipe 判断标准输入是否是管道/文件而非常量设备。
func stdinHasPipe() bool {
	st, err := os.Stdin.Stat()
	if err != nil {
		return false
	}
	return st.Mode()&os.ModeCharDevice == 0
}
