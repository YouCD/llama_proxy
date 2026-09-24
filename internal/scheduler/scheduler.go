// Package scheduler 负责按请求的模型 ID 拉起/切换 llama.cpp 模型进程。
package scheduler

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"llama_proxy/internal/config"

	"github.com/youcd/toolkit/log"
)

const (
	modeCoding     = "coding"
	modeBackground = "background"
)

// Scheduler 负责根据请求的模型 ID 启动/切换 llama.cpp 模型进程：
// 请求命中 cfg.Coding.Model 时切换/启动 coding 进程，命中 cfg.Background.Model 时
// 确保 background 进程就绪。仅在配置了 scheduling 时由 main 装配启用；未启用时为 nil，
// 代理退化为纯转发。
type Scheduler struct {
	cfg    *config.SchedulingConfig
	lease  time.Duration
	sw     config.SwitchConfig
	client *http.Client

	// activeCount 返回当前在途请求数，用于切换时的优雅排空。
	activeCount func() int64
	probe       func(ctx context.Context, url string) bool

	mu sync.Mutex
	// switchMu 串行化整个进程切换过程（ensureTarget），防止并发切换覆盖 readyCh，
	// 导致等待者持有的旧 channel 永不关闭。
	switchMu sync.Mutex

	// 在途切换跟踪：某个切换持有 switchMu 期间，switchTarget/switchCancel 标识它，
	// 使更高优先级的请求可以抢占它（取消其就绪等待）而不是排在其后等待。
	// preempted 为单调标志：一旦发生过抢占就保持为 true（唯一读者是 Start，
	// 用它判断“首次 background 加载被 coding 请求抢占”这一非致命情况）。
	// 均由 s.mu 保护。
	switchTarget string
	switchCancel context.CancelFunc
	preempted    bool

	mode          string
	readyOK       bool
	readyCh       chan struct{}
	readyChClosed bool
	cmd           *exec.Cmd
	logFile       *os.File
	lastCodingAt  time.Time
	started       bool
}

// NewScheduler 基于调度配置创建一个调度器。日志走全局 log（[scheduler] 前缀）。
func New(cfg *config.SchedulingConfig, lease time.Duration, sw config.SwitchConfig, client *http.Client) *Scheduler {
	return &Scheduler{
		cfg:    cfg,
		lease:  lease,
		sw:     sw,
		client: client,
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

// Start 启动调度器：首次拉起 background 进程并等待就绪。
func (s *Scheduler) Start(ctx context.Context) error {
	s.mu.Lock()
	if s.started {
		s.mu.Unlock()
		return nil
	}
	s.started = true
	s.mu.Unlock()
	if _, err := s.ensureTarget(ctx, modeBackground, s.cfg.Background.Command, s.cfg.Background.ReadinessURL, s.cfg.Background.APIKey, s.cfg.Background.LogFile); err != nil {
		// 若首次 background 加载期间有 coding 请求到达并抢占了它，则这不是致命错误：
		// coding 切换已在进行，而 main.go 对 Start 的任何错误都会 Fatalf 杀掉代理。
		if s.wasPreempted() {
			log.WithCtx(nil).Infof("[scheduler] initial background start preempted by coding request; continuing")
			return nil
		}
		return fmt.Errorf("initial background start: %w", err)
	}
	return nil
}

// Shutdown 停止当前运行的子进程（优雅退出时调用）。
func (s *Scheduler) Shutdown() {
	s.mu.Lock()
	cmd := s.cmd
	logFile := s.logFile
	s.logFile = nil
	s.mu.Unlock()
	if cmd != nil && cmd.Process != nil {
		_ = cmd.Process.Kill()
	}
	if logFile != nil {
		_ = logFile.Close()
	}
}

// EnsureCoding 确保 coding 进程已就绪。若当前不在 coding 模式或未就绪则触发切换，
// 并返回一个就绪信号 channel：channel 关闭即代表 coding 进程可用。调用方等待该 channel。
// 并发调用会复用同一次切换，避免重复启动进程。
func (s *Scheduler) EnsureCoding(ctx context.Context) (chan struct{}, error) {
	s.mu.Lock()
	if s.mode == modeCoding && s.readyOK {
		ch := s.readyCh
		s.mu.Unlock()
		return ch, nil
	}
	// 已处于切换过程中的请求直接等待同一个 channel。
	if s.mode == modeCoding && !s.readyOK && s.readyCh != nil {
		ch := s.readyCh
		s.mu.Unlock()
		return ch, nil
	}
	s.mu.Unlock()

	ready, err := s.ensureTarget(ctx, modeCoding, s.cfg.Coding.Command, s.cfg.Coding.ReadinessURL, s.cfg.Coding.APIKey, s.cfg.Coding.LogFile)
	return ready, err
}

// EnsureBackground 确保 background 进程已就绪（请求 background 固化模型 ID 时使用）。
// 若当前不在 background 模式或未就绪则触发切换，并返回就绪信号 channel。
// 注意：coding 租约仍活跃时 background 切换会被跳过（保护正在使用的 coding 进程），
// 调用方应先检查 IsCodingActive/BackgroundReady。
func (s *Scheduler) EnsureBackground(ctx context.Context) (chan struct{}, error) {
	return s.ensureTarget(ctx, modeBackground, s.cfg.Background.Command, s.cfg.Background.ReadinessURL, s.cfg.Background.APIKey, s.cfg.Background.LogFile)
}

// TouchCoding 刷新开发租约时间。每次收到 coding 模型流量都应调用。
func (s *Scheduler) TouchCoding() {
	s.mu.Lock()
	s.lastCodingAt = time.Now()
	s.mu.Unlock()
}

// IsCodingActive 报告当前是否处于开发状态：即正在 coding 模式，或距离最后一次 coding 模型流量仍在租约内。
func (s *Scheduler) IsCodingActive() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.mode == modeCoding {
		return true
	}
	if s.lease > 0 && !s.lastCodingAt.IsZero() && time.Since(s.lastCodingAt) < s.lease {
		return true
	}
	return false
}

// ActiveBaseURL 返回当前已就绪模式的转发基址（含 /v1 前缀），用于代理转发。
// 若当前无就绪进程则返回空串。
func (s *Scheduler) ActiveBaseURL() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.readyOK {
		return ""
	}
	if s.mode == modeCoding {
		return apiBaseFromReadiness(s.cfg.Coding.ReadinessURL)
	}
	return apiBaseFromReadiness(s.cfg.Background.ReadinessURL)
}

