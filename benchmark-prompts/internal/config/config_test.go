package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func writeCfg(t *testing.T, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "c.yaml")
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatalf("写配置文件失败: %v", err)
	}
	return p
}

func TestDefaultIsValid(t *testing.T) {
	cfg := Default()
	if err := cfg.Validate(); err != nil {
		t.Fatalf("默认配置必须自洽: %v", err)
	}
	if cfg.Server.Addr != ":8080" {
		t.Fatalf("默认 addr 不符: %s", cfg.Server.Addr)
	}
	if cfg.Auth.MaxClockSkew.D() != 5*time.Minute {
		t.Fatalf("默认时钟容差不符: %v", cfg.Auth.MaxClockSkew.D())
	}
	// 配额必须与 docs/api.md §6 一致
	if got := cfg.RateLimit.Anonymous["meta"]; got != 10 {
		t.Fatalf("匿名 meta 配额应为 10，得到 %d", got)
	}
	if got := cfg.RateLimit.Authed["upload"]; got != 10 {
		t.Fatalf("已鉴权 upload 配额应为 10，得到 %d", got)
	}
}

// TestDurationHumanReadable 覆盖自定义 Duration 解析：
// yaml.v3 不会自动把 "3s" 解析成 time.Duration，缺了 UnmarshalYAML 会启动即失败。
func TestDurationHumanReadable(t *testing.T) {
	p := writeCfg(t, "server:\n  addr: \":9999\"\n  read_timeout: 3s\nratelimit:\n  window: 90s\n")
	cfg, err := Load(p)
	if err != nil {
		t.Fatalf("加载失败: %v", err)
	}
	if cfg.Server.ReadTimeout.D() != 3*time.Second {
		t.Fatalf("read_timeout 解析不符: %v", cfg.Server.ReadTimeout.D())
	}
	if cfg.RateLimit.Window.D() != 90*time.Second {
		t.Fatalf("window 解析不符: %v", cfg.RateLimit.Window.D())
	}
	if cfg.Server.Addr != ":9999" {
		t.Fatalf("addr 覆盖失败: %s", cfg.Server.Addr)
	}
}

func TestBadDurationRejected(t *testing.T) {
	p := writeCfg(t, "server:\n  read_timeout: bogus\n")
	if _, err := Load(p); err == nil {
		t.Fatalf("非法时长必须报错，不能静默变 0")
	}
}

func TestMalformedYAMLRejected(t *testing.T) {
	p := writeCfg(t, "server: [oops\n")
	if _, err := Load(p); err == nil {
		t.Fatalf("非法 YAML 必须报错")
	}
}

func TestMissingFileRejected(t *testing.T) {
	if _, err := Load(filepath.Join(t.TempDir(), "absent.yaml")); err == nil {
		t.Fatalf("配置缺失且显式指定路径时应报错")
	}
}

func TestEnvOverrides(t *testing.T) {
	t.Setenv("BENCH_SERVER_ADDR", ":1234")
	t.Setenv("BENCH_STORE_PATH", filepath.Join(t.TempDir(), "env.db"))
	t.Setenv("BENCH_TLS_CERT", "/tmp/x.crt")

	cfg, err := Load("")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Server.Addr != ":1234" {
		t.Fatalf("环境变量应覆盖 addr，得到 %s", cfg.Server.Addr)
	}
	if cfg.Store.Path == "" || !strings.HasSuffix(cfg.Store.Path, "env.db") {
		t.Fatalf("环境变量应覆盖 store.path，得到 %s", cfg.Store.Path)
	}
	if cfg.Server.TLSCert != "/tmp/x.crt" {
		t.Fatalf("环境变量应覆盖 tls_cert，得到 %s", cfg.Server.TLSCert)
	}
}

func TestValidateRejectsBrokenValues(t *testing.T) {
	cases := map[string]func(c *Config){
		"空 addr":     func(c *Config) { c.Server.Addr = "" },
		"空 store 路径": func(c *Config) { c.Store.Path = "" },
		"带宽阈值非正数":    func(c *Config) { c.Bandwidth.MaxMbps = 0 },
		"正文上限非正数":    func(c *Config) { c.Moderation.MaxPromptLen = -1 },
	}
	for name, mut := range cases {
		cfg := Default()
		mut(cfg)
		if err := cfg.Validate(); err == nil {
			t.Fatalf("%s：应被校验拒绝", name)
		}
	}
}

func TestSecretKey(t *testing.T) {
	t.Setenv("BENCH_SECRET_KEY", "")
	if _, err := SecretKey(); err == nil {
		t.Fatalf("未设置主密钥必须报错")
	}

	t.Setenv("BENCH_SECRET_KEY", "zzzz")
	if _, err := SecretKey(); err == nil {
		t.Fatalf("非 hex 必须报错")
	}

	t.Setenv("BENCH_SECRET_KEY", strings.Repeat("ab", 16))
	if _, err := SecretKey(); err == nil {
		t.Fatalf("长度不是 32 字节必须报错")
	}

	t.Setenv("BENCH_SECRET_KEY", strings.Repeat("ab", 32))
	key, err := SecretKey()
	if err != nil {
		t.Fatalf("合法主密钥应通过: %v", err)
	}
	if len(key) != 32 {
		t.Fatalf("应解码出 32 字节，得到 %d", len(key))
	}
}
