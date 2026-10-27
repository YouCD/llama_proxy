
package scheduler

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"

	"llama_proxy/internal/config"

	"github.com/youcd/toolkit/log"
)
// procSlot 是单个模型进程的运行时状态。readyCh 在该模型进程就绪时关闭一次。
type procSlot struct {
	cmd           *exec.Cmd
	logFile       *os.File
	readyOK       bool
	readyCh       chan struct{}
	readyChClosed bool
}

// Scheduler 负责按请求的模型 ID 管理本地模型进程组：
// background 组成员并行常驻（独立端口），coding 模型按需独占（抢占 background 组），
// coding 空闲租约到期切回 background 组。仅在配置了 scheduling 时由 main 装配启用。
type Scheduler struct {
	cfg    *config.SchedulingConfig
	lease  time.Duration
	sw     config.SwitchConfig
	client *http.Client

	// activeCount 返回当前在途请求数，用于切换时的优雅排空。
	activeCount func() int64
	// probeInterval 就绪探活周期（测试可缩短）。
	probeInterval time.Duration
	probe         func(ctx context.Context, url string) bool

	mu sync.Mutex
	// switchMu 串行化整个状态切换过程（switchTo），防止并发切换互相覆盖进程槽位。
	switchMu sync.Mutex

	// 在途切换跟踪：switchTarget/switchMode/switchCancel 标识当前持有 switchMu 的
	// 切换，使更高优先级（coding > background）的请求可以抢占它（取消其就绪等待）
	// 而不是排在其后等待。被抢占的切换会在 waitReady 中观察到取消并释放 switchMu，
	// 随后新切换的 stopAll 会杀掉被抢占切换刚启动的进程。
	// preempted 为单调标志：一旦发生过抢占就保持为 true（唯一读者是 Start，
	// 用它判断"首次默认 background 组加载被 coding 请求抢占"这一非致命情况）。
	// 均由 s.mu 保护。
	switchTarget string // coding 切换为模型 ID；background 组切换为 "group"
	switchMode   string
	switchCancel context.CancelFunc
	preempted    bool

	procs       map[string]*procSlot // 已启动/在运行的模型进程槽位（按模型 ID）
	expected    map[string]bool      // 当前状态下应运行的模型 ID 集合
	codingModel string               // coding 态下运行的模型 ID；background 组态为空
	lastCodingAt time.Time
	started       bool
}

// NewScheduler 基于调度配置创建一个调度器。日志走全局 log（[scheduler] 前缀）。
func New(cfg *config.SchedulingConfig, lease time.Duration, sw config.SwitchConfig, client *http.Client) *Scheduler {
	return &Scheduler{
		cfg:           cfg,
		lease:         lease,
		sw:            sw,
		client:        client,
		probeInterval: 5 * time.Second,
		procs:         make(map[string]*procSlot),
		expected:      make(map[string]bool),
	}
}

// SetProbe 覆盖就绪探测函数（主要用于测试）。
func (s *Scheduler) SetProbe(f func(ctx context.Context, url string) bool) *Scheduler {
	s.probe = f
	return s
}

// SetTimings 热更新 lease 与 switch 时序参数（Loop 每轮重读 lease，下一 tick 生效）。
func (s *Scheduler) SetTimings(lease time.Duration, sw config.SwitchConfig) {
	s.mu.Lock()
	s.lease = lease
	s.sw = sw
	s.mu.Unlock()
}

// SetActiveCount 注入在途请求计数函数，用于切换时优雅排空。
func (s *Scheduler) SetActiveCount(f func() int64) *Scheduler {
	s.activeCount = f
	return s
}

// Start 启动调度器：并行加载常驻默认 background 组并等待全部就绪。未配置默认组
// （无 background 条目）时不启动本地进程，留待首个模型请求触发切换。
func (s *Scheduler) Start(ctx context.Context) error {
	s.mu.Lock()
	if s.started {
		s.mu.Unlock()
		return nil
	}
	s.started = true
	s.mu.Unlock()
	group := s.cfg.DefaultBackgroundGroup()
	if len(group) == 0 {
		log.WithCtx(ctx).Infof("[scheduler] no default background model; local processes stay idle until first model request")
		return nil
	}
	if _, err := s.switchTo(ctx, group, config.ModeBackground); err != nil {
		// 若首次默认 background 组加载期间有 coding 请求到达并抢占了它，则这不是
		// 致命错误：coding 切换已在进行，而 main.go 对 Start 的任何错误都会 Fatalf。
		if s.wasPreempted() {
			log.WithCtx(ctx).Infof("[scheduler] initial background start preempted by coding request; continuing")
			return nil
		}
		return fmt.Errorf("initial background start: %w", err)
	}
	return nil
}

