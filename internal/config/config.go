// Package config 定义 YAML 配置结构与运行时（legacy）配置。
// 配置通过 viper 读取文件并解码（mapstructure 标签），解码后应用默认值，
// 再用 go-playground/validator 做声明式校验，最后做跨字段/业务校验。
package config

import (
	"fmt"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/go-playground/validator/v10"
	"github.com/mitchellh/mapstructure"
	"github.com/spf13/viper"
)


type Config struct {
	ListenAddr         string
	DataDir            string
	RetentionDays      int
	MaxRequestBytes    int64
	MaxCaptureBytes    int64
	RequestTimeout     time.Duration
	PollBackendMetrics bool
	PollInterval       time.Duration
	RecordPaths        []string
	APIKey             string   // 客户端访问 proxy 的 API Key，空值则不鉴权
	UIAllowedHosts     []string // 允许访问 /_proxy/ui 的 Host 白名单，空则不限制
	LogLevel           string
}

type YAMLConfig struct {
	Server     ServerConfig      `mapstructure:"server"`
	Database   DatabaseConfig    `mapstructure:"database"`
	Backends   BackendsConfig    `mapstructure:"backends"`
	Proxy      ProxyConfig       `mapstructure:"proxy"`
	Routing    *RoutingConfig    `mapstructure:"routing"`
	Scheduling *SchedulingConfig `mapstructure:"scheduling"`
}

type ServerConfig struct {
	ListenAddr string `mapstructure:"listen_addr" validate:"required"`
	DataDir    string `mapstructure:"data_dir" validate:"required"`
	// UIAllowedHosts 允许访问 /_proxy/ui 面板的 Host 列表（忽略端口与大小写）。
	// 为空则不限制；配置后，Host 不在列表内的请求访问面板将返回 403。
	UIAllowedHosts []string `mapstructure:"ui_allowed_hosts"`
	// LogLevel 日志级别：debug / info / warn / error / fatal，留空使用默认值。
	// 具体级别由 log.SetLogLevel 容错处理，此处不做枚举校验。
	LogLevel string `mapstructure:"log_level"`
}

type DatabaseConfig struct {
	Type       string           `mapstructure:"type" validate:"required,oneof=sqlite postgresql"`
	SQLite     SQLiteConfig     `mapstructure:"sqlite"`
	PostgreSQL PostgreSQLConfig `mapstructure:"postgresql"`
}

type SQLiteConfig struct {
	Path string `mapstructure:"path" validate:"required"`
}

type PostgreSQLConfig struct {
	DSN             string `mapstructure:"dsn" validate:"required_if=Type postgresql"`
	MaxOpenConns    int    `mapstructure:"max_open_conns" validate:"gte=0"`
	MaxIdleConns    int    `mapstructure:"max_idle_conns" validate:"gte=0"`
	ConnMaxLifetime int    `mapstructure:"conn_max_lifetime_seconds" validate:"gte=0"`
}

type BackendsConfig struct {
	List     []BackendConfig `mapstructure:"list" validate:"dive"`
	Strategy string          `mapstructure:"strategy" validate:"required,oneof=wrr swrr rr random"`
}

type BackendConfig struct {
	Name   string `mapstructure:"name" validate:"required"`
	URL    string `mapstructure:"url" validate:"required,backend_url"`
	Weight int    `mapstructure:"weight" validate:"gte=1"`
	// Model 是该后端实际部署的模型 ID，转发时自动重写请求体的 model 字段。
	Model string `mapstructure:"model"`
	// APIKey 是该后端自身的 API Key，转发时自动注入 Authorization: Bearer <key>。
	APIKey string `mapstructure:"api_key"`
	// Tags 是该后端加入的路由标签池列表；routing 规则按 pool 标签选择后端。
	// 加入 "tool_call" 标签即表示该后端支持工具调用，可供 routing 规则 pool: "tool_call" 选用。
	Tags []string `mapstructure:"tags"`
}

// EffectiveTags 返回后端的有效标签集（显式 tags 去除空白后的集合）。
func (b *BackendConfig) EffectiveTags() map[string]bool {
	tags := make(map[string]bool, len(b.Tags))
	for _, t := range b.Tags {
		if t = strings.TrimSpace(t); t != "" {
			tags[t] = true
		}
	}
	return tags
}

