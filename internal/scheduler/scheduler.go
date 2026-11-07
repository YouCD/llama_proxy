
package scheduler

import (
	"context"
	"errors"
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
// cmd 是包装命令进程（/bin/sh -c 启动脚本）；serverPID 是 readiness_url 端口上
// 实际监听服务进程的真实 PID——启动脚本可能以 setsid 派生独立会话的服务后自行
// 退出（如 vLLM launcher），此时 cmd 退出而 serverPID 仍存活，槽位继续视为运行中。
type procSlot struct {
	cmd           *exec.Cmd
	port          int      // readiness_url 解析出的监听端口（0 表示无法解析）
	serverPID     int      // 真实服务进程 PID（端口持有者）；0 表示未知/已退出
	logFile       *os.File
	readyOK       bool
	readyCh       chan struct{}
	readyChClosed bool
	// sleeping 标记主动休眠：成员因闲置被调度器停进程，等待直接请求经
	// ensureBackground→restartMember 唤醒。区别于崩溃：reap 观察到 sleeping 不记
	// 失败、不退避，reconcile 不自动拉起，组切换（switchTo）也不打扰它。
	sleeping bool
	// loadStartedAt 本次加载开始时刻（进程启动），用于监控面板展示加载耗时；
	// lastLoadDuration 最近一次成功加载的完整耗时（启动→就绪探测通过）。
	// 均由 s.mu 保护。
	loadStartedAt    time.Time
	lastLoadDuration time.Duration
}

// running 报告槽位是否仍有进程在运行：包装命令存活，或已接管到仍存活的真实服务。
func (sl *procSlot) running() bool {
	return sl.cmd != nil || (sl.serverPID != 0 && pidAlive(sl.serverPID))
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

	// failCount/nextRestartAt 跟踪"启动失败退避"：模型进程在未就绪前退出
	//（启动失败/崩溃）时累加计数，退避窗口内不重复拉起同一模型，防止必然失败
	// 的启动（如 vLLM 因 vm.overcommit_memory 取消启动）被请求风暴反复触发。
	// 模型就绪后清零。failBackoff 为基础退避时长（翻倍递增，上限 5min）。
	failCount     map[string]int
	nextRestartAt map[string]time.Time
	failBackoff   time.Duration

	// lastActiveAt 记录每个本地调度模型最近一次被请求（或首次就绪，由 markSlotReady
	// 锚定）的时间，用于休眠判定。inflight 记录每个本地调度模型当前在途请求数，由
	// 代理层 ModelEnter/ModelLeave 维护，休眠前要求为 0，避免停掉仍有人使用的进程。
	// no_pool 休眠模型的全部流量都是按模型 ID 的直连请求，经 TouchModel/ModelEnter
	// 完整观测，故闲置判定不失真。均由 s.mu 保护。
	lastActiveAt map[string]time.Time
	inflight     map[string]int
}

// maxStartupBackoff 是启动失败退避的上限。
const maxStartupBackoff = 5 * time.Minute

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
		failCount:     make(map[string]int),
		nextRestartAt: make(map[string]time.Time),
		failBackoff:   30 * time.Second,
		lastActiveAt:  make(map[string]time.Time),
		inflight:      make(map[string]int),
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
		// 调用方 ctx 已取消（Ctrl+C 优雅关停）：Shutdown 会杀掉正在加载的成员，
		// 首次加载被打断不是错误——main.go 对 Start 的任何错误都会 Fatalf，必须放行。
		if ctx.Err() != nil {
			log.WithCtx(ctx).Infof("[scheduler] initial background start interrupted by shutdown: %v", err)
			return nil
		}
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

// Shutdown 停止全部子进程（优雅退出时调用）。与 stopAll 相同，按端口接管真实
// 服务进程并整树终止，确保 setsid 派生的推理服务不会残留占用端口/显存。
func (s *Scheduler) Shutdown() {
	s.mu.Lock()
	slots := make([]*procSlot, 0, len(s.procs))
	for _, sl := range s.procs {
		slots = append(slots, sl)
		// 清空槽位进程引用：主动关停不是启动失败，迟到的 reap 不应记录失败。
		sl.cmd = nil
		sl.serverPID = 0
	}
	s.mu.Unlock()
	deadline := time.Now().Add(s.sw.KillTimeout)
	for _, sl := range slots {
		s.stopSlotProcess(sl, deadline)
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
	if sl != nil && sl.running() {
		ch := sl.readyCh
		s.mu.Unlock()
		return ch, nil
	}
	// 启动失败退避：模型刚因启动失败/崩溃退出且退避窗口未过时，不重复拉起
	//（避免必然失败的启动被请求风暴反复触发），直接返回明确错误。
	if s.backoffActiveLocked(modelID) {
		nt := s.nextRestartAt[modelID]
		n := s.failCount[modelID]
		s.mu.Unlock()
		return nil, fmt.Errorf("model %q failed to start %d time(s) in a row; retrying at %s",
			modelID, n, nt.Format(time.RFC3339))
	}
	s.mu.Unlock()

	// 高优先级请求抢占在途的更低优先级切换（background 组加载；或切换到其他
	// coding 模型时抢占正在加载的同级 coding 切换），先于获取 switchMu。
	s.preemptLowerPriority(ctx, config.ModeCoding, modelID)

	s.switchMu.Lock()
	defer s.switchMu.Unlock()

	// 二次检查：被抢占的 background 切换完成后，本模型可能已就绪或在途。
	s.mu.Lock()
	sl = s.procs[modelID]
	if sl != nil && sl.running() {
		ch := sl.readyCh
		s.mu.Unlock()
		return ch, nil
	}
	s.mu.Unlock()

	chs, err := s.switchTo(ctx, []string{modelID}, config.ModeCoding)
	ch := chs[modelID]
	return ch, err
}

// ensureBackground 确保 background 模型进程已就绪：default 组成员走常驻路径
//（background 组态下成员缺失触发整组切换、成员已退出立即重启该成员（只重启该
// 成员）；coding 态下租约活跃时跳过切换，防御性保留：代理层已对此场景返回 503）；
// default 组之外的成员走按需路径：不随启动加载，收到请求才单独拉起，与其余运行中
// 成员并行，不做整组切换。
func (s *Scheduler) ensureBackground(ctx context.Context, modelID string) (chan struct{}, error) {
	group := s.cfg.DefaultBackgroundGroup()
	isMember := false
	for _, id := range group {
		if id != modelID {
			continue
		}
		isMember = true
		e := s.cfg.Entry(modelID)
		s.mu.Lock()
		coding := s.codingModel != ""
		sl := s.procs[modelID]
		s.mu.Unlock()
		// 常驻模型：coding 态下同样常驻。在运行→直接返回就绪 channel；已退出→
		// 原地重启（同 background 组态），不按 coding 租约跳过切换。
		if e != nil && e.Resident && sl != nil {
			if !sl.running() {
				s.restartMember(ctx, modelID)
			}
			ch := sl.readyCh
			return ch, nil
		}
		// background 组态：成员在运行（就绪或待就绪，含已接管 setsid 服务）时直接
		// 等待其就绪 channel；成员确认退出时立即重启该成员，其余成员不受影响。
		if !coding && sl != nil {
			if !sl.running() {
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
	if !isMember {
		// default 组之外的 background：不随启动加载，收到请求才单独拉起。
		return s.ensureOnDemandBackground(ctx, modelID)
	}

	s.switchMu.Lock()
	defer s.switchMu.Unlock()

	// 二次检查：等待 switchMu 期间状态可能已变化（如 coding 切换完成又空闲切回）。
	s.mu.Lock()
	coding := s.codingModel != ""
	sl := s.procs[modelID]
	s.mu.Unlock()
	if !coding && sl != nil && sl.running() {
		return sl.readyCh, nil
	}

	chs, err := s.switchTo(ctx, group, config.ModeBackground)
	ch := chs[modelID]
	return ch, err
}

// ensureOnDemandBackground 确保 default 组之外的 background 模型进程已就绪：
// 未启动则单独拉起（与其余运行中成员并行，不做整组切换、不停其他进程）；已启动
// 直接返回其就绪 channel。拉起期间登记为在途 background 切换，可被更高优先级的
// coding 请求抢占（抢占后 coding 切换的 stopAll 会杀掉刚启动的进程，本次等待收到
// 取消错误返回）。启动失败退避策略与组内成员一致。
func (s *Scheduler) ensureOnDemandBackground(ctx context.Context, modelID string) (chan struct{}, error) {
	e := s.cfg.Entry(modelID)
	s.mu.Lock()
	sl := s.procs[modelID]
	if sl != nil && sl.running() {
		ch := sl.readyCh
		s.mu.Unlock()
		return ch, nil
	}
	// 启动失败退避：模型刚因启动失败/崩溃退出且退避窗口未过时，不重复拉起
	//（避免必然失败的启动被请求风暴反复触发），直接返回明确错误。
	if s.backoffActiveLocked(modelID) {
		nt := s.nextRestartAt[modelID]
		n := s.failCount[modelID]
		s.mu.Unlock()
		return nil, fmt.Errorf("model %q failed to start %d time(s) in a row; retrying at %s",
			modelID, n, nt.Format(time.RFC3339))
	}
	s.mu.Unlock()

	// coding 请求可在拉起期间抢占本次加载（background 优先级低于 coding）。
	s.preemptLowerPriority(ctx, config.ModeBackground, modelID)

	s.switchMu.Lock()
	defer s.switchMu.Unlock()

	// 二次检查：等待 switchMu 期间状态可能已变化（如另一请求已拉起本模型）。
	s.mu.Lock()
	sl = s.procs[modelID]
	running := sl != nil && sl.running()
	s.mu.Unlock()
	if running {
		return sl.readyCh, nil
	}

	// per-GPU 独占仲裁：同卡已有非 resident 模型在跑时先将其优雅驱逐。
	if err := s.acquireGPU(ctx, modelID); err != nil {
		return nil, err
	}

	slot := s.launchSlot(ctx, modelID)

	// 登记在途切换（可被 coding 抢占），等待就绪。
	switchCtx, switchCancel := context.WithCancel(ctx)
	s.mu.Lock()
	s.switchTarget = modelID
	s.switchMode = config.ModeBackground
	s.switchCancel = switchCancel
	s.mu.Unlock()
	defer func() {
		switchCancel()
		s.mu.Lock()
		s.clearSwitchInflightLocked()
		s.mu.Unlock()
	}()

	if err := s.waitReady(switchCtx, modelID, e.ReadinessURL, e.APIKey, s.sw.StartupTimeout, true); err != nil {
		s.mu.Lock()
		preempted := s.preempted
		s.mu.Unlock()
		if errors.Is(err, context.Canceled) && preempted {
			log.WithCtx(ctx).Infof("[scheduler] on-demand start of %s preempted by higher-priority request", modelID)
		} else if errors.Is(err, context.Canceled) {
			// 调用方 ctx 取消（优雅关停）：与抢占区分，避免日志误报。
			log.WithCtx(ctx).Infof("[scheduler] on-demand start of %s canceled (shutdown in progress)", modelID)
		} else {
			// 进程可能仍在加载（如启动超时）：槽位仍在 procs/expected 中，
			// reconcile 探活循环就绪后会接管并关闭 readyCh。
			log.WithCtx(ctx).Warnf("[scheduler] on-demand start of %s failed: %v", modelID, err)
		}
		return slot.readyCh, err
	}
	s.markSlotReady(modelID)
	log.WithCtx(ctx).Infof("[scheduler] %s ready (on-demand, outside default group)", modelID)
	return slot.readyCh, nil
}

// acquireGPU 在启动模型 id 前做 per-GPU 独占仲裁：id 声明了 gpu 且同卡上已有
// 另一个模型 O 在运行/加载中时，先把 O 优雅驱逐（排空其在途请求、杀进程、
// 等显存释放），O 的槽位置为休眠态（reconcile 不自动拉起，避免与新模型互相
// 复活的崩溃循环），随后调用方继续加载 id。O 为 resident 时拒绝换入——常驻
// 模型独占该卡显存，杀掉会破坏全程在线契约。未声明 gpu 的模型恒放行。
// 调用方须持有 switchMu（与整组切换/其他单模型拉起互斥）。
func (s *Scheduler) acquireGPU(ctx context.Context, id string) error {
	e := s.cfg.Entry(id)
	if e == nil || e.GPU == "" {
		return nil
	}
	s.mu.Lock()
	var owner string
	var ownerSlot *procSlot
	for oid, sl := range s.procs {
		if oid == id || sl == nil || !sl.running() {
			continue
		}
		if oe := s.cfg.Entry(oid); oe != nil && oe.GPU == e.GPU {
			owner, ownerSlot = oid, sl
			break
		}
	}
	s.mu.Unlock()
	if owner == "" {
		return nil
	}
	if oe := s.cfg.Entry(owner); oe.Resident {
		return fmt.Errorf("gpu %s 被常驻模型 %s 独占（全程在线不可抢占），模型 %s 无法加载", e.GPU, owner, id)
	}
	log.WithCtx(ctx).Infof("[scheduler] gpu %s busy with %s: evicting to load %s", e.GPU, owner, id)
	// 优雅排空 owner 的在途请求（上限 drain_timeout，超时强制停止，同整组切换语义）。
	if s.sw.DrainTimeout > 0 {
		deadline := time.Now().Add(s.sw.DrainTimeout)
		for {
			s.mu.Lock()
			n := s.inflight[owner]
			s.mu.Unlock()
			if n == 0 || time.Now().After(deadline) {
				break
			}
			time.Sleep(100 * time.Millisecond)
		}
	}
	// 杀前读显存基线：驱动异步回收显存慢于进程退出，等释放完再启动新模型，
	// 否则大显存模型加载会 OOM 崩溃循环。
	gpuBaseline := gpuBaselineTotal()
	s.suspendSlot(ctx, owner, ownerSlot)
	s.waitForGPURelease(ctx, gpuBaseline, 60*time.Second)
	return nil
}

// TouchCoding 刷新开发租约时间。每次收到 coding 模式模型流量都应调用。
func (s *Scheduler) TouchCoding() {
	s.mu.Lock()
	s.lastCodingAt = time.Now()
	s.mu.Unlock()
}

// TouchModel 刷新指定模型最近一次被请求的时间。代理层在命中本地 background 调度
// 模型时调用，用于休眠判定（距最近一次请求超过 sleep_timeout 且无在途请求才睡）。
func (s *Scheduler) TouchModel(modelID string) {
	s.mu.Lock()
	s.lastActiveAt[modelID] = time.Now()
	s.mu.Unlock()
}

// ModelEnter 把指定模型的在途请求计数 +1。代理层转发到本地调度模型前调用。
func (s *Scheduler) ModelEnter(modelID string) {
	s.mu.Lock()
	s.inflight[modelID]++
	s.mu.Unlock()
}

// ModelLeave 把指定模型的在途请求计数 -1（下限 0）。代理层请求结束时调用。
func (s *Scheduler) ModelLeave(modelID string) {
	s.mu.Lock()
	if s.inflight[modelID] > 0 {
		s.inflight[modelID]--
	}
	s.mu.Unlock()
}

// idleForSleep 报告指定模型是否闲置到可休眠：无在途请求，且距最近一次请求（或
// 首次就绪）已超过 timeout。
func (s *Scheduler) idleForSleep(id string, timeout time.Duration) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.inflight[id] > 0 {
		return false
	}
	last := s.lastActiveAt[id]
	return !last.IsZero() && time.Since(last) > timeout
}

// IsCodingActive 报告当前是否处于开发状态：coding 模式模型进程正在运行
//（服务中或加载中）。进程已退出时返回 false——死亡的 coding 进程不占用
// background 模型，也不应让 background 请求在租约窗口内持续被 503 拒绝；
// 租约到期后的自动切回由 Loop 的租约分支负责。
func (s *Scheduler) IsCodingActive() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.codingProcRunningLocked()
}

// codingSlotReadyLocked 报告当前 coding 模型槽位是否已就绪。调用方须持有 s.mu。
func (s *Scheduler) codingSlotReadyLocked() bool {
	if s.codingModel == "" {
		return false
	}
	sl := s.procs[s.codingModel]
	return sl != nil && sl.readyOK
}

// slotDeadLocked 报告槽位进程是否已确认退出：槽位被移除（切换已接管），或
// 包装命令与接管的真实服务都不在运行。调用方须持有 s.mu。
func (s *Scheduler) slotDeadLocked(id string) bool {
	sl := s.procs[id]
	return sl == nil || !sl.running()
}

// recordStartupFailLocked 记录一次进程退出（未就绪即退出或就绪后崩溃），
// 返回下次允许重启的时间：第 1 次允许立即重试，之后按 failBackoff 翻倍
// 退避、上限 maxStartupBackoff。调用方须持有 s.mu。
func (s *Scheduler) recordStartupFailLocked(id string) time.Time {
	s.failCount[id]++
	n := s.failCount[id]
	if n <= 1 {
		return time.Time{}
	}
	backoff := s.failBackoff
	for i := 1; i < n-1 && backoff < maxStartupBackoff; i++ {
		backoff *= 2
	}
	if backoff > maxStartupBackoff {
		backoff = maxStartupBackoff
	}
	nt := time.Now().Add(backoff)
	s.nextRestartAt[id] = nt
	return nt
}

// clearStartupFailLocked 模型就绪后清零失败计数与退避。调用方须持有 s.mu。
func (s *Scheduler) clearStartupFailLocked(id string) {
	delete(s.failCount, id)
	delete(s.nextRestartAt, id)
}

// backoffActiveLocked 报告指定模型是否仍处于启动失败退避窗口内。调用方须持有 s.mu。
func (s *Scheduler) backoffActiveLocked(id string) bool {
	nt, ok := s.nextRestartAt[id]
	return ok && time.Now().Before(nt)
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
		if !sl.running() {
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
		// no_pool 模型不加入代理池（仅按模型 ID 直连），不返回为池节点。
		if e.NoPool {
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

