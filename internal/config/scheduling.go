// Package config 定义 YAML 配置结构与运行时（legacy）配置。
// 配置通过 viper 读取文件并解码（mapstructure 标签），解码后应用默认值，
// 再用 go-playground/validator 做声明式校验，最后做跨字段/业务校验。
package config

import (
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/mitchellh/mapstructure"
	"github.com/spf13/viper"
)

// 进程模式：coding 为按需模型（收到流量才切换，空闲超时后按租约切回默认 background）；
// background 为常驻模型（默认启动加载，就绪后作为动态节点加入代理池）。
const (
	ModeCoding     = "coding"
	ModeBackground = "background"
)

// SchedulingConfig 描述进程调度子系统的配置。为 nil 时整个调度子系统禁用，
// 代理退化为纯转发模式（转发到 backends.list 外部后端）。
//
// Models 以模型 ID 为 key 描述本地模型进程表（key 即模型 ID，不再单独配置 model
// 字段；viper 将 key 统一小写化）。调度器在两种互斥状态间切换：
//   - background 组态：default 列出的全部 background 模型常驻并行运行（各自独立
//     端口，可各自绑定一张 GPU），就绪后都作为动态节点入代理池；
//   - coding 态：仅运行被请求的 coding 模型进程（可双卡张量并行），background 组
//     被停止释放显存；空闲租约到期后自动切回 background 组态。
// coding 条目可多个（按需切换）；background 条目必须全部在 default 组内。
// lease/switch/default 为保留子 key，不得用作模型 ID。
type SchedulingConfig struct {
	Models map[string]ProcessConfig `mapstructure:"models" validate:"dive"`
	Lease  LeaseConfig              `mapstructure:"lease"`
	Switch SwitchConfig             `mapstructure:"switch"`
	// Default 是常驻默认 background 组的模型 ID 列表：启动时并行加载组内全部模型
	//（各自独立端口/GPU），coding 空闲租约到期后切回整个组。可写成标量或列表
	//（弱类型解码统一归一为列表）；仅有一个 background 条目时可省略（隐式单成员组）。
	Default []string `mapstructure:"default"`
}

// ProcessConfig 描述一个本地模型进程的启动配置。
// 条目在 scheduling 下的 key 即该进程对外服务的模型 ID：客户端请求该模型 ID 时，
// 调度器据此启动（或切换到）对应进程；模型 ID 唯一性由 key 天然保证。
type ProcessConfig struct {
	Command string `mapstructure:"command"`
	// Mode 进程模式：coding（按需，空闲租约到期切回默认 background）或
	// background（常驻，就绪后作为动态节点加入代理池参与负载均衡）。
	Mode         string `mapstructure:"mode" validate:"omitempty,oneof=coding background"`
	ReadinessURL string `mapstructure:"readiness_url"`
	// APIKey 是模型进程自身的 API Key（对应 llama-server 的 --api-key）。
	// 非空时：就绪探测与代理转发均使用 Authorization: Bearer <key>，客户端 key 与后端 key 隔离。
	APIKey  string `mapstructure:"api_key"`
	LogFile string `mapstructure:"log_file"`
	// Weight 仅对 background 生效：该模型作为后端节点加入代理池（与 backends.list
	// 一起负载均衡）时的权重，未配置按 1 处理。
	Weight int `mapstructure:"weight" validate:"gte=1"`
	// Tags 仅对 background 生效：该节点入池后携带的路由标签，routing 规则的 pool
	// 可与之对应（含 "tool_call" 标签即加入 tool_call 子池）。
	Tags []string `mapstructure:"tags"`
}

// Entry 返回指定模型 ID 的调度条目（未配置时返回 nil）。
func (c *SchedulingConfig) Entry(modelID string) *ProcessConfig {
	if c == nil {
		return nil
	}
	e, ok := c.Models[strings.TrimSpace(modelID)]
	if !ok {
		return nil
	}
	return &e
}

// BackgroundModels 返回所有 background 模式条目（按模型 ID 字典序排序，保证确定性）。
func (c *SchedulingConfig) BackgroundModels() []*ProcessConfig {
	if c == nil {
		return nil
	}
	ids := make([]string, 0, len(c.Models))
	for id, e := range c.Models {
		if e.Mode == ModeBackground {
			ids = append(ids, id)
		}
	}
	sort.Strings(ids)
	out := make([]*ProcessConfig, 0, len(ids))
	for _, id := range ids {
		e := c.Models[id]
		out = append(out, &e)
	}
	return out
}

