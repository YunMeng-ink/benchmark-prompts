// Command bench-server 是源站 API 服务（对应 docs/server.md）。
//
// 运行：
//
//	bench-server -config /etc/bench/config.yaml
//
// 维护子命令：
//
//	bench-server -backup /backup/bench.db      # 一次性一致性备份（纯 Go 驱动，不依赖 sqlite3 CLI）
//	bench-server -put-key "alice:key:secret"   # 登记 API Key
package main

import (
	"context"
	"crypto/rand"
	"crypto/tls"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/example/benchmark-prompts/internal/api"
	"github.com/example/benchmark-prompts/internal/auth"
	"github.com/example/benchmark-prompts/internal/buildinfo"
	"github.com/example/benchmark-prompts/internal/cache"
	"github.com/example/benchmark-prompts/internal/config"
	"github.com/example/benchmark-prompts/internal/metrics"
	"github.com/example/benchmark-prompts/internal/model"
	"github.com/example/benchmark-prompts/internal/moderation"
	"github.com/example/benchmark-prompts/internal/ratelimit"
	"github.com/example/benchmark-prompts/internal/store"
)

const snapshotKeepSeconds = 30 * 24 * 3600 // 目录快照保留 30 天

func main() {
	var (
		cfgPath  = flag.String("config", "", "YAML 配置路径（留空则使用内置默认值）")
		backupTo = flag.String("backup", "", "执行一次 VACUUM INTO 备份后退出")
		putKey   = flag.String("put-key", "", "登记 API Key，格式 name:plainKey:plainSecret，然后退出")
		review   = flag.Bool("review", false, "列出待审核（pending）提示词后退出")
		approve  = flag.String("approve", "", "把指定 id 审核通过为 approved 后退出")
		reject   = flag.String("reject", "", "把指定 id 审核打回为 rejected 后退出")
		devMode  = flag.Bool("dev", false, "开发模式：明文 HTTP，文本日志")
		logLevel = flag.String("log-level", "info", "日志级别 debug|info|warn|error")
		showVer  = flag.Bool("version", false, "打印构建版本后退出")
	)
	flag.Parse()

	if *showVer {
		// 运维需要能在不起服务、不读配置的情况下确认“这台机器上装的是哪一版”。
		_, _ = fmt.Fprintln(os.Stdout, "bench-server "+buildinfo.String())
		return
	}

	logger := newLogger(*logLevel, *devMode)
	logger.Info("启动", "version", buildinfo.Version, "commit", buildinfo.Commit, "built", buildinfo.Date)

	cfg, err := config.Load(*cfgPath)
	if err != nil {
		logger.Error("加载配置失败", "err", err)
		os.Exit(1)
	}

	st, err := store.Open(cfg.Store.Path)
	if err != nil {
		logger.Error("打开数据库失败", "err", err, "path", cfg.Store.Path)
		os.Exit(1)
	}
	defer st.Close()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if cfg.Store.Migrate {
		if err := st.Migrate(ctx); err != nil {
			logger.Error("数据库迁移失败", "err", err)
			os.Exit(1)
		}
	}

	// 一次性维护任务：先于 HTTP 服务处理
	switch {
	case *backupTo != "":
		if err := st.Backup(ctx, *backupTo); err != nil {
			logger.Error("备份失败", "err", err)
			os.Exit(1)
		}
		logger.Info("备份完成", "dest", *backupTo)
		return
	case *putKey != "":
		if err := runPutKey(ctx, st, *putKey, logger); err != nil {
			logger.Error("登记 API Key 失败", "err", err)
			os.Exit(1)
		}
		return
	case *review:
		ps, err := st.ListByStatus(ctx, model.StatusPending, 50)
		if err != nil {
			logger.Error("读取待审核队列失败", "err", err)
			os.Exit(1)
		}
		fmt.Printf("待审核 %d 条：\n", len(ps))
		for _, p := range ps {
			fmt.Printf("  %s\t%s\n", p.ID, preview(p.Content))
		}
		return
	case *approve != "":
		if err := st.SetStatus(ctx, *approve, model.StatusApproved); err != nil {
			logger.Error("审核通过失败", "err", err, "id", *approve)
			os.Exit(1)
		}
		logger.Info("已通过审核", "id", *approve)
		return
	case *reject != "":
		if err := st.SetStatus(ctx, *reject, model.StatusRejected); err != nil {
			logger.Error("审核打回失败", "err", err, "id", *reject)
			os.Exit(1)
		}
		logger.Info("已打回", "id", *reject)
		return
	}

	masterKey, err := loadMasterKey(*devMode, logger)
	if err != nil {
		logger.Error("初始化鉴权主密钥失败", "err", err)
		os.Exit(1)
	}

	limiter := ratelimit.New(cfg.RateLimit.Window.D(), map[string]map[string]int{
		ratelimit.TierAnonymous: cfg.RateLimit.Anonymous,
		ratelimit.TierAuthed:    cfg.RateLimit.Authed,
	})
	mod, err := moderation.New(cfg.Moderation.Enabled, cfg.Moderation.BannedWordsFile)
	if err != nil {
		logger.Error("初始化审核模块失败", "err", err)
		os.Exit(1)
	}
	reg := &metrics.Registry{}

	srv := api.New(api.Deps{
		Config:  cfg,
		Store:   st,
		Cache:   cache.New(2048),
		Auth:    auth.New(st, masterKey, cfg.Auth.MaxClockSkew.D()),
		Limiter: limiter,
		Metrics: reg,
		Mod:     mod,
		Logger:  logger,
	})

	// 后台维护：带宽看门狗 + 限流桶回收 + 快照裁剪
	if cfg.Bandwidth.WatchEnabled {
		wd := metrics.NewWatchdog(reg, cfg.Bandwidth.MaxMbps, time.Second, 60)
		go wd.Run(ctx)
		logger.Info("带宽看门狗已启动", "max_mbps", cfg.Bandwidth.MaxMbps, "degrade", cfg.Bandwidth.Degrade)
	}
	go maintenance(ctx, st, limiter, logger)

	httpSrv := &http.Server{
		Addr:         cfg.Server.Addr,
		Handler:      srv.Routes(),
		ReadTimeout:  cfg.Server.ReadTimeout.D(),
		WriteTimeout: cfg.Server.WriteTimeout.D(),
		ErrorLog:     slog.NewLogLogger(logger.Handler(), slog.LevelError),
	}

	go func() {
		logger.Info("服务启动", "addr", cfg.Server.Addr, "tls", !*devMode && cfg.Server.TLSCert != "")
		if err := listen(httpSrv, cfg, *devMode); err != nil && !errors.Is(err, http.ErrServerClosed) {
			logger.Error("HTTP 服务异常退出", "err", err)
			stop()
		}
	}()

	<-ctx.Done()
	logger.Info("收到退出信号，开始优雅关闭")

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := httpSrv.Shutdown(shutdownCtx); err != nil {
		logger.Error("优雅关闭超时", "err", err)
	}
	logger.Info("已停止", "metrics", reg.Snapshot())
}

