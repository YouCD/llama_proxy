// switching.go 状态切换机制：switchTo 串行化执行"排空→停止全部→并行启动→等待就绪"，
// 并实现 coding > background 的在途切换抢占（preemptLowerPriority）与就绪登记。
package scheduler

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"time"

	"llama_proxy/internal/config"

	"github.com/youcd/toolkit/log"
)
// switchTo 把调度器切换到目标状态：停止当前全部进程，并行启动 ids 中的全部模型
// 进程，并等待各自就绪（共享一个可被抢占取消的 switchCtx）。
// 通过 switchMu 串行化整个切换过程（含 drain/停止旧进程/等待就绪），
// 防止并发切换互相覆盖进程槽位。返回每个模型的就绪 channel 与整体错误
// （任一成员启动超时/失败即报错，但各 channel 仍有效，可交由探活循环恢复）。
func (s *Scheduler) switchTo(ctx context.Context, ids []string, mode string) (map[string]chan struct{}, error) {
	// 优雅排空在途请求（被停止的进程可能仍有人在途流量）。
	s.drain()

	// 杀掉 coding 模型前先记录显存基线：进程退出后驱动异步回收显存，
	// 期间启动大显存模型会 OOM 崩溃循环，需等显存真正释放。
	gpuBaseline := gpuBaselineTotal()

	// 停止当前全部进程。
	s.stopAll(ctx)

	// 等被杀模型的显存释放完毕再启动新模型（无 GPU 时立即返回）。
	s.waitForGPURelease(ctx, gpuBaseline, 60*time.Second)

	// 启动目标状态的全部模型进程。toWait 收集需要等待就绪的成员（启动中的与已在
	// 运行的）；休眠成员被跳过——组切换不打扰休眠，仅保留 expected 登记。
	chs := make(map[string]chan struct{}, len(ids))
	var toWait []string
	for _, id := range ids {
		entry := s.cfg.Entry(id)
		if entry == nil {
			chs[id] = make(chan struct{})
			toWait = append(toWait, id)
			continue
		}
		// 已在运行的模型（coding 切换时保留的 resident 成员）：复用原槽位与就绪
		// channel，不重启、不清端口，避免误杀正在运行的常驻进程。
		s.mu.Lock()
		existing := s.procs[id]
		running := existing != nil && existing.running()
		sleeping := existing != nil && existing.sleeping
		s.mu.Unlock()
		if running {
			chs[id] = existing.readyCh
			s.mu.Lock()
			s.expected[id] = true
			s.mu.Unlock()
			toWait = append(toWait, id)
			continue
		}
		if sleeping {
			// 休眠成员：组切换不打扰它——不唤醒、不启动、不等待就绪，仅保留 expected
			// 登记。它由直接请求经 ensureBackground→restartMember 唤醒。
			s.mu.Lock()
			s.expected[id] = true
			s.mu.Unlock()
			continue
		}
		slot := s.launchSlot(ctx, id)
		chs[id] = slot.readyCh
		toWait = append(toWait, id)
	}

	// 更新状态登记：expected 集合与 coding 态标记。expected 采用合并而非清空：
	// stopAll 已保留 resident 成员的登记，这里只追加目标成员，不抹掉常驻模型。
	s.mu.Lock()
	for _, id := range ids {
		s.expected[id] = true
	}
	s.codingModel = ""
	if mode == config.ModeCoding {
		s.codingModel = ids[0]
	}
	now := time.Now()
	if mode == config.ModeCoding {
		s.lastCodingAt = now
	}
	// 等待就绪。使用 switchCtx（调用方 ctx 的子 context）：更高优先级的切换
	// 可通过 switchCancel 抢占本次切换，即使调用方本身仍在等待也能提前结束等待。
	switchCtx, switchCancel := context.WithCancel(ctx)
	s.switchTarget = describeTarget(mode, ids)
	s.switchMode = mode
	s.switchCancel = switchCancel
	s.mu.Unlock()
	defer switchCancel()

	// 并行等待各成员就绪（各自独立超时计时）。
	type result struct {
		id  string
		err error
	}
	resCh := make(chan result, len(toWait))
	for _, id := range toWait {
		entry := s.cfg.Entry(id)
		if entry == nil || entry.Command == "" {
			// 无真实命令（测试桩）：直接探测。
			resCh <- result{id, s.waitReady(switchCtx, "", entry.ReadinessURL, entry.APIKey, s.sw.StartupTimeout, false)}
			continue
		}
		go func(id string, e *config.ProcessConfig) {
			// 监控进程存活：成员启动失败（进程未就绪即退出）时快速返回，不空等
			// 整个 startup_timeout 占着 switchMu 阻塞后续切换。
			resCh <- result{id, s.waitReady(switchCtx, id, e.ReadinessURL, e.APIKey, s.sw.StartupTimeout, true)}
		}(id, entry)
	}

	var firstErr error
	var errID string
	for range toWait {
		r := <-resCh
		if r.err == nil {
			s.markSlotReady(r.id)
		} else if firstErr == nil {
			errID = r.id
			// 取消分类依据 s.preempted（在错误分类时读取，抢占可能发生在就绪等待
			// 期间）：被更高优先级切换真正抢占才叫"被抢占"；调用方 ctx 被取消
			// （如 Ctrl+C 优雅关停）时 waitReady 同样返回 context.Canceled，不能误报。
			s.mu.Lock()
			preempted := s.preempted
			s.mu.Unlock()
			if errors.Is(r.err, context.Canceled) && preempted {
				firstErr = fmt.Errorf("%s switch preempted by higher-priority request", r.id)
			} else {
				firstErr = fmt.Errorf("%s ready probe failed: %w", r.id, r.err)
			}
		}
	}

	s.mu.Lock()
	wasPreempted := s.preempted
	slotDead := errID != "" && s.slotDeadLocked(errID)
	s.clearSwitchInflightLocked()
	s.mu.Unlock()
	if firstErr == nil {
		log.WithCtx(ctx).Infof("[scheduler] %s ready: %s", mode, strings.Join(ids, ","))
		return chs, nil
	}
	if wasPreempted && errors.Is(switchCtx.Err(), context.Canceled) {
		log.WithCtx(ctx).Infof("[scheduler] %s switch preempted by higher-priority request", strings.Join(ids, ","))
	} else if slotDead {
		log.WithCtx(ctx).Warnf("[scheduler] %s startup failed: process exited before ready (failure recorded, restart backoff applied)", strings.Join(ids, ","))
	} else {
		log.WithCtx(ctx).Warnf("[scheduler] %s: %v (still running, will reconcile later)", strings.Join(ids, ","), firstErr)
	}
	return chs, firstErr
}