type ProxyConfig struct {
	// RetentionDays 数据保留天数：0 表示永久保留（不做清理）；未配置时默认 14。
	RetentionDays      *int     `mapstructure:"retention_days" validate:"omitempty,gte=0"`
	MaxRequestBytes    int      `mapstructure:"max_request_bytes" validate:"gt=0"`
	MaxCaptureBytes    int      `mapstructure:"max_capture_bytes" validate:"gt=0"`
	RequestTimeout     int      `mapstructure:"request_timeout_seconds" validate:"gt=0"`
	PollBackendMetrics *bool    `mapstructure:"poll_backend_metrics"`
	PollInterval       int      `mapstructure:"poll_interval_seconds" validate:"gt=0"`
	RecordPaths        []string `mapstructure:"record_paths"`
	APIKey             string   `mapstructure:"api_key"` // 客户端访问 proxy 的 API Key，与后端 api_key 隔离
}

// v 是进程内共享的 validator 实例，Load 每次解码后对其执行 Struct 校验。
var v = validator.New()

func init() {
	// 注册后端 URL 校验规则：必须为 http:// 或 https:// 开头的合法地址。
	if err := v.RegisterValidation("backend_url", func(fl validator.FieldLevel) bool {
		return ValidateBackendURL(fl.Field().String()) == nil
	}); err != nil {
		panic(fmt.Sprintf("register backend_url validation: %v", err))
	}
}

// Load 用 viper 读取并解码配置文件，应用默认值与 validator 声明式校验，
// 最后执行跨字段/业务校验（routing、scheduling、模型 ID 唯一性）。
func Load(path string) (*YAMLConfig, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}

	vp := viper.New()
	vp.SetConfigFile(path)
	if err := vp.ReadInConfig(); err != nil {
		return nil, fmt.Errorf("read config %s: %w", path, err)
	}

	cfg := &YAMLConfig{}
	dec, err := mapstructure.NewDecoder(&mapstructure.DecoderConfig{
		TagName:          "mapstructure",
		WeaklyTypedInput: true,
		Result:           cfg,
		DecodeHook: mapstructure.ComposeDecodeHookFunc(
			mapstructure.StringToTimeDurationHookFunc(),
			mapstructure.StringToSliceHookFunc(","),
		),
	})
	if err != nil {
		return nil, fmt.Errorf("init decoder: %w", err)
	}
	if err := dec.Decode(vp.AllSettings()); err != nil {
		return nil, fmt.Errorf("decode config %s: %w", path, err)
	}

	if err := checkLegacyRouting(data); err != nil {
		return nil, err
	}

	// viper 会把所有 key 统一小写，routing 规则的 header 头名被一并小写化；
	// 头名匹配本就忽略大小写，这里恢复为规范化形式（如 "User-Agent"）便于展示。
	if cfg.Routing != nil {
		for i := range cfg.Routing.Rules {
			for k := range cfg.Routing.Rules[i].Header {
				if ck := http.CanonicalHeaderKey(k); ck != k {
					cfg.Routing.Rules[i].Header[ck] = cfg.Routing.Rules[i].Header[k]
					delete(cfg.Routing.Rules[i].Header, k)
				}
			}
		}
	}

	// scheduling 子 map 同时包含保留 key（lease/switch/default）与模型条目，
	// 主解码只填保留字段，模型条目在此单独解码入 Models。
	if cfg.Scheduling != nil {
		if err := decodeSchedulingModels(vp, cfg.Scheduling); err != nil {
			return nil, err
		}
	}

	setConfigDefaults(cfg)

	if err := v.Struct(cfg); err != nil {
		return nil, fmt.Errorf("validate config %s: %w", path, err)
	}

	if err := validateRouting(cfg.Routing); err != nil {
		return nil, err
	}

	if err := validateScheduling(cfg.Scheduling); err != nil {
		return nil, err
	}

	if err := cfg.ValidateModelIDs(); err != nil {
		return nil, err
	}

	if err := cfg.ValidatePortCollisions(); err != nil {
		return nil, err
	}

	return cfg, nil
}

