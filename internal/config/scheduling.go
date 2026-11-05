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
// background 为并行模型（autostart 成员启动时加载、常驻并行；非 autostart 者
// 不随启动加载，收到其模型 ID 请求时才单独拉起，与运行中的成员并行）。
const (
	ModeCoding     = "coding"
	ModeBackground = "background"
)

// SchedulingConfig 描述进程调度子系统的配置。为 nil 时整个调度子系统禁用，
// 代理退化为纯转发模式（转发到 backends.list 外部后端）。
//
// Models 以模型 ID 为 key 描述本地模型进程表（key 即模型 ID，不再单独配置 model
// 字段；viper 将 key 统一小写化）。调度器在两种互斥状态间切换：
//   - background 组态：autostart 的全部 background 模型并行运行（启动集，各自独立
//     端口，可各自绑定一张 GPU），就绪后都作为动态节点入代理池；非 autostart 的
//     background 不在启动时加载，收到其模型 ID 请求时单独拉起（按需，并行运行）；
//   - coding 态：仅运行被请求的 coding 模型进程（可双卡张量并行），background 组
//     被停止释放显存；空闲租约到期后自动切回 background 组态。
// coding 条目可多个（按需切换）；background 条目可多个，autostart: true 的成员
// 构成启动集（可为 background 的真子集，其余按需）。
// lease/switch 为保留子 key，不得用作模型 ID。
//
// background 条目可附加三个可选标记：
//   - autostart：启动时即拉起并常驻（加入启动集）；缺省按需加载；
//   - resident：常驻模型，coding 态切换时不停止，与 coding 模型并行运行（共享
//     显存），用于翻译/Embedding 等需全程在线的专用服务（纯 CPU 模型天然幸存，
//     无此必要）；
//   - no_pool：不加入代理池（不被 llm_proxy 轮询命中），仅按模型 ID 直连。
type SchedulingConfig struct {
	Models map[string]ProcessConfig `mapstructure:"models" validate:"dive"`
	Lease  LeaseConfig              `mapstructure:"lease"`
	Switch SwitchConfig             `mapstructure:"switch"`
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
	// Autostart 仅对 background 生效：启动时即拉起并常驻（加入启动集）。缺省（false）
	// 表示按需加载：不随代理启动加载，收到其模型 ID 请求时才单独拉起，与运行中的
	// 成员并行。coding 条目忽略此标记（coding 永远按需）。
	Autostart bool `mapstructure:"autostart"`
	// Resident 仅对 background 生效：常驻模型，coding 态切换时不停止，与 coding
	// 模型并行运行（共享显存）。主要用于 GPU 模型显式声明“与 coding 共存”（并因此
	// 钉死该卡，acquireGPU 不驱逐它）。未声明 gpu 的纯 CPU 模型不占显存，coding
	// 切换天然不停它，无需此标记。用于翻译/Embedding 等专用服务，使其在开发期间保持在线。
	// GPU 模型要求显存同时装得下 coding 模型与全部常驻模型，否则 coding 模型加载会 OOM。
	Resident bool `mapstructure:"resident"`
	// NoPool 仅对 background 生效：该模型不加入代理池（不被 llm_proxy 轮询命中），
	// 仅当请求使用其模型 ID 时直连。适合翻译/Embedding 等不应混入通用均衡流量的专用模型。
	NoPool bool `mapstructure:"no_pool"`
	// SleepTimeout 仅对 background 生效：该成员无在途请求、且距最近一次请求（或首次
	// 就绪）超过该时长后，调度器主动停止其进程释放显存（休眠）；下次请求该模型 ID 时
	// 按需重新加载唤醒。0 表示禁用休眠（默认，成员就绪后永久常驻）。与 resident 正交：
	// resident 决定 coding 切换时是否保留，SleepTimeout 决定闲置时是否停进程。
	// 要求 NoPool：休眠判定依赖"全部流量都是按模型 ID 的直连请求"（可被 TouchModel
	// 完整观测）；池内成员还会被 llm_proxy 轮询命中（不经过 TouchModel），闲置判定会
	// 失真，故禁止对池内成员配置休眠。
	SleepTimeout time.Duration `mapstructure:"sleep_timeout"`
	// GPU 声明该模型进程占用的 GPU（如 "CUDA1"，对应 llama-server 的 --device）。
	// 显存是唯一不可超卖的硬件资源，调度器按此做 per-GPU 独占仲裁：
	//   - resident 模型的 GPU 被它独占（常驻在线、永久占卡），配置时即拒绝其他
	//     模型声明同一 GPU；
	//   - 两个非 resident 模型可声明同一 GPU，运行时互斥换入：请求其中一方时，
	//     先把同卡上正在运行的另一方形优雅停止（排空在途、等显存释放）再加载；
	//   - 空串 = 未声明 GPU（纯 CPU 模型或不确定），不参与仲裁。
	// 大小写不敏感（cuda1 与 CUDA1 等价）。
	GPU string `mapstructure:"gpu"`
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

// DefaultBackgroundGroup 返回启动集（全部 autostart 的 background 模型）的模型 ID
// 列表（字典序，确定性）。非 autostart 的 background 条目按需加载（收到其模型 ID
// 请求时才拉起）。
func (c *SchedulingConfig) DefaultBackgroundGroup() []string {
	if c == nil {
		return nil
	}
	var out []string
	for id, e := range c.Models {
		if e.Mode == ModeBackground && e.Autostart {
			out = append(out, id)
		}
	}
	sort.Strings(out)
	return out
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

// schedulingReservedKeys 是 scheduling 下的保留子 key（全局时序配置），
// 不得用作模型 ID。
var schedulingReservedKeys = map[string]bool{"lease": true, "switch": true}

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
		// GPU 声明归一化：trim + 大写（cuda1 与 CUDA1 等价，避免大小写拼写差异绕过仲裁）。
		pc.GPU = strings.ToUpper(strings.TrimSpace(pc.GPU))
		models[key] = pc
	}
	sc.Models = models
	return nil
}