// freeOccupiedPort 在启动新服务前清理端口上的残留监听进程（TERM 整树，超时
// KILL），并确认监听释放。端口为 0（无法解析）或已空闲时直接返回。
func (s *Scheduler) freeOccupiedPort(ctx context.Context, port int, timeout time.Duration) {
	if port <= 0 || portListenerGone(port) {
		return
	}
	deadline := time.Now().Add(timeout)
	for {
		pid, err := resolveServerPID(port)
		if err != nil {
			return
		}
		if pid == os.Getpid() {
			// 端口属主是本进程（如就绪地址误配到代理自身端口，或测试内嵌监听）：
			// 杀掉自己会让代理/测试直接死掉，只告警不清理。
			log.WithCtx(ctx).Warnf("[scheduler] port %d is held by this process (pid %d), skipping cleanup", port, pid)
			return
		}
		log.WithCtx(ctx).Warnf("[scheduler] port %d held by leftover pid %d, killing process tree before startup", port, pid)
		group, _ := procGroupID(pid)
		_ = killTree(pid, group, syscall.SIGTERM)
		for pidAlive(pid) && !portListenerGone(port) && time.Now().Before(deadline) {
			time.Sleep(100 * time.Millisecond)
		}
		if pidAlive(pid) {
			_ = killTree(pid, group, syscall.SIGKILL)
		}
		if portListenerGone(port) || time.Now().After(deadline) {
			return
		}
	}
}

// describeTarget 生成在途切换登记的目标描述（coding 为模型 ID，background 组为 "group"）。
func describeTarget(mode string, ids []string) string {
	if mode == config.ModeCoding {
		return ids[0]
	}
	return "group"
}

// launchSlot 启动单个模型进程并登记槽位（procs/expected），返回槽位：打开日志、
// 创建槽位、清理端口残留监听者、启动包装命令并派生回收协程。调用方须持有
// switchMu（与整组切换互斥）；已在运行/休眠的槽位由调用方先行判断处理。
// 组内整组加载（switchTo）与 default 组外按需拉起（ensureOnDemandBackground）共用。
func (s *Scheduler) launchSlot(ctx context.Context, id string) *procSlot {
	e := s.cfg.Entry(id)
	slot := &procSlot{readyCh: make(chan struct{})}
	if e == nil {
		// 无配置条目（测试桩）：空槽位，直接交由探活探测。
		s.mu.Lock()
		s.procs[id] = slot
		s.expected[id] = true
		s.mu.Unlock()
		return slot
	}
	slot.port = readinessPort(e)
	f, err := openProcessLog(e.LogFile)
	if err != nil {
		log.WithCtx(ctx).Warnf("[scheduler] open log file %s failed: %v", e.LogFile, err)
	}
	slot.logFile = f
	if e.Command != "" {
		// 启动前确认端口无残留监听者（如上一次代理进程遗留的 setsid 服务），
		// 否则新服务绑定端口会失败、启动超时卡死切换。
		s.freeOccupiedPort(ctx, slot.port, s.sw.KillTimeout)
		// 加载起点：面板据此展示“加载中 Ns”实时计时与加载完成后的总耗时。
		slot.loadStartedAt = time.Now()
		cmd := exec.Command("/bin/sh", "-c", e.Command)
		if f != nil {
			cmd.Stdout = f
			cmd.Stderr = f
		}
		if err := cmd.Start(); err != nil {
			log.WithCtx(ctx).Warnf("[scheduler] start %s command failed: %v", id, err)
		} else {
			slot.cmd = cmd
			log.WithCtx(ctx).Infof("[scheduler] %s process started pid=%d log=%v", id, cmd.Process.Pid, f != nil)
			go s.reap(id, cmd)
		}
	}
	s.mu.Lock()
	s.procs[id] = slot
	s.expected[id] = true
	s.mu.Unlock()
	return slot
}