// Shutdown 停止全部子进程（优雅退出时调用）。
func (s *Scheduler) Shutdown() {
	s.mu.Lock()
	slots := make([]*procSlot, 0, len(s.procs))
	for _, sl := range s.procs {
		slots = append(slots, sl)
	}
	s.mu.Unlock()
	for _, sl := range slots {
		if sl.cmd != nil && sl.cmd.Process != nil {
			_ = sl.cmd.Process.Kill()
		}
		if sl.logFile != nil {
			_ = sl.logFile.Close()
		}
	}
}

// EnsureModel 确保指定模型 ID 的进程已就绪。
//   - background 模型：background 组态下返回其就绪 channel（未就绪则等待，成员
//     进程意外退出时由探活循环自动重启该成员）；coding 态下受租约保护。
//   - coding 模型：触发 coding 态切换（停止 background 组、独占运行），返回就绪 channel。
//
// 返回的 channel 关闭即代表该模型进程可用，调用方等待该 channel。
func (s *Scheduler) EnsureModel(ctx context.Context, modelID string) (chan struct{}, error) {
	entry := s.cfg.Entry(modelID)
	if entry == nil {
		return nil, fmt.Errorf("model %q is not configured in scheduling", modelID)
	}
	if entry.Mode == config.ModeCoding {
		return s.ensureCoding(ctx, modelID)
	}
	return s.ensureBackground(ctx, modelID)
}

// ensureCoding 确保 coding 模型进程已就绪：已就绪直接返回；进程在运行但未就绪
// （启动超时后等待探活）时返回其就绪 channel；进程缺失/已退出时触发 coding 态切换。
func (s *Scheduler) ensureCoding(ctx context.Context, modelID string) (chan struct{}, error) {
	s.mu.Lock()
	sl := s.procs[modelID]
	if sl != nil && sl.cmd != nil {
		ch := sl.readyCh
		s.mu.Unlock()
		return ch, nil
	}
	s.mu.Unlock()

	// 高优先级请求抢占在途的更低优先级切换（background 组加载），先于获取 switchMu。
	s.preemptLowerPriority(ctx, config.ModeCoding)

	s.switchMu.Lock()
	defer s.switchMu.Unlock()

	// 二次检查：被抢占的 background 切换完成后，本模型可能已就绪或在途。
	s.mu.Lock()
	sl = s.procs[modelID]
	if sl != nil && sl.cmd != nil {
		ch := sl.readyCh
		s.mu.Unlock()
		return ch, nil
	}
	s.mu.Unlock()

	chs, err := s.switchTo(ctx, []string{modelID}, config.ModeCoding)
	ch := chs[modelID]
	return ch, err
}

// ensureBackground 确保 background 组成员进程已就绪：background 组态下成员缺失
// 触发整组切换、成员已退出立即重启该成员（只重启该成员）；coding 态下租约活跃
// 时跳过切换（防御性保留：代理层已对此场景返回 503）。
func (s *Scheduler) ensureBackground(ctx context.Context, modelID string) (chan struct{}, error) {
	group := s.cfg.DefaultBackgroundGroup()
	if len(group) == 0 {
		return nil, fmt.Errorf("model %q is a background model but no default background group is configured", modelID)
	}
	for _, id := range group {
		if id != modelID {
			continue
		}
		s.mu.Lock()
		coding := s.codingModel != ""
		sl := s.procs[modelID]
		s.mu.Unlock()
		// background 组态：成员在运行（就绪或待就绪）时直接等待其就绪 channel；
		// 成员已退出时立即重启该成员，其余成员不受影响。
		if !coding && sl != nil {
			if sl.cmd == nil {
				s.restartMember(ctx, modelID)
			}
			ch := sl.readyCh
			return ch, nil
		}
		// coding 态且租约仍活跃：跳过切换，避免杀掉正在使用的 coding 进程。
		if coding && s.codingLeaseActive() {
			log.WithCtx(ctx).Infof("[scheduler] skip %s switch: coding lease still active", modelID)
			ch := make(chan struct{})
			return ch, nil
		}
		break
	}

	s.switchMu.Lock()
	defer s.switchMu.Unlock()

	// 二次检查：等待 switchMu 期间状态可能已变化（如 coding 切换完成又空闲切回）。
	s.mu.Lock()
	coding := s.codingModel != ""
	sl := s.procs[modelID]
	s.mu.Unlock()
	if !coding && sl != nil && sl.cmd != nil {
		return sl.readyCh, nil
	}

	chs, err := s.switchTo(ctx, group, config.ModeBackground)
	ch := chs[modelID]
	return ch, err
}

