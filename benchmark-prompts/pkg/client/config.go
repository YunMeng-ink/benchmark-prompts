package client

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"
)

// Config 是 bench CLI 的本地配置，默认落在 ~/.bench/config。
type Config struct {
	Endpoint string `yaml:"endpoint"`
	APIKey   string `yaml:"api_key"`
	Secret   string `yaml:"secret"`
	// DeviceID 留空时由本地缓存自动生成并持久化（评分去重指纹）。
	DeviceID string `yaml:"device_id"`
}

// HomeDir 返回 bench 家目录：BENCH_HOME 优先，否则 ~/.bench。
// 测试正是靠 BENCH_HOME 把配置与缓存隔离到临时目录的。
func HomeDir() (string, error) {
	if v := strings.TrimSpace(os.Getenv("BENCH_HOME")); v != "" {
		return v, nil
	}
	h, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("无法定位用户主目录: %w", err)
	}
	return filepath.Join(h, ".bench"), nil
}

// DefaultConfigPath 返回配置文件路径。
func DefaultConfigPath() (string, error) {
	d, err := HomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(d, "config"), nil
}

// DefaultCachePath 返回本地缓存 SQLite 文件路径。
func DefaultCachePath() (string, error) {
	d, err := HomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(d, "cache.db"), nil
}

// LoadConfig 读取配置。**文件不存在时返回空配置而非错误**——首次运行是正常的。
func LoadConfig(path string) (*Config, error) {
	if path == "" {
		p, err := DefaultConfigPath()
		if err != nil {
			return nil, err
		}
		path = p
	}
	b, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return &Config{}, nil
		}
		return nil, fmt.Errorf("读取配置失败: %w", err)
	}
	cfg := &Config{}
	if err := yaml.Unmarshal(b, cfg); err != nil {
		return nil, fmt.Errorf("解析配置失败: %w", err)
	}
	return cfg, nil
}

// Save 写入配置。权限固定 0600：文件里可能有 API Key。
//
// 注意必须显式 Chmod：os.WriteFile 的 perm 只在**创建**时生效，
// 文件已存在时（典型场景：上一次用了别的权限，或被其他工具创建）
// 不会收紧权限。
func (c *Config) Save(path string) error {
	if path == "" {
		p, err := DefaultConfigPath()
		if err != nil {
			return err
		}
		path = p
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("创建配置目录失败: %w", err)
	}
	b, err := yaml.Marshal(c)
	if err != nil {
		return fmt.Errorf("序列化配置失败: %w", err)
	}
	if err := os.WriteFile(path, b, 0o600); err != nil {
		return fmt.Errorf("写入配置失败: %w", err)
	}
	// 尽力而为：POSIX 下必定收紧成功；Windows 不支持 POSIX 权限位（只映射
	// 只读位），把它当致命错误会让整个 config init 在 Windows 上不可用。
	// Windows 上的凭据保护依赖文件放在用户配置目录（ACL 默认仅当前用户可读）。
	_ = os.Chmod(path, 0o600)
	return nil
}

// ApplyEnv 用环境变量覆盖配置文件的值，便于容器/CI 场景免改文件。
func (c *Config) ApplyEnv() {
	if v := strings.TrimSpace(os.Getenv("BENCH_ENDPOINT")); v != "" {
		c.Endpoint = v
	}
	if v := strings.TrimSpace(os.Getenv("BENCH_API_KEY")); v != "" {
		c.APIKey = v
	}
	if v := strings.TrimSpace(os.Getenv("BENCH_SECRET")); v != "" {
		c.Secret = v
	}
	if v := strings.TrimSpace(os.Getenv("BENCH_DEVICE_ID")); v != "" {
		c.DeviceID = v
	}
}

// NormalizeEndpoint 补全缺失的 scheme，让 "bench.example.com" 也能用。
// 明确拒绝非 http(s) 协议，避免把 file/unix 之类的东西送进 http.Client。
func NormalizeEndpoint(raw string) (string, error) {
	v := strings.TrimSpace(raw)
	if v == "" {
		return "", errors.New("endpoint 不能为空")
	}
	if !strings.Contains(v, "://") {
		v = "https://" + v
	}
	if !strings.HasPrefix(v, "http://") && !strings.HasPrefix(v, "https://") {
		return "", fmt.Errorf("endpoint 协议必须是 http/https，得到 %q", raw)
	}
	return strings.TrimRight(v, "/"), nil
}

// ToOptions 把配置转成客户端参数。
func (c *Config) ToOptions() Options {
	return Options{
		BaseURL:  c.Endpoint,
		APIKey:   c.APIKey,
		Secret:   c.Secret,
		DeviceID: c.DeviceID,
	}
}
