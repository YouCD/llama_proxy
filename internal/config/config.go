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
	"gopkg.in/yaml.v3"
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

// RoutingConfig 描述按客户端特征路由到指定标签池的规则表。
// 规则按配置顺序评估，首条命中的规则生效；未配置时不存在路由规则，全部流量走默认池。
type RoutingConfig struct {
	Rules []RoutingRule `mapstructure:"rules" validate:"dive"`
}

// RoutingRule 描述一条路由规则：Header 条件命中后，请求只从 Pool 标签对应的后端子池
// 中选择；请求失败时自动故障转移到子池内下一个未尝试的后端。
type RoutingRule struct {
	// Name 规则名，仅用于日志与报错；留空时按序号生成。
	Name string `mapstructure:"name"`
	// Header 规则命中条件：每个请求头的名称与值都必须与客户端请求头匹配，map 内所有
	// key 同时满足（AND）才算命中。头名按规范化形式比较（服务端不区分大小写），头值
	// trim 后精确相等、区分大小写。
	Header map[string]string `mapstructure:"header" validate:"min=1"`
	// Pool 标签名：命中后只从携带该标签（tags）的后端中选择。
	Pool string `mapstructure:"pool" validate:"required"`
}

// SchedulingConfig 描述进程调度子系统的配置。为 nil 时整个调度子系统禁用，
// 代理退化为纯转发模式（转发到 backends.list 外部后端）。
type SchedulingConfig struct {
	Coding     ProcessConfig `mapstructure:"coding"`
	Background ProcessConfig `mapstructure:"background"`
	// Lease 描述开发租约相关配置。
	Lease LeaseConfig `mapstructure:"lease"`
	// Switch 描述模型切换时的时序参数。
	Switch SwitchConfig `mapstructure:"switch"`
}

// ProcessConfig 描述模型进程的启动配置（coding/background 共用）。
// Model 固化该进程对外服务的模型 ID：客户端请求该模型 ID 时，调度器据此启动
// （或切换到）对应进程；启动时校验所有模型 ID 互不重复，保证路由无歧义。
type ProcessConfig struct {
	Command string `mapstructure:"command"`
	// Model 固化该进程服务的模型 ID（如 "qwen3.8"），请求该 ID 时路由/启动对应进程。
	Model        string `mapstructure:"model"`
	ReadinessURL string `mapstructure:"readiness_url"`
	// APIKey 是模型进程自身的 API Key（对应 llama-server 的 --api-key）。
	// 非空时：就绪探测与代理转发均使用 Authorization: Bearer <key>，客户端 key 与后端 key 隔离。
	APIKey  string `mapstructure:"api_key"`
	LogFile string `mapstructure:"log_file"`
	// Weight 仅对 background 生效：本地 background 模型进程就绪后作为后端节点加入
	// 代理池（与 backends.list 一起负载均衡）时的权重，未配置按 1 处理。
	Weight int `mapstructure:"weight" validate:"gte=1"`
	// Tags 仅对 background 生效：本地 background 节点入池后携带的路由标签，
	// routing 规则的 pool 可与之对应（含 "tool_call" 标签即加入 tool_call 子池）。
	Tags []string `mapstructure:"tags"`
}

// EffectiveTags 返回本地 background 节点的有效标签集（语义同 BackendConfig.EffectiveTags）。
func (p *ProcessConfig) EffectiveTags() map[string]bool {
	tags := make(map[string]bool, len(p.Tags))
	for _, t := range p.Tags {
		if t = strings.TrimSpace(t); t != "" {
			tags[t] = true
		}
	}
	return tags
}

// LeaseConfig 描述开发租约相关配置。
type LeaseConfig struct {
	// CodingIdleTimeout 距最后一次 coding 流量超过该时长即切回 background。
	CodingIdleTimeout time.Duration `mapstructure:"coding_idle_timeout"`
}

// SwitchConfig 描述模型切换时的时序参数。
type SwitchConfig struct {
	DrainTimeout   time.Duration `mapstructure:"drain_timeout"`
	KillTimeout    time.Duration `mapstructure:"kill_timeout"`
	StartupTimeout time.Duration `mapstructure:"startup_timeout"`
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
	RetentionDays      int      `mapstructure:"retention_days" validate:"gte=1"`
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

	setConfigDefaults(cfg)

	if err := v.Struct(cfg); err != nil {
		return nil, fmt.Errorf("validate config %s: %w", path, err)
	}

	if err := validateRouting(cfg.Routing); err != nil {
		return nil, err
	}

	if err := validateSchedulingModels(cfg.Scheduling); err != nil {
		return nil, err
	}

	if err := cfg.ValidateModelIDs(); err != nil {
		return nil, err
	}

	return cfg, nil
}