// ActiveAPIKey 返回当前已就绪模式进程自身的 API Key，供代理转发时覆盖 Authorization，
// 使客户端 key 与后端 key 隔离。未配置或当前无就绪进程时返回空串。
func (s *Scheduler) ActiveAPIKey() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.readyOK {
		return ""
	}
	if s.mode == modeCoding {
		return s.cfg.Coding.APIKey
	}
	return s.cfg.Background.APIKey
}

// BackgroundReady 报告 background 进程当前是否就绪、可作为代理池中的后端节点，
// 就绪时返回对应的转发基址（含 /v1 前缀）。coding 模式或未就绪时 ok 为 false。
func (s *Scheduler) BackgroundReady() (string, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.mode == modeBackground && s.readyOK {
		return apiBaseFromReadiness(s.cfg.Background.ReadinessURL), true
	}
	return "", false
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

// Loop 运行租约超时检查与周期性就绪探活：距最后一次 coding 模型流量超过 lease 则切回
// background；同时每 5s 对当前模式进程做就绪探测，恢复因启动超时误标为未就绪的进程。
func (s *Scheduler) Loop(ctx context.Context) {
	s.mu.Lock()
	lease := s.lease
	s.mu.Unlock()
	if lease <= 0 {
		return
	}
	probeTicker := time.NewTicker(5 * time.Second)
	defer probeTicker.Stop()
	for {
		// lease 支持热更新（SetTimings），每轮按当前值重建定时器；
		// 运行中从 0 改为 >0 不生效（Loop 已退出），属边界情况，需重启。
		s.mu.Lock()
		lease = s.lease
		s.mu.Unlock()
		leaseTimer := time.NewTimer(lease)
		select {
		case <-ctx.Done():
			leaseTimer.Stop()
			return
		case <-probeTicker.C:
			leaseTimer.Stop()
			s.reconcileReady()
		case <-leaseTimer.C:
			s.mu.Lock()
			idle := s.mode == modeCoding && !s.lastCodingAt.IsZero() && time.Since(s.lastCodingAt) >= s.lease
			s.mu.Unlock()
			if idle {
				log.WithCtx(nil).Infof("[scheduler] coding idle timeout reached, switching to background")
				_, _ = s.ensureTarget(context.Background(), modeBackground, s.cfg.Background.Command, s.cfg.Background.ReadinessURL, s.cfg.Background.APIKey, s.cfg.Background.LogFile)
			}
		}
	}
}

// ensureTarget 将当前进程切换到 target 模式并等待就绪。
// 通过 switchMu 串行化整个切换过程（含 drain/停止旧进程/等待就绪），
// 防止并发切换互相覆盖 readyCh 导致等待者永久阻塞。
// 优先级：coding > background。在获取 switchMu 之前，若当前已有更低优先级的
// 切换在途（switchTarget），则取消其就绪等待（switchCancel），使本次切换可以
// 插队而不是排队等它跑完。这样 coding 请求在 background 大模型加载期间到达时
// 可以立即打断加载：两者共用同一端口，background 本来就要先被杀掉才能加载
// coding，等它加载完纯属浪费（最坏可达 2*startup_timeout 的纯延迟）。
func (s *Scheduler) ensureTarget(ctx context.Context, target, command, readinessURL, apiKey, logFile string) (chan struct{}, error) {
	// coding 请求抢占在途的 background 切换（加载或切回），先于获取 switchMu。
	s.preemptLowerPriority(target)

	// 串行化切换：同一时刻只允许一个切换在执行，避免并发 ensureTarget 覆盖 readyCh。
	s.switchMu.Lock()
	defer s.switchMu.Unlock()

	// 二次检查，避免竞态下重复切换。
	s.mu.Lock()
	if s.mode == target && s.readyOK {
		ch := s.readyCh
		s.mu.Unlock()
		return ch, nil
	}
	s.mu.Unlock()

	// background 切换且 coding 租约仍活跃：跳过切换，避免杀掉已就绪/正在使用的
	// coding 进程。覆盖启动竞态：coding 请求先抢到首次切换并完成，随后
	// Start 的 background 切换不应把它杀掉（与上面的抢占互为镜像）。
	if target == modeBackground {
		s.mu.Lock()
		codingActive := s.mode == modeCoding && s.lease > 0 &&
			!s.lastCodingAt.IsZero() && time.Since(s.lastCodingAt) < s.lease
		s.mu.Unlock()
		if codingActive {
			log.WithCtx(nil).Infof("[scheduler] skip background switch: coding lease still active")
			ch := s.getReadyCh()
			if ch == nil {
				ch = make(chan struct{})
				s.mu.Lock()
				if s.readyCh == nil {
					s.readyCh = ch
				}
				s.mu.Unlock()
			}
			return ch, nil
		}
	}

	log.WithCtx(nil).Infof("[scheduler] switching to %s", target)

	// 优雅排空在途请求。
	s.drain()

	// 停止旧进程。
	s.stopProcess()

	// 打开新进程的日志文件（若配置了）。
	f, err := openProcessLog(logFile)
	if err != nil {
		log.WithCtx(nil).Warnf("[scheduler] open log file %s failed: %v", logFile, err)
	}
	if f != nil {
		s.mu.Lock()
		s.logFile = f
		s.mu.Unlock()
	}

	newReady := make(chan struct{})
	s.mu.Lock()
	s.mode = target
	s.readyOK = false
	s.readyCh = newReady
	s.readyChClosed = false
	s.mu.Unlock()

	if command != "" {
		cmd := exec.Command("/bin/sh", "-c", command)
		if f != nil {
			cmd.Stdout = f
			cmd.Stderr = f
		}
		if err := cmd.Start(); err != nil {
			log.WithCtx(nil).Warnf("[scheduler] start %s command failed: %v", target, err)
			s.mu.Lock()
			s.clearSwitchInflightLocked()
			s.mu.Unlock()
			s.markReadyClosed(newReady)
			return newReady, err
		}
		log.WithCtx(nil).Infof("[scheduler] %s process started pid=%d log=%v", target, cmd.Process.Pid, f != nil)
		s.mu.Lock()
		s.cmd = cmd
		s.mu.Unlock()
		go s.reap(cmd)
	}

	// 等待就绪。使用 switchCtx（调用方 ctx 的子 context）：更高优先级的切换
	// 可通过 switchCancel 抢占本次切换，即使调用方本身仍在等待也能提前结束等待。
	switchCtx, switchCancel := context.WithCancel(ctx)
	s.mu.Lock()
	s.switchTarget = target
	s.switchCancel = switchCancel
	s.mu.Unlock()
	defer switchCancel()

	if err := s.waitReady(switchCtx, readinessURL, apiKey, s.sw.StartupTimeout); err != nil {
		s.mu.Lock()
		s.readyOK = false
		s.clearSwitchInflightLocked()
		wasPreempted := s.preempted
		s.mu.Unlock()
		if wasPreempted && errors.Is(switchCtx.Err(), context.Canceled) {
			log.WithCtx(nil).Infof("[scheduler] %s switch preempted by higher-priority request", target)
			return newReady, fmt.Errorf("%s switch preempted by higher-priority request", target)
		}
		log.WithCtx(nil).Warnf("[scheduler] %s ready probe failed (will reconcile later): %v", target, err)
		// 失败时不再关闭 newReady，交由 reconcileReady 在进程真正就绪后放行等待者。
		return newReady, err
	}

	s.mu.Lock()
	s.readyOK = true
	now := time.Now()
	if target == modeCoding {
		s.lastCodingAt = now
	}
	s.clearSwitchInflightLocked()
	s.mu.Unlock()
	log.WithCtx(nil).Infof("[scheduler] %s ready", target)
	s.markReadyClosed(newReady)
	return newReady, nil
}

// switchPriority 返回切换目标的抢占优先级：coding > background。
func switchPriority(target string) int {
	switch target {
	case modeCoding:
		return 2
	case modeBackground:
		return 1
	default:
		return 0
	}
}

// preemptLowerPriority 取消在途的更低优先级切换（switchTarget/switchCancel）的
// 就绪等待。在获取 switchMu 之前调用：background 加载期间 coding 请求到达时直接
// 插队，而不是排队等 background 加载完成。被抢占的切换会在 waitReady 中观察到
// 取消，重置状态并释放 switchMu，随后本次切换正常执行（其 stopProcess 会杀掉
// 被抢占切换刚启动的进程）。
func (s *Scheduler) preemptLowerPriority(target string) {
	s.mu.Lock()
	prev := s.switchTarget
	cancel := s.switchCancel
	if cancel == nil || switchPriority(target) <= switchPriority(prev) {
		s.mu.Unlock()
		return
	}
	s.preempted = true
	s.mu.Unlock()
	log.WithCtx(nil).Infof("[scheduler] preempting in-flight %s switch with %s request", prev, target)
	cancel()
}

// clearSwitchInflightLocked 清除在途切换登记。调用方须持有 s.mu。
func (s *Scheduler) clearSwitchInflightLocked() {
	s.switchTarget = ""
	s.switchCancel = nil
}

// wasPreempted 报告是否发生过抢占。唯一读者是 Start：用它判断
// “首次 background 加载被 coding 请求抢占”这一非致命情况（main.go 对 Start
// 的任何错误都会 Fatalf）。
func (s *Scheduler) wasPreempted() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.preempted
}