func listen(s *http.Server, cfg *config.Config, dev bool) error {
	if dev || cfg.Server.TLSCert == "" || cfg.Server.TLSKey == "" {
		return s.ListenAndServe()
	}
	s.TLSConfig = &tls.Config{MinVersion: tls.VersionTLS12}
	return s.ListenAndServeTLS(cfg.Server.TLSCert, cfg.Server.TLSKey)
}

// maintenance 周期回收限流桶并裁剪过期的目录快照。
func maintenance(ctx context.Context, st *store.Store, lim *ratelimit.Limiter, log *slog.Logger) {
	ticker := time.NewTicker(10 * time.Minute)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			lim.GC()
			if err := st.PruneSnapshots(ctx, snapshotKeepSeconds); err != nil {
				log.Warn("裁剪目录快照失败", "err", err)
			}
		}
	}
}

func runPutKey(ctx context.Context, st *store.Store, spec string, log *slog.Logger) error {
	masterKey, err := config.SecretKey()
	if err != nil {
		return err
	}
	name, plainKey, plainSecret, err := parseKeySpec(spec)
	if err != nil {
		return err
	}
	if err := st.PutAPIKey(ctx, plainKey, name, plainSecret, masterKey); err != nil {
		return err
	}
	log.Info("API Key 已登记", "name", name)
	return nil
}

func parseKeySpec(spec string) (name, plainKey, plainSecret string, err error) {
	parts := strings.SplitN(spec, ":", 3)
	if len(parts) != 3 {
		return "", "", "", fmt.Errorf("格式应为 name:plainKey:plainSecret，收到 %q", spec)
	}
	for i := range parts {
		parts[i] = strings.TrimSpace(parts[i])
		if parts[i] == "" {
			return "", "", "", fmt.Errorf("name/plainKey/plainSecret 均不能为空")
		}
	}
	return parts[0], parts[1], parts[2], nil
}

// loadMasterKey 读取解密 HMAC secret 的主密钥。
// -dev 时生成一次性随机密钥并告警，方便本地跑通但不误用于生产。
func loadMasterKey(dev bool, log *slog.Logger) ([]byte, error) {
	key, err := config.SecretKey()
	if err == nil {
		return key, nil
	}
	if !dev {
		return nil, err
	}
	fallback := make([]byte, 32)
	if _, rerr := rand.Read(fallback); rerr != nil {
		return nil, fmt.Errorf("生成临时主密钥失败: %w", rerr)
	}
	log.Warn("未设置 BENCH_SECRET_KEY，已生成一次性主密钥；重启后既有 HMAC Key 将全部失效")
	return fallback, nil
}

func newLogger(level string, dev bool) *slog.Logger {
	var lvl slog.Level
	switch strings.ToLower(level) {
	case "debug":
		lvl = slog.LevelDebug
	case "warn":
		lvl = slog.LevelWarn
	case "error":
		lvl = slog.LevelError
	default:
		lvl = slog.LevelInfo
	}
	opts := &slog.HandlerOptions{Level: lvl}
	if dev {
		return slog.New(slog.NewTextHandler(os.Stderr, opts))
	}
	return slog.New(slog.NewJSONHandler(os.Stderr, opts))
}

// preview 取正文首行并截断，避免待审核队列输出刷屏。
func preview(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	r := []rune(s)
	if len(r) > 40 {
		return string(r[:40]) + "…"
	}
	return string(r)
}