// validateSchedulingModels 校验调度配置：启用了某模型进程（command 非空）时必须
// 固化该进程服务的模型 ID（model），并显式指定就绪探测地址 readiness_url。
func validateSchedulingModels(sc *SchedulingConfig) error {
	if sc == nil {
		return nil
	}
	for _, e := range []struct {
		name string
		pc   *ProcessConfig
	}{
		{"scheduling.coding", &sc.Coding},
		{"scheduling.background", &sc.Background},
	} {
		if e.pc.Command == "" {
			continue
		}
		if strings.TrimSpace(e.pc.Model) == "" {
			return fmt.Errorf("%s: 配置了 command 时必须固化模型名称 model（客户端按该模型 ID 请求/触发启动）", e.name)
		}
		if strings.TrimSpace(e.pc.ReadinessURL) == "" {
			return fmt.Errorf("%s: 配置了 command 时必须指定 readiness_url（就绪探测地址）", e.name)
		}
	}
	return nil
}

// ValidateModelIDs 校验所有已配置的模型 ID（backends.list 的 model 与 scheduling 的
// coding/background model）互不重复，且不与代理占位 ID llm_prox 冲突：
// 具体模型 ID 路由要求"一个模型 ID 唯一对应一个后端/进程"，重复会导致路由歧义。
func (c *YAMLConfig) ValidateModelIDs() error {
	const proxyID = "llm_prox"
	seen := make(map[string]string)
	locate := func(modelID, where string) error {
		if modelID == proxyID {
			return fmt.Errorf("%s: 模型 ID %q 与代理占位 ID %s 冲突（%s 保留用于轮询负载均衡）", where, modelID, proxyID, proxyID)
		}
		if prev, dup := seen[modelID]; dup {
			return fmt.Errorf("模型 ID %q 重复：同时配置在 %s 与 %s（具体模型路由要求模型 ID 全局唯一）", modelID, prev, where)
		}
		seen[modelID] = where
		return nil
	}
	for i := range c.Backends.List {
		b := &c.Backends.List[i]
		if m := strings.TrimSpace(b.Model); m != "" {
			if err := locate(m, "backends.list["+b.Name+"]"); err != nil {
				return err
			}
		}
	}
	if c.Scheduling != nil {
		for _, entry := range []struct {
			name string
			m    string
		}{
			{"scheduling.coding", strings.TrimSpace(c.Scheduling.Coding.Model)},
			{"scheduling.background", strings.TrimSpace(c.Scheduling.Background.Model)},
		} {
			if entry.m == "" {
				continue
			}
			if err := locate(entry.m, entry.name); err != nil {
				return err
			}
		}
	}
	return nil
}

// checkLegacyRouting 检测旧版 routing 配置字段（match / user_agent_contains）并给出
// 迁移提示；这些字段已不再支持。
func checkLegacyRouting(data []byte) error {
	var legacy struct {
		Routing *struct {
			Rules []struct {
				Match             map[string]any `yaml:"match"`
				UserAgentContains string         `yaml:"user_agent_contains"`
			} `yaml:"rules"`
		} `yaml:"routing"`
	}
	if err := yaml.Unmarshal(data, &legacy); err != nil {
		return err
	}
	if legacy.Routing == nil {
		return nil
	}
	for i, lr := range legacy.Routing.Rules {
		if lr.Match != nil || lr.UserAgentContains != "" {
			return fmt.Errorf("routing.rules[%d]: 旧字段 match/user_agent_contains 已移除，请改写为 header（如 User-Agent: \"完整User-Agent值\"）", i)
		}
	}
	return nil
}

// validateRouting 保留作为路由规则的结构兜底校验（声明式校验已覆盖 pool/header
// 非空），当前仅做 trim 后的空串判定以给出带序号的友好报错。
func validateRouting(rc *RoutingConfig) error {
	if rc == nil {
		return nil
	}
	for i, rr := range rc.Rules {
		if strings.TrimSpace(rr.Pool) == "" {
			return fmt.Errorf("routing.rules[%d]: pool 不能为空", i)
		}
		if len(rr.Header) == 0 {
			return fmt.Errorf("routing.rules[%d]: header 不能为空（需配置至少一个请求头条件）", i)
		}
	}
	return nil
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
	if cfg.Proxy.RetentionDays == 0 {
		cfg.Proxy.RetentionDays = 14
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
		if cfg.Scheduling.Coding.Weight == 0 {
			cfg.Scheduling.Coding.Weight = 1
		}
		if cfg.Scheduling.Background.Weight == 0 {
			cfg.Scheduling.Background.Weight = 1
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

// HasScheduling 报告进程调度子系统是否启用。
func (c *YAMLConfig) HasScheduling() bool {
	return c.Scheduling != nil && (c.Scheduling.Coding.Command != "" || c.Scheduling.Background.Command != "")
}

func (c *YAMLConfig) ToLegacy() Config {
	return Config{
		ListenAddr:         c.Server.ListenAddr,
		DataDir:            c.Server.DataDir,
		RetentionDays:      c.Proxy.RetentionDays,
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