func setConfigDefaults(cfg *YAMLConfig) {
	if cfg.Server.ListenAddr == "" {
		cfg.Server.ListenAddr = ":9091"
	}
	if cfg.Server.DataDir == "" {
		cfg.Server.DataDir = "./data"
	}
	if cfg.Database.Type == "" {
		cfg.Database.Type = "sqlite"
	}
	if cfg.Database.SQLite.Path == "" {
		cfg.Database.SQLite.Path = "proxy.db"
	}
	if cfg.Database.PostgreSQL.MaxOpenConns == 0 {
		cfg.Database.PostgreSQL.MaxOpenConns = 25
	}
	if cfg.Database.PostgreSQL.MaxIdleConns == 0 {
		cfg.Database.PostgreSQL.MaxIdleConns = 5
	}
	if cfg.Database.PostgreSQL.ConnMaxLifetime == 0 {
		cfg.Database.PostgreSQL.ConnMaxLifetime = 300
	}
	if cfg.Backends.Strategy == "" {
		cfg.Backends.Strategy = "wrr"
	}
	if cfg.Proxy.RetentionDays == nil {
		v := 14
		cfg.Proxy.RetentionDays = &v
	}
	if cfg.Proxy.MaxRequestBytes == 0 {
		cfg.Proxy.MaxRequestBytes = 32 * 1024 * 1024
	}
	if cfg.Proxy.MaxCaptureBytes == 0 {
		cfg.Proxy.MaxCaptureBytes = 32 * 1024 * 1024
	}
	if cfg.Proxy.RequestTimeout == 0 {
		cfg.Proxy.RequestTimeout = 600
	}
	if cfg.Proxy.PollInterval == 0 {
		cfg.Proxy.PollInterval = 10
	}
	if cfg.Proxy.PollBackendMetrics == nil {
		v := true
		cfg.Proxy.PollBackendMetrics = &v
	}
	if len(cfg.Proxy.RecordPaths) == 0 {
		cfg.Proxy.RecordPaths = []string{"/v1/chat/completions", "/v1/completions", "/v1/embeddings"}
	}

	for i := range cfg.Backends.List {
		if cfg.Backends.List[i].Weight == 0 {
			cfg.Backends.List[i].Weight = 1
		}
	}

	if cfg.Scheduling != nil {
		for id, e := range cfg.Scheduling.Models {
			if e.Weight == 0 {
				e.Weight = 1
				cfg.Scheduling.Models[id] = e
			}
		}
		if cfg.Scheduling.Lease.CodingIdleTimeout == 0 {
			cfg.Scheduling.Lease.CodingIdleTimeout = 30 * time.Minute
		}
		if cfg.Scheduling.Switch.DrainTimeout == 0 {
			cfg.Scheduling.Switch.DrainTimeout = 10 * time.Second
		}
		if cfg.Scheduling.Switch.KillTimeout == 0 {
			cfg.Scheduling.Switch.KillTimeout = 10 * time.Second
		}
		if cfg.Scheduling.Switch.StartupTimeout == 0 {
			cfg.Scheduling.Switch.StartupTimeout = 120 * time.Second
		}
	}
}

// HasScheduling 报告进程调度子系统是否启用（存在至少一个模型条目）。
func (c *YAMLConfig) HasScheduling() bool {
	return c.Scheduling != nil && len(c.Scheduling.Models) > 0
}

func (c *YAMLConfig) ToLegacy() Config {
	return Config{
		ListenAddr:         c.Server.ListenAddr,
		DataDir:            c.Server.DataDir,
		RetentionDays:      *c.Proxy.RetentionDays,
		MaxRequestBytes:    int64(c.Proxy.MaxRequestBytes),
		MaxCaptureBytes:    int64(c.Proxy.MaxCaptureBytes),
		RequestTimeout:     time.Duration(c.Proxy.RequestTimeout) * time.Second,
		PollBackendMetrics: c.Proxy.PollBackendMetrics != nil && *c.Proxy.PollBackendMetrics,
		PollInterval:       time.Duration(c.Proxy.PollInterval) * time.Second,
		RecordPaths:        c.Proxy.RecordPaths,
		APIKey:             c.Proxy.APIKey,
		UIAllowedHosts:     c.Server.UIAllowedHosts,
		LogLevel:           c.Server.LogLevel,
	}
}

func (c *YAMLConfig) HasWeightedBackends() bool {
	return len(c.Backends.List) > 0
}

// ValidateBackendURL 校验后端 URL 格式。
func ValidateBackendURL(raw string) error {
	if strings.TrimSpace(raw) == "" {
		return fmt.Errorf("backend URL is empty")
	}
	if !strings.HasPrefix(raw, "http://") && !strings.HasPrefix(raw, "https://") {
		return fmt.Errorf("backend URL must start with http:// or https://")
	}
	return nil
}