// validateScheduling 校验调度配置：
//   - 每个条目 mode 必填（coding 或 background）；配置了 command 时必须指定 readiness_url；
//   - coding 条目可多个；background 条目可多个，autostart 者构成启动集（启动时加载、
//     常驻并行），非 autostart 者收到请求才按需拉起；
//   - gpu 仲裁：resident 模型的 gpu 全局独占（常驻占卡，同卡其他模型配置即报错）；
//     启动集内两个成员不能声明同一 gpu（启动时并行加载会抢同一张卡）；
//     非 resident 模型间可共享 gpu（运行时互斥换入）。
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
		if e.Autostart && e.Mode != ModeBackground {
			return fmt.Errorf("%s: autostart 仅支持 background 模式（coding 永远按需加载）", where)
		}
		if e.Resident && e.Mode != ModeBackground {
			return fmt.Errorf("%s: resident 仅支持 background 模式", where)
		}
		if e.NoPool && e.Mode != ModeBackground {
			return fmt.Errorf("%s: no_pool 仅支持 background 模式", where)
		}
		if e.SleepTimeout > 0 {
			if e.Mode != ModeBackground {
				return fmt.Errorf("%s: sleep_timeout 仅支持 background 模式", where)
			}
			if !e.NoPool {
				return fmt.Errorf("%s: 配置 sleep_timeout 时必须同时设置 no_pool（池内成员会被轮询命中，闲置判定失真）", where)
			}
		}
	}
	// 启动集（autostart 成员）启动时并行加载：同 gpu 的两个成员会在同一时刻抢同一张卡，
	// 配置时即拦截（非 resident 间的运行时互斥换入只发生在“启动 vs 按需”之间）。
	groupGPU := make(map[string]string) // gpu -> 启动集内占用者
	for _, d := range sc.DefaultBackgroundGroup() {
		e := sc.Models[d]
		if e.GPU == "" {
			continue
		}
		if prev, ok := groupGPU[e.GPU]; ok {
			return fmt.Errorf("scheduling.%s: gpu %s 与启动集成员 %s 相同（启动时并行加载会抢同一张卡）", d, e.GPU, prev)
		}
		groupGPU[e.GPU] = d
	}
	// gpu 独占仲裁（resident 的卡不可被任何其他模型声明）：resident 常驻在线、
	// 永久占卡显存，同卡其他模型永远装不下，配置时即拦截而非运行时报 OOM。
	gpuResident := make(map[string]string) // gpu -> 占用它的 resident 模型 ID
	for id, e := range sc.Models {
		if e.GPU == "" || !e.Resident {
			continue
		}
		if prev, ok := gpuResident[e.GPU]; ok {
			return fmt.Errorf("scheduling.%s: gpu %s 已被 resident 模型 %s 独占（一张卡只能容纳一个 resident 模型）", id, e.GPU, prev)
		}
		gpuResident[e.GPU] = id
	}
	for id, e := range sc.Models {
		if e.GPU == "" {
			continue
		}
		if r, pinned := gpuResident[e.GPU]; pinned && r != id {
			return fmt.Errorf("scheduling.%s: gpu %s 已被 resident 模型 %s 独占，模型 %s 将永远无法加载", id, e.GPU, r, id)
		}
	}
	// 组外 background 条目合法：不随启动加载，收到其模型 ID 请求时按需拉起
	//（scheduler.ensureOnDemandBackground），与组内成员并行运行；同 gpu 的非
	// resident 模型间由调度器运行时互斥换入（acquireGPU）。
	return nil
}

