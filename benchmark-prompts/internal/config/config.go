// Package config 负责加载并校验服务配置（YAML + 环境变量覆盖）。
package config

import (
	"encoding/hex"
	"fmt"
	"os"
	"time"

	"gopkg.in/yaml.v3"
)

// Duration 让 YAML 支持 "10s" 这类人类可读时长。
// yaml.v3 不会把字符串自动解析成 time.Duration，必须自定义，否则启动即报错。
type Duration time.Duration

// UnmarshalYAML 实现 yaml.Unmarshaler。
func (d *Duration) UnmarshalYAML(value *yaml.Node) error {
	var s string
	if err := value.Decode(&s); err != nil {
		return fmt.Errorf("duration 必须是字符串: %w", err)
	}
	v, err := time.ParseDuration(s)
	if err != nil {
		return fmt.Errorf("无法解析时长 %q: %w", s, err)
	}
	*d = Duration(v)
	return nil
}

// D 返回标准 time.Duration。
func (d Duration) D() time.Duration { return time.Duration(d) }

// Config 是服务的全部可配置项，对应 docs/architecture.md §4。
type Config struct {
	Server     Server     `yaml:"server"`
	Store      Store      `yaml:"store"`
	Auth       Auth       `yaml:"auth"`
	RateLimit  RateLimit  `yaml:"ratelimit"`
	Bandwidth  Bandwidth  `yaml:"bandwidth"`
	CORS       CORS       `yaml:"cors"`
	Moderation Moderation `yaml:"moderation"`
}

// CORS 前端跳域白名单（源站与 CDN 不同域，见 docs/frontend.md §8）。
type CORS struct {
	AllowedOrigins []string `yaml:"allowed_origins"`
}

// Server HTTP 服务监听配置。
type Server struct {
	Addr         string   `yaml:"addr"`
	ReadTimeout  Duration `yaml:"read_timeout"`
	WriteTimeout Duration `yaml:"write_timeout"`
	TLSCert      string   `yaml:"tls_cert"`
	TLSKey       string   `yaml:"tls_key"`
}

// Store SQLite 配置。
type Store struct {
	Path    string `yaml:"path"`
	Migrate bool   `yaml:"migrate"`
}

// Auth 鉴权配置。
type Auth struct {
	ReadonlyAnonymous bool     `yaml:"readonly_anonymous"`
	MaxClockSkew      Duration `yaml:"max_clock_skew"`
}

// RateLimit 分级限流配置（每窗口请求数）。
type RateLimit struct {
	Window    Duration       `yaml:"window"`
	Anonymous map[string]int `yaml:"anonymous"`
	Authed    map[string]int `yaml:"authed"`
}

// Bandwidth 带宽看门狗配置。阈值由来与校准方法见 docs/deployment.md，
// 不要在别处重复这个数（改一处要扫全仓库）。
type Bandwidth struct {
	WatchEnabled bool     `yaml:"watch_enabled"`
	MaxMbps      float64  `yaml:"max_mbps"`
	Degrade      []string `yaml:"degrade"`
}

// Moderation 上传审核配置。
type Moderation struct {
	Enabled         bool   `yaml:"enabled"`
	BannedWordsFile string `yaml:"banned_words_file"`
	MaxPromptLen    int    `yaml:"max_prompt_len"`
}

// Default 返回一份可直接跑通的默认配置。
func Default() *Config {
	return &Config{
		Server: Server{
			Addr:         ":8080",
			ReadTimeout:  Duration(10 * time.Second),
			WriteTimeout: Duration(20 * time.Second),
		},
		Store: Store{Path: "./bench.db", Migrate: true},
		Auth: Auth{
			ReadonlyAnonymous: true,
			MaxClockSkew:      Duration(300 * time.Second),
		},
		RateLimit: RateLimit{
			Window: Duration(60 * time.Second),
			// 与 docs/api.md §6 的配额表一致
			Anonymous: map[string]int{"meta": 10, "list": 60, "random": 60, "get": 60, "delta": 5},
			Authed: map[string]int{
				"meta": 60, "list": 300, "random": 300, "get": 300,
				"delta": 30, "scores": 30, "upload": 10,
			},
		},
		Bandwidth: Bandwidth{
			WatchEnabled: true,
			MaxMbps:      8.0,
			Degrade:      []string{"delta", "list"},
		},
		CORS:       CORS{AllowedOrigins: []string{"*"}},
		Moderation: Moderation{Enabled: true, MaxPromptLen: 8192},
	}
}

// Load 读取 YAML（path 为空则只用默认值），叠加环境变量，最后校验。
func Load(path string) (*Config, error) {
	cfg := Default()
	if path != "" {
		b, err := os.ReadFile(path)
		if err != nil {
			return nil, fmt.Errorf("读取配置失败: %w", err)
		}
		if err := yaml.Unmarshal(b, cfg); err != nil {
			return nil, fmt.Errorf("解析配置失败: %w", err)
		}
	}
	cfg.applyEnv()
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	return cfg, nil
}

// applyEnv 用环境变量覆盖敏感/部署相关项。
func (c *Config) applyEnv() {
	if v := os.Getenv("BENCH_SERVER_ADDR"); v != "" {
		c.Server.Addr = v
	}
	if v := os.Getenv("BENCH_STORE_PATH"); v != "" {
		c.Store.Path = v
	}
	if v := os.Getenv("BENCH_TLS_CERT"); v != "" {
		c.Server.TLSCert = v
	}
	if v := os.Getenv("BENCH_TLS_KEY"); v != "" {
		c.Server.TLSKey = v
	}
}

// Validate 启动前自检。
func (c *Config) Validate() error {
	if c.Server.Addr == "" {
		return fmt.Errorf("server.addr 不能为空")
	}
	if c.Store.Path == "" {
		return fmt.Errorf("store.path 不能为空")
	}
	if c.Bandwidth.MaxMbps <= 0 {
		return fmt.Errorf("bandwidth.max_mbps 必须大于 0")
	}
	if c.Moderation.MaxPromptLen <= 0 {
		return fmt.Errorf("moderation.max_prompt_len 必须大于 0")
	}
	return nil
}

// SecretKey 读取 HMAC secret 的加密主密钥（32 字节，hex 编码）。
// 用于解密 api_keys.secret_enc；只允许来自环境变量，绝不入库、不入仓库。
func SecretKey() ([]byte, error) {
	v := os.Getenv("BENCH_SECRET_KEY")
	if v == "" {
		return nil, fmt.Errorf("缺少环境变量 BENCH_SECRET_KEY（写入端点的 HMAC secret 解密主密钥）")
	}
	key, err := hex.DecodeString(v)
	if err != nil {
		return nil, fmt.Errorf("BENCH_SECRET_KEY 必须是 hex 编码: %w", err)
	}
	if len(key) != 32 {
		return nil, fmt.Errorf("BENCH_SECRET_KEY 必须是 32 字节（64 位 hex），当前 %d 字节", len(key))
	}
	return key, nil
}
