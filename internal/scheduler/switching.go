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

	// 启动目标状态的全部模型进程。
	chs := make(map[string]chan struct{}, len(ids))
	for _, id := range ids {
		entry := s.cfg.Entry(id)
		if entry == nil {
			chs[id] = make(chan struct{})
			continue
		}
		f, err := openProcessLog(entry.LogFile)
		if err != nil {
			log.WithCtx(ctx).Warnf("[scheduler] open log file %s failed: %v", entry.LogFile, err)
		}
		slot := &procSlot{port: readinessPort(entry), readyCh: make(chan struct{})}
		chs[id] = slot.readyCh
		if entry.Command != "" {
			// 启动前确认端口无残留监听者（如上一次代理进程遗留的 setsid 服务），
			// 否则新服务绑定端口会失败、启动超时卡死切换。
			s.freeOccupiedPort(ctx, slot.port, s.sw.KillTimeout)
			cmd := exec.Command("/bin/sh", "-c", entry.Command)
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
	}

	// 更新状态登记：expected 集合与 coding 态标记。
	s.mu.Lock()
	s.expected = make(map[string]bool, len(ids))
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
	resCh := make(chan result, len(ids))
	for _, id := range ids {
		entry := s.cfg.Entry(id)
		if entry == nil || entry.Command == "" {
			// 无真实命令（测试桩）：直接探测。
			resCh <- result{id, s.waitReady(switchCtx, entry.ReadinessURL, entry.APIKey, s.sw.StartupTimeout)}
			continue
		}
		go func(id string, e *config.ProcessConfig) {
			resCh <- result{id, s.waitReady(switchCtx, e.ReadinessURL, e.APIKey, s.sw.StartupTimeout)}
		}(id, entry)
	}

	var firstErr error
	for range ids {
		r := <-resCh
		if r.err == nil {
			s.markSlotReady(r.id)
		} else if firstErr == nil {
			if errors.Is(r.err, context.Canceled) {
				firstErr = fmt.Errorf("%s switch preempted by higher-priority request", r.id)
			} else {
				firstErr = fmt.Errorf("%s ready probe failed: %w", r.id, r.err)
			}
		}
	}

	s.mu.Lock()
	wasPreempted := s.preempted
	s.clearSwitchInflightLocked()
	s.mu.Unlock()
	if firstErr == nil {
		log.WithCtx(ctx).Infof("[scheduler] %s ready: %s", mode, strings.Join(ids, ","))
		return chs, nil
	}
	if wasPreempted && errors.Is(switchCtx.Err(), context.Canceled) {
		log.WithCtx(ctx).Infof("[scheduler] %s switch preempted by higher-priority request", strings.Join(ids, ","))
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
func (s *Scheduler) preemptLowerPriority(ctx context.Context, mode string) {
	s.mu.Lock()
	prev := s.switchMode
	cancel := s.switchCancel
	if cancel == nil || switchPriority(mode) <= switchPriority(prev) {
		s.mu.Unlock()
		return
	}
	s.preempted = true
	s.mu.Unlock()
	log.WithCtx(ctx).Infof("[scheduler] preempting in-flight %s switch with %s request", prev, mode)
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
	sl.readyOK = true
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