// TouchCoding 刷新开发租约时间。每次收到 coding 模式模型流量都应调用。
func (s *Scheduler) TouchCoding() {
	s.mu.Lock()
	s.lastCodingAt = time.Now()
	s.mu.Unlock()
}

// IsCodingActive 报告当前是否处于开发状态：即正在运行 coding 模式模型进程，
// 或距离最后一次 coding 模型流量仍在租约内。
func (s *Scheduler) IsCodingActive() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.codingProcRunningLocked() {
		return true
	}
	if s.lease > 0 && !s.lastCodingAt.IsZero() && time.Since(s.lastCodingAt) < s.lease {
		return true
	}
	return false
}

// codingLeaseActive 报告 coding 租约当前是否活跃：coding 模式模型进程在运行，
// 且最后一次 coding 流量仍在租约窗口内（租约过期后允许切回 background 组）。
func (s *Scheduler) codingLeaseActive() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.codingProcRunningLocked() && s.lease > 0 &&
		!s.lastCodingAt.IsZero() && time.Since(s.lastCodingAt) < s.lease
}

// codingProcRunningLocked 报告是否有 coding 模式模型进程正在运行。
// 调用方须持有 s.mu。
func (s *Scheduler) codingProcRunningLocked() bool {
	for id, sl := range s.procs {
		if sl.cmd == nil {
			continue
		}
		if e := s.cfg.Entry(id); e != nil && e.Mode == config.ModeCoding {
			return true
		}
	}
	return false
}

// BaseURLFor 返回指定模型进程就绪时的转发基址（含 /v1 前缀）。
// 该模型进程未就绪（未运行/启动中/崩溃）时返回空串。
func (s *Scheduler) BaseURLFor(modelID string) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	sl := s.procs[modelID]
	if sl == nil || !sl.readyOK {
		return ""
	}
	if e := s.cfg.Entry(modelID); e != nil {
		return apiBaseFromReadiness(e.ReadinessURL)
	}
	return ""
}

// APIKeyFor 返回指定模型进程自身的 API Key，供代理转发时覆盖 Authorization，
// 使客户端 key 与后端 key 隔离。未配置或该模型进程未就绪时返回空串。
func (s *Scheduler) APIKeyFor(modelID string) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	sl := s.procs[modelID]
	if sl == nil || !sl.readyOK {
		return ""
	}
	if e := s.cfg.Entry(modelID); e != nil {
		return e.APIKey
	}
	return ""
}

// LocalNode 描述本地进程作为代理池动态节点的信息。
type LocalNode struct {
	BaseURL string
	ModelID string
	Weight  int
	Tags    []string
	APIKey  string
}

// LocalNodes 报告当前已就绪、可作为代理池后端节点的本地 background 成员列表：
// background 组态下每个就绪成员一个节点（并行参与负载均衡）；coding 态（本地节点
// 全部停止）或成员未就绪时无节点。
func (s *Scheduler) LocalNodes() []LocalNode {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.codingModel != "" {
		return nil
	}
	var out []LocalNode
	for id, sl := range s.procs {
		if !sl.readyOK {
			continue
		}
		e := s.cfg.Entry(id)
		if e == nil || e.Mode != config.ModeBackground {
			continue
		}
		out = append(out, LocalNode{
			BaseURL: apiBaseFromReadiness(e.ReadinessURL),
			ModelID: id,
			Weight:  e.Weight,
			Tags:    e.Tags,
			APIKey:  e.APIKey,
		})
	}
	return out
}

// apiBaseFromReadiness 从就绪探测地址推导 llama-server API 基址（scheme://host:port/v1）。
// readiness_url 可能带任意路径（如 /health、/v1/models），仅取其 origin 部分并拼上 /v1，
// 避免路径后缀污染转发地址。解析失败时返回空串。
func apiBaseFromReadiness(readinessURL string) string {
	u, err := url.Parse(strings.TrimRight(readinessURL, "/"))
	if err != nil || u.Scheme == "" || u.Host == "" {
		return ""
	}
	return u.Scheme + "://" + u.Host + "/v1"
}

