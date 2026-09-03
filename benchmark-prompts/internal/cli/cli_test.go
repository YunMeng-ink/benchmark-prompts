package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/example/benchmark-prompts/internal/api"
	"github.com/example/benchmark-prompts/internal/auth"
	"github.com/example/benchmark-prompts/internal/cache"
	"github.com/example/benchmark-prompts/internal/config"
	"github.com/example/benchmark-prompts/internal/metrics"
	"github.com/example/benchmark-prompts/internal/model"
	"github.com/example/benchmark-prompts/internal/moderation"
	"github.com/example/benchmark-prompts/internal/ratelimit"
	"github.com/example/benchmark-prompts/internal/store"
)

// harness 把 CLI 接到一个**真实运行**的源站上，并把它自己的配置/缓存
// 隔离到临时目录，因此测试之间完全互不影响。
type harness struct {
	t      *testing.T
	ts     *httptest.Server
	st     *store.Store
	master []byte
	home   string
}

func newHarness(t *testing.T, seedKey bool) *harness {
	t.Helper()

	cfg := config.Default()
	cfg.Bandwidth.WatchEnabled = false
	for k := range cfg.RateLimit.Anonymous {
		cfg.RateLimit.Anonymous[k] = 1 << 20
	}
	for k := range cfg.RateLimit.Authed {
		cfg.RateLimit.Authed[k] = 1 << 20
	}

	db, err := store.Open(filepath.Join(t.TempDir(), "api.db"))
	if err != nil {
		t.Fatalf("打开源站库失败: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := db.Migrate(context.Background()); err != nil {
		t.Fatalf("迁移失败: %v", err)
	}

	master := make([]byte, 32)
	for i := range master {
		master[i] = byte(i + 5)
	}
	mod, err := moderation.New(false, "")
	if err != nil {
		t.Fatalf("moderation: %v", err)
	}

	s := api.New(api.Deps{
		Config: cfg,
		Store:  db,
		Cache:  cache.New(64),
		Auth:   auth.New(db, master, 5*time.Minute),
		Limiter: ratelimit.New(time.Minute, map[string]map[string]int{
			ratelimit.TierAnonymous: cfg.RateLimit.Anonymous,
			ratelimit.TierAuthed:    cfg.RateLimit.Authed,
		}),
		Metrics: &metrics.Registry{},
		Mod:     mod,
		Logger:  slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	ts := httptest.NewServer(s.Routes())
	t.Cleanup(ts.Close)

	h := &harness{t: t, ts: ts, st: db, master: master, home: t.TempDir()}
	if seedKey {
		if err := db.PutAPIKey(context.Background(), "cli-key", "cli", "cli-secret", master); err != nil {
			t.Fatalf("登记 key 失败: %v", err)
		}
	}
	return h
}

// publish 直接落库并过审。
func (h *harness) publish(content string, tags []string) string {
	h.t.Helper()
	res, err := h.st.CreatePendingPrompt(context.Background(), content, tags, "")
	if err != nil {
		h.t.Fatalf("seed 失败: %v", err)
	}
	if err := h.st.SetStatus(context.Background(), res.PromptID, model.StatusApproved); err != nil {
		h.t.Fatalf("过审失败: %v", err)
	}
	return res.PromptID
}

// run 执行一条命令。故意把全局参数放在最后，以覆盖"位置参数在前"的解析路径。
func (h *harness) run(args ...string) (code int, stdout, stderr string) {
	h.t.Helper()
	var out, errOut bytes.Buffer
	app := New(&out, &errOut)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	full := append([]string{}, args...)
	full = append(full, "--home", h.home, "--timeout", "25s")
	code = app.Run(ctx, full)
	return code, out.String(), errOut.String()
}

func (h *harness) configured(extra ...string) *harness {
	if code, out, errOut := h.run(append([]string{"config", "init", "--endpoint", h.ts.URL}, extra...)...); code != 0 {
		h.t.Fatalf("config init 失败 code=%d out=%s err=%s", code, out, errOut)
	}
	return h
}

// jsonOf 断言 `--json` 的成功输出是统一信封，并返回里面的 data。
//
// 先校信封再取数据：这样“输出形状”本身就是被测试的契约，
// 而不是碰巧能 grep 到字段。
func jsonOf(t *testing.T, s string) map[string]any {
	t.Helper()

	raw := map[string]json.RawMessage{}
	if err := json.Unmarshal([]byte(strings.TrimSpace(s)), &raw); err != nil {
		t.Fatalf("输出不是单个 JSON 对象: %v\n%s", err, s)
	}
	for _, k := range []string{"ok", "data", "error", "v"} {
		if _, has := raw[k]; !has {
			t.Fatalf("--json 信封缺少字段 %q（与服务端同形）：%s", k, s)
		}
	}
	var okFlag bool
	var v int
	if err := json.Unmarshal(raw["ok"], &okFlag); err != nil || !okFlag {
		t.Fatalf("成功输出应为 ok:true：%s", s)
	}
	if err := json.Unmarshal(raw["v"], &v); err != nil || v != 1 {
		t.Fatalf("信封 v 应为 1，得到 %d：%s", v, s)
	}
	if string(raw["error"]) != "null" {
		t.Fatalf("成功时 error 必须为 null，得到 %s", raw["error"])
	}

	var m map[string]any
	if err := json.Unmarshal(raw["data"], &m); err != nil {
		// data 可能是数组或标量，统一包到 _raw 便于断言
		return map[string]any{"_raw": string(raw["data"])}
	}
	return m
}

// jsonErrOf 断言失败信封并返回 error 对象。
func jsonErrOf(t *testing.T, s string) map[string]any {
	t.Helper()

	var env struct {
		OK    bool `json:"ok"`
		Data  any  `json:"data"`
		Error *struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
		V int `json:"v"`
	}
	if err := json.Unmarshal([]byte(strings.TrimSpace(s)), &env); err != nil {
		t.Fatalf("错误输出不是 JSON: %v\n%s", err, s)
	}
	if env.OK {
		t.Fatalf("失败输出的 ok 必须为 false：%s", s)
	}
	if env.Data != nil {
		t.Fatalf("失败时 data 应为 null：%s", s)
	}
	if env.Error == nil || env.Error.Code == "" {
		t.Fatalf("失败输出必须带 error.code 供插件分支：%s", s)
	}
	return map[string]any{"code": env.Error.Code, "message": env.Error.Message}
}

// ---- 参数解析 ----

func TestSplitArgs(t *testing.T) {
	cases := []struct {
		in    []string
		flags []string
		pos   []string
	}{
		{[]string{"init", "--home", "/tmp/a"}, []string{"--home", "/tmp/a"}, []string{"init"}},
		{[]string{"--json", "get", "p_1"}, []string{"--json"}, []string{"get", "p_1"}},
		{[]string{"p_1", "--limit=5"}, []string{"--limit=5"}, []string{"p_1"}},
		{[]string{"--quiet", "--all", "x"}, []string{"--quiet", "--all"}, []string{"x"}},
		{[]string{"--", "--not-a-flag"}, []string{}, []string{"--not-a-flag"}},
		{[]string{"-c"}, []string{"-c"}, []string{}}, // 缺值交给 flag 包报错
		{[]string{}, nil, nil},
	}
	for _, c := range cases {
		f, p := splitArgs(c.in)
		if strings.Join(f, "|") != strings.Join(c.flags, "|") {
			t.Fatalf("%v：flag 分组错，期望 %v 得到 %v", c.in, c.flags, f)
		}
		if strings.Join(p, "|") != strings.Join(c.pos, "|") {
			t.Fatalf("%v：位置参数分组错，期望 %v 得到 %v", c.in, c.pos, p)
		}
	}
}

// TestFlagsAfterPositionalStillWork 是关键回归：flag 包原生会**静默丢弃**
// 位置参数之后的选项，若不修，--home 失效会让测试互相污染。
func TestFlagsAfterPositionalStillWork(t *testing.T) {
	h := newHarness(t, false)
	code, out, errOut := h.run("config", "init", "--endpoint", h.ts.URL)
	if code != 0 {
		t.Fatalf("code=%d out=%s err=%s", code, out, errOut)
	}
	if _, err := os.Stat(filepath.Join(h.home, "config")); err != nil {
		t.Fatalf("--home 放在位置参数之后仍须生效，配置文件不存在: %v", err)
	}
}

// ---- 用法与错误路径 ----

func TestUsageAndUnknownCommand(t *testing.T) {
	var out, errOut bytes.Buffer
	app := New(&out, &errOut)
	ctx := context.Background()

	if code := app.Run(ctx, nil); code != 5 {
		t.Fatalf("无参数应以 5 退出，得到 %d", code)
	}
	if !strings.Contains(errOut.String(), "用法") {
		t.Fatalf("stderr 应打印用法")
	}

	out.Reset()
	errOut.Reset()
	if code := app.Run(ctx, []string{"help"}); code != 0 {
		t.Fatalf("help 应 0 退出，得到 %d", code)
	}
	if !strings.Contains(out.String(), "退出码") {
		t.Fatalf("help 应说明退出码约定")
	}

	out.Reset()
	errOut.Reset()
	if code := app.Run(ctx, []string{"frobnicate"}); code != 5 {
		t.Fatalf("未知命令应 5，得到 %d", code)
	}
	if !strings.Contains(errOut.String(), "未知命令") {
		t.Fatalf("应提示未知命令")
	}

	out.Reset()
	errOut.Reset()
	if code := app.Run(ctx, []string{"version", "--nope"}); code != 5 {
		t.Fatalf("未知参数应 5，得到 %d", code)
	}
}

func TestVersion(t *testing.T) {
	h := newHarness(t, false)
	if code, out, _ := h.run("version"); code != 0 || !strings.Contains(out, "bench") {
		t.Fatalf("version 输出不符: code=%d out=%s", code, out)
	}
	code, out, _ := h.run("version", "--json")
	if code != 0 || jsonOf(t, out)["version"] == nil {
		t.Fatalf("--json version 不符: code=%d out=%s", code, out)
	}
}

func TestConfigRequiresEndpoint(t *testing.T) {
	h := newHarness(t, false)
	code, _, errOut := h.run("config", "init")
	if code != 5 {
		t.Fatalf("缺 endpoint 应失败，得到 %d", code)
	}
	if !strings.Contains(errOut, "--endpoint") {
		t.Fatalf("应明确提示缺 --endpoint，得到 %q", errOut)
	}
}

func TestConfigShowMasksCredentials(t *testing.T) {
	h := newHarness(t, false).configured("--key", "abcdef123456")

	code, out, errOut := h.run("config", "show")
	if code != 0 {
		t.Fatalf("code=%d err=%s", code, errOut)
	}
	if strings.Contains(out, "abcdef123456") {
		t.Fatalf("show 不得明文回显 API Key，得到:\n%s", out)
	}
	if !strings.Contains(out, "ab…56") {
		t.Fatalf("应以掩码显示凭据，得到:\n%s", out)
	}

	code, out, _ = h.run("config", "show", "--json")
	if code != 0 {
		t.Fatalf("code=%d", code)
	}
	m := jsonOf(t, out)
	if m["has_key"] != true {
		t.Fatalf("--json 只应暴露有无，不得回显真值: %v", m)
	}
	if _, leaked := m["api_key"]; leaked {
		t.Fatalf("--json 不得包含 api_key 原值")
	}
	if _, leaked := m["secret"]; leaked {
		t.Fatalf("--json 不得包含 secret 原值")
	}

	// set 子命令
	if code, _, e := h.run("config", "set", "endpoint", "https://other.example"); code != 0 {
		t.Fatalf("config set 失败: %d %s", code, e)
	}
	if code, out, _ := h.run("config", "show"); code != 0 || !strings.Contains(out, "other.example") {
		t.Fatalf("set 后应能读到新值: %d %s", code, out)
	}
	if code, _, _ := h.run("config", "set", "bogus", "x"); code != 5 {
		t.Fatalf("未知配置项应 5")
	}
	if code, _, _ := h.run("config", "wat"); code != 5 {
		t.Fatalf("未知 config 子命令应 5")
	}
}

// ---- 主流程 ----

func TestFullWorkflow(t *testing.T) {
	h := newHarness(t, true).configured("--key", "cli-key", "--secret", "cli-secret")

	body := "请用一句话解释什么是 LRU 缓存，并给出时间复杂度。"
	id := h.publish(body, []string{"coding"})

	// sync
	code, out, errOut := h.run("sync")
	if code != 0 {
		t.Fatalf("sync 失败 code=%d out=%s err=%s", code, out, errOut)
	}
	if !strings.Contains(out, "同步完成") || !strings.Contains(out, "1 条") {
		t.Fatalf("sync 输出不符: %s", out)
	}

	// get：正文走 stdout，元信息走 stderr，便于管道
	code, out, errOut = h.run("get", id)
	if code != 0 {
		t.Fatalf("get 失败 code=%d err=%s", code, errOut)
	}
	if strings.TrimRight(out, "\n") != body {
		t.Fatalf("stdout 必须只有正文，得到 %q", out)
	}
	if !strings.Contains(errOut, id) {
		t.Fatalf("元信息应走 stderr，得到 %q", errOut)
	}

	// get --json
	code, out, errOut = h.run("get", id, "--json")
	if code != 0 {
		t.Fatalf("get --json 失败: %d %s", code, errOut)
	}
	m := jsonOf(t, out)
	if m["id"] != id || m["p"] != body {
		t.Fatalf("JSON 字段名必须与契约一致，得到 %v", m)
	}

	// random
	code, out, errOut = h.run("random", "--json")
	if code != 0 {
		t.Fatalf("random 失败: %d %s", code, errOut)
	}
	if jsonOf(t, out)["id"] != id {
		t.Fatalf("只有 1 条时随机必然抽到它: %s", out)
	}

	// score
	code, out, errOut = h.run("score", id, "5", "--json")
	if code != 0 {
		t.Fatalf("score 失败: %d %s", code, errOut)
	}
	sm := jsonOf(t, out)
	if sm["count"] != float64(1) || sm["avg"] != float64(5) {
		t.Fatalf("打分结果不符: %v", sm)
	}

	// upload
	code, out, errOut = h.run("upload", "-c", "新写的一条提示词正文内容", "-t", "coding,reasoning", "--client-id", "cli-cid-1", "--json")
	if code != 0 {
		t.Fatalf("upload 失败: %d %s", code, errOut)
	}
	um := jsonOf(t, out)
	if um["s"] != model.StatusPending || um["id"] == "" {
		t.Fatalf("上传应返回 pending + id，得到 %v", um)
	}

	// 幂等重放
	code, out, _ = h.run("upload", "-c", "另一个正文但不该新建", "--client-id", "cli-cid-1", "--json")
	if code != 0 {
		t.Fatalf("重放应成功: %d %s", code, out)
	}
	if jsonOf(t, out)["id"] != um["id"] {
		t.Fatalf("同 clientId 必须幂等，得到 %s", out)
	}

	// 待审核内容不可见
	if code, out, _ := h.run("get", um["id"].(string)); code != 4 {
		t.Fatalf("取未过审内容应 404（退出码 4），得到 %d %s", code, out)
	}

	// list
	code, out, errOut = h.run("list", "--json")
	if code != 0 {
		t.Fatalf("list 失败: %d %s", code, errOut)
	}
	lm := jsonOf(t, out)
	if lm["count"] != float64(1) {
		t.Fatalf("列表应只含 1 条公开内容，得到 %v", lm)
	}
	items, _ := lm["items"].([]any)
	if len(items) != 1 {
		t.Fatalf("items 长度不符: %v", lm)
	}
	if _, hasBody := items[0].(map[string]any)["p"]; hasBody {
		t.Fatalf("list 不得返回正文")
	}

	// meta
	code, out, errOut = h.run("meta", "--json")
	if code != 0 {
		t.Fatalf("meta 失败: %d %s", code, errOut)
	}
	mm := jsonOf(t, out)
	if mm["up_to_date"] != true {
		t.Fatalf("同步后应判定为已最新: %v", mm)
	}

	// reset
	code, out, errOut = h.run("reset")
	if code != 0 || !strings.Contains(out, "已清空") {
		t.Fatalf("reset 失败: code=%d out=%s err=%s", code, out, errOut)
	}
	if code, out, _ := h.run("meta", "--json"); code != 0 {
		t.Fatalf("reset 后 meta 仍可运行: %d %s", code, out)
	} else if jsonOf(t, out)["up_to_date"] != false {
		t.Fatalf("清空缓存后不应再判定为最新")
	}
}

// TestOfflineGet 验证离线能力：同步过一次之后，--local 不依赖网络。
func TestOfflineGet(t *testing.T) {
	h := newHarness(t, false).configured()
	body := "离线可读的提示词正文"
	id := h.publish(body, nil)

	if code, out, errOut := h.run("sync"); code != 0 {
		t.Fatalf("sync 失败: %d %s %s", code, out, errOut)
	}

	h.ts.Close() // 源站下线

	if code, out, _ := h.run("get", id, "--local"); code != 0 || strings.TrimRight(out, "\n") != body {
		t.Fatalf("--local 应在无网络时成功，得到 code=%d out=%q", code, out)
	}
	code, _, errOut := h.run("get", id)
	if code != 1 {
		t.Fatalf("源站下线后普通 get 应以 1（网络）退出，得到 %d %s", code, errOut)
	}
}

func TestErrorExitCodesAndJSONShape(t *testing.T) {
	t.Run("未登记凭据的写请求", func(t *testing.T) {
		h := newHarness(t, false).configured()
		id := h.publish("auth failure target", nil)

		code, _, errOut := h.run("score", id, "5")
		if code != 3 {
			t.Fatalf("鉴权失败应 3，得到 %d %s", code, errOut)
		}
		code, out, _ := h.run("score", id, "5", "--json")
		if code != 3 {
			t.Fatalf("--json 也应保持退出码，得到 %d", code)
		}
		errObj := jsonErrOf(t, out)
		if errObj["code"] != "unauthorized" {
			t.Fatalf("错误码应为 unauthorized，得到 %v", errObj)
		}
	})

	t.Run("参数与校验错误", func(t *testing.T) {
		h := newHarness(t, true).configured("--key", "cli-key")
		id := h.publish("validation target", nil)

		if code, _, _ := h.run("score"); code != 5 {
			t.Fatalf("缺参数应 5，得到 %d", code)
		}
		if code, _, _ := h.run("score", id, "abc"); code != 5 {
			t.Fatalf("非数字分值应 5，得到 %d", code)
		}
		if code, _, _ := h.run("score", id, "9"); code != 5 {
			t.Fatalf("越界分值应 5，得到 %d", code)
		}
		if code, _, _ := h.run("get"); code != 5 {
			t.Fatalf("get 缺 id 应 5")
		}
		if code, _, _ := h.run("get", "p_deadbeef"); code != 4 {
			t.Fatalf("不存在应 4，得到 %d", code)
		}
		if code, _, _ := h.run("upload"); code != 5 {
			t.Fatalf("空正文应 5（且不会误发网络请求）")
		}
	})

	t.Run("未配置服务地址", func(t *testing.T) {
		h := newHarness(t, false)
		code, _, errOut := h.run("meta")
		if code != 5 {
			t.Fatalf("未配置 endpoint 应 5，得到 %d", code)
		}
		if !strings.Contains(errOut, "config init") {
			t.Fatalf("错误信息必须告诉用户下一步做什么，得到 %q", errOut)
		}
	})
}

// TestRandomFreshOnlyExcludesRecentlyDrawn 钉住 --fresh 的正确语义。
//
// 曾经的实现排除“全部本地缓存 id”，而 bench sync 会把整个目录灌进缓存，
// 导致 --fresh 几乎必然 404——一个完全无法用的默认行为。
func TestRandomFreshOnlyExcludesRecentlyDrawn(t *testing.T) {
	h := newHarness(t, false).configured()
	only := h.publish("the only prompt", nil)

	if code, out, errOut := h.run("sync"); code != 0 {
		t.Fatalf("sync 失败: %d %s %s", code, out, errOut)
	}

	// 刚同步过、但从未抽过：--fresh 必须仍能抽到
	code, out, errOut := h.run("random", "--fresh", "--json")
	if code != 0 || jsonOf(t, out)["id"] != only {
		t.Fatalf("--fresh 不得排除整个已同步目录: code=%d out=%s err=%s", code, out, errOut)
	}

	// 抽过一次后，--fresh 才应报“没有新东西”
	if code, _, _ := h.run("random", "--json"); code != 0 {
		t.Fatalf("普通 random 应成功")
	}
	if code, _, _ := h.run("random", "--fresh"); code != 4 {
		t.Fatalf("已抽过唯一条目后 --fresh 应退出码 4，得到 %d", code)
	}

	// 但去掉 --fresh 仍可用，--fresh 不得把缓存弄坏
	if code, out, _ := h.run("random", "--json"); code != 0 || jsonOf(t, out)["id"] != only {
		t.Fatalf("--fresh 不影响普通抽取: code=%d out=%s", code, out)
	}
}

func TestUploadFromFile(t *testing.T) {
	h := newHarness(t, true).configured("--key", "cli-key")
	src := filepath.Join(t.TempDir(), "p.md")
	if err := os.WriteFile(src, []byte("从文件读入的提示词正文，含换行。\n第二行。"), 0o600); err != nil {
		t.Fatalf("写文件失败: %v", err)
	}

	code, out, errOut := h.run("upload", "-f", src, "--json")
	if code != 0 {
		t.Fatalf("从文件上传失败: code=%d err=%s", code, errOut)
	}
	id := jsonOf(t, out)["id"].(string)
	if id == "" {
		t.Fatalf("未返回 id: %s", out)
	}

	if code, _, errOut := h.run("upload", "-f", filepath.Join(t.TempDir(), "absent.md")); code != 5 {
		t.Fatalf("文件不存在应 5，得到 %d %s", code, errOut)
	}
}

func TestListAllFollowsPages(t *testing.T) {
	h := newHarness(t, false).configured()
	for i := 0; i < 3; i++ {
		h.publish("page content "+uniqueMarker(i), []string{"bulk"})
	}

	code, out, errOut := h.run("list", "--limit", "2", "--all", "--json")
	if code != 0 {
		t.Fatalf("list --all 失败: %d %s", code, errOut)
	}
	m := jsonOf(t, out)
	items, _ := m["items"].([]any)
	if len(items) != 3 {
		t.Fatalf("--all 应翻完所有页得到 3 条，实际 %d：%v", len(items), m)
	}

	// 不带 --all 时只取一页
	code, out, _ = h.run("list", "--limit", "2", "--json")
	if code != 0 {
		t.Fatalf("单页 list 失败: %d", code)
	}
	if items2, _ := jsonOf(t, out)["items"].([]any); len(items2) != 2 {
		t.Fatalf("单页应正好 2 条，得到 %d", len(items2))
	}
}

func uniqueMarker(i int) string { return string(rune('A' + i)) }