// markReadyClosed 持锁关闭 ready channel 并记录状态。
// 仅当 ch 仍是当前就绪 channel（s.readyCh）时才关闭，且保证只 close 一次：
//   - 并发 close 同一 channel 不会 panic；
//   - 被新切换覆盖的旧 channel 不再关闭，避免其"已关闭"标志吞掉新 channel 的关闭，
//     导致等待新 channel 的请求永久阻塞。
func (s *Scheduler) markReadyClosed(ch chan struct{}) {
	if ch == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.readyCh != ch {
		return
	}
	if s.readyChClosed {
		return
	}
	s.readyChClosed = true
	close(ch)
}

// reconcileReady 周期性检查当前模式进程的就绪状态，用于修复"大模型启动慢导致
// 首次 waitReady 超时后进程实际就绪却一直标记为未就绪"的问题。就绪时恢复
// readyOK 并放行等待就绪信号的调用方。
func (s *Scheduler) reconcileReady() {
	s.mu.Lock()
	if s.readyOK || s.cmd == nil {
		s.mu.Unlock()
		return
	}
	mode := s.mode
	ch := s.readyCh
	var baseURL, apiKey string
	if mode == modeCoding {
		baseURL = s.cfg.Coding.ReadinessURL
		apiKey = s.cfg.Coding.APIKey
	} else {
		baseURL = s.cfg.Background.ReadinessURL
		apiKey = s.cfg.Background.APIKey
	}
	s.mu.Unlock()

	if baseURL == "" {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if s.probeReady(ctx, baseURL, apiKey) {
		s.mu.Lock()
		if !s.readyOK {
			s.readyOK = true
			log.WithCtx(nil).Infof("[scheduler] %s became ready (reconciled)", mode)
			s.mu.Unlock()
			s.markReadyClosed(ch)
		} else {
			s.mu.Unlock()
		}
	}
}

// openProcessLog 以追加模式打开进程日志文件；logFile 为空时返回 (nil, nil)。
func openProcessLog(logFile string) (*os.File, error) {
	if logFile == "" {
		return nil, nil
	}
	if dir := filepath.Dir(logFile); dir != "." && dir != "" {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return nil, err
		}
	}
	return os.OpenFile(logFile, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
}

// drain 等待在途请求排空，受 drain_timeout 限制。
func (s *Scheduler) drain() {
	if s.activeCount == nil || s.sw.DrainTimeout <= 0 {
		return
	}
	deadline := time.Now().Add(s.sw.DrainTimeout)
	for {
		if s.activeCount() <= 0 {
			return
		}
		if time.Now().After(deadline) {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// stopProcess 停止当前子进程，受 kill_timeout 限制。
func (s *Scheduler) stopProcess() {
	s.mu.Lock()
	cmd := s.cmd
	s.cmd = nil
	logFile := s.logFile
	s.logFile = nil
	s.mu.Unlock()

	if cmd != nil && cmd.Process != nil {
		done := make(chan struct{})
		go func() {
			_ = cmd.Process.Kill()
			_, _ = cmd.Process.Wait()
			close(done)
		}()
		select {
		case <-done:
		case <-time.After(s.sw.KillTimeout):
		}
	}
	if logFile != nil {
		_ = logFile.Close()
	}
	log.WithCtx(nil).Infof("[scheduler] stopped previous process")
}

// reap 回收已退出的子进程，防止僵尸进程。进程意外退出时清除就绪标记，
// 使本地 background 节点及时退出代理池（正常切换时 stopProcess 已置 s.cmd=nil，不会误清）。
func (s *Scheduler) reap(cmd *exec.Cmd) {
	_ = cmd.Wait()
	s.mu.Lock()
	if s.cmd == cmd {
		s.readyOK = false
	}
	s.mu.Unlock()
}

// waitReady 轮询配置的 readinessURL 本身，直到成功或超时。
func (s *Scheduler) waitReady(ctx context.Context, baseURL, apiKey string, timeout time.Duration) error {
	if baseURL == "" {
		return fmt.Errorf("readiness_url is empty")
	}
	probeCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	ticker := time.NewTicker(500 * time.Millisecond)
	defer ticker.Stop()
	for {
		if s.probeReady(probeCtx, baseURL, apiKey) {
			return nil
		}
		select {
		case <-probeCtx.Done():
			return probeCtx.Err()
		case <-ticker.C:
		}
	}
}

func (s *Scheduler) probeReady(ctx context.Context, url, apiKey string) bool {
	if s.probe != nil {
		return s.probe(ctx, url)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return false
	}
	if apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+apiKey)
	}
	resp, err := s.client.Do(req)
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	return resp.StatusCode >= 200 && resp.StatusCode < 500
}

// getReadyCh 返回当前就绪信号（测试/状态查询用）。
func (s *Scheduler) getReadyCh() chan struct{} {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.readyCh
}

// Snapshot 为调度器内部状态的只读快照，供监控面板展示。
type Snapshot struct {
	Mode         string
	Ready        bool
	LastCodingAt time.Time
	PID          int
	Lease        time.Duration
}

// Snapshot 返回当前调度器状态快照。
func (s *Scheduler) Snapshot() Snapshot {
	s.mu.Lock()
	defer s.mu.Unlock()
	snap := Snapshot{Mode: s.mode, Ready: s.readyOK, LastCodingAt: s.lastCodingAt, Lease: s.lease}
	if s.cmd != nil && s.cmd.Process != nil {
		snap.PID = s.cmd.Process.Pid
	}
	return snap
}