// DefaultBackgroundGroup 返回常驻默认 background 组的模型 ID 列表：显式 default
// 优先；仅有一个 background 条目时它即隐式单成员组；未配置且多个 background
// 时返回空（配置错误，Load 阶段已拦截）。
func (c *SchedulingConfig) DefaultBackgroundGroup() []string {
	if c == nil {
		return nil
	}
	if len(c.Default) > 0 {
		out := make([]string, 0, len(c.Default))
		for _, d := range c.Default {
			if d = strings.TrimSpace(d); d != "" {
				out = append(out, d)
			}
		}
		return out
	}
	var solo string
	count := 0
	for id, e := range c.Models {
		if e.Mode == ModeBackground {
			count++
			solo = id
		}
	}
	if count == 1 {
		return []string{solo}
	}
	return nil
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

// schedulingReservedKeys 是 scheduling 下的保留子 key（全局时序/默认背景配置），
// 不得用作模型 ID。
var schedulingReservedKeys = map[string]bool{"lease": true, "switch": true, "default": true}

// decodeSchedulingModels 将 scheduling 子 map 中除保留 key 外的所有 key 解码为模型条目
// （key 即模型 ID）。注意 viper 会将所有 key 统一小写化，模型 ID 按小写形式生效。
// 直接用 vp.Get 取原始 map：Sub().AllSettings() 会按 keyDelim（.）拆 key，
// 形如 qwen3.8 的模型 ID 会被拆成嵌套 map。
func decodeSchedulingModels(vp *viper.Viper, sc *SchedulingConfig) error {
	rawVal := vp.Get("scheduling")
	if rawVal == nil {
		return nil
	}
	raw, ok := rawVal.(map[string]any)
	if !ok {
		return fmt.Errorf("scheduling: 必须是映射结构")
	}
	// 保留 key 形态检查：内容含进程条目字段（command/mode）说明模型误用了保留名。
	for key := range schedulingReservedKeys {
		if m, ok := raw[key].(map[string]any); ok {
			if _, has := m["command"]; has {
				return fmt.Errorf("scheduling.%s: 保留 key，不能用作模型 ID", key)
			}
		}
	}
	models := make(map[string]ProcessConfig, len(raw))
	for key, val := range raw {
		if schedulingReservedKeys[key] {
			continue
		}
		m, ok := val.(map[string]any)
		if !ok {
			return fmt.Errorf("scheduling.%s: 必须是进程条目映射", key)
		}
		var pc ProcessConfig
		dec, err := mapstructure.NewDecoder(&mapstructure.DecoderConfig{
			TagName:          "mapstructure",
			WeaklyTypedInput: true,
			Result:           &pc,
			DecodeHook: mapstructure.ComposeDecodeHookFunc(
				mapstructure.StringToTimeDurationHookFunc(),
				mapstructure.StringToSliceHookFunc(","),
			),
		})
		if err != nil {
			return fmt.Errorf("init decoder for scheduling.%s: %w", key, err)
		}
		if err := dec.Decode(m); err != nil {
			return fmt.Errorf("decode scheduling.%s: %w", key, err)
		}
		models[key] = pc
	}
	sc.Models = models
	return nil
}

// validateScheduling 校验调度配置：
//   - 每个条目 mode 必填（coding 或 background）；配置了 command 时必须指定 readiness_url；
//   - coding 条目可多个；background 条目必须全部在 default 组内（常驻并行运行）；
//   - default 列表成员必须都是 background 条目；未指定 default 且存在多个 background 时报错。
func validateScheduling(sc *SchedulingConfig) error {
	if sc == nil {
		return nil
	}
	for id, e := range sc.Models {
		where := "scheduling." + id
		if strings.TrimSpace(e.Mode) == "" {
			return fmt.Errorf("%s: 必须指定 mode（coding 或 background）", where)
		}
		if e.Mode != ModeCoding && e.Mode != ModeBackground {
			return fmt.Errorf("%s: mode 仅支持 coding 或 background，当前为 %q", where, e.Mode)
		}
		if e.Command != "" && strings.TrimSpace(e.ReadinessURL) == "" {
			return fmt.Errorf("%s: 配置了 command 时必须指定 readiness_url（就绪探测地址）", where)
		}
	}
	for _, d := range sc.DefaultBackgroundGroup() {
		e, ok := sc.Models[d]
		if !ok || e.Mode != ModeBackground {
			return fmt.Errorf("scheduling.default: %q 不存在或不是 background 模式", d)
		}
	}
	for id, e := range sc.Models {
		if e.Mode != ModeBackground {
			continue
		}
		inGroup := false
		for _, d := range sc.DefaultBackgroundGroup() {
			if d == id {
				inGroup = true
				break
			}
		}
		if !inGroup {
			return fmt.Errorf("scheduling.%s: background 模型必须列入 scheduling.default 组（常驻并行运行），当前不在组内将永远不会加载", id)
		}
	}
	return nil
}