// switchPriority 返回切换模式的抢占优先级：coding > background。
func switchPriority(mode string) int {
	switch mode {
	case config.ModeCoding:
		return 2
	case config.ModeBackground:
		return 1
	default:
		return 0
	}
}

// preemptLowerPriority 取消在途的更低优先级切换（switchTarget/switchMode/switchCancel）
// 的就绪等待。在获取 switchMu 之前调用：background 组加载期间 coding 请求到达时直接
// 插队，而不是排队等 background 加载完成。被抢占的切换会在 waitReady 中观察到
// 取消，重置状态并释放 switchMu，随后本次切换正常执行（其 stopAll 会杀掉
// 被抢占切换刚启动的进程）。
func (s *Scheduler) preemptLowerPriority(ctx context.Context, mode, target string) {
	s.mu.Lock()
	prev := s.switchMode
	prevTarget := s.switchTarget
	cancel := s.switchCancel
	initialLoad := s.initialLoad
	if cancel == nil {
		s.mu.Unlock()
		return
	}
	// 特例 1：coding 切 coding 且目标模型不同时，允许抢占正在加载/等待的同级
	// coding 切换（用户连点切换或改选另一个 coding 模型时，不必等旧模型加载完，
	// 也不必等旧模型启动失败的空等结束）。
	samePriorityDifferentTarget := mode == config.ModeCoding && prev == config.ModeCoding && target != "" && target != prevTarget
	// 特例 2：Start 的一次性初始加载允许被按需 background 请求抢占：手动切换
	// 模型不必等整个初始加载（上限 startup_timeout）完成。被取消的只是就绪等待，
	// 其余成员继续在后台加载、由探活循环接管；Start 对抢占豁免 FATAL。
	initialLoadPreempt := initialLoad && mode == config.ModeBackground && prev == config.ModeBackground
	if switchPriority(mode) <= switchPriority(prev) && !samePriorityDifferentTarget && !initialLoadPreempt {
		s.mu.Unlock()
		return
	}
	s.preempted = true
	s.mu.Unlock()
	log.WithCtx(ctx).Infof("[scheduler] preempting in-flight %s switch (%s) with %s request", prev, prevTarget, mode)
	cancel()
}

// clearSwitchInflightLocked 清除在途切换登记。调用方须持有 s.mu。
func (s *Scheduler) clearSwitchInflightLocked() {
	s.switchTarget = ""
	s.switchMode = ""
	s.switchCancel = nil
}

// wasPreempted 报告是否发生过抢占。唯一读者是 Start：用它判断
// "首次默认 background 组加载被 coding 请求抢占"这一非致命情况（main.go 对 Start
// 的任何错误都会 Fatalf）。
func (s *Scheduler) wasPreempted() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.preempted
}

// markSlotReady 标记指定模型进程就绪并关闭其就绪 channel（保证只 close 一次）。
// 就绪探测成功即证明该模型的服务进程正在其端口上监听，顺势按端口解析并接管
// 真实服务进程 PID（如 setsid 派生、包装脚本已退出的 vLLM 服务）。
// 调用方自行记录日志（switchTo/reconcile 的上下文信息更完整）。
func (s *Scheduler) markSlotReady(id string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	sl := s.procs[id]
	if sl == nil || sl.readyOK {
		return
	}
	// 加载耗时：进程启动（launchSlot/restartMember 记 loadStartedAt）到就绪探测通过。
	if !sl.loadStartedAt.IsZero() {
		sl.lastLoadDuration = time.Since(sl.loadStartedAt)
	}
	sl.readyOK = true
	s.clearStartupFailLocked(id)
	// 锚定闲置时钟：从未被直接请求过的成员，以其首次就绪时刻为闲置起点，避免
	// "刚就绪即因 lastActiveAt 为零值被误判"或"永远不睡"。
	if t := s.lastActiveAt[id]; t.IsZero() {
		s.lastActiveAt[id] = time.Now()
	}
	if sl.port > 0 {
		if pid, err := resolveServerPID(sl.port); err == nil {
			sl.serverPID = pid
		}
	}
	if !sl.readyChClosed {
		sl.readyChClosed = true
		close(sl.readyCh)
	}
}

