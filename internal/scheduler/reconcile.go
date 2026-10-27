// reconcile.go 探活循环与成员级崩溃恢复：Loop 周期性驱动 reconcile——background
// 组成员进程意外退出时只重启该成员（restartMember），未就绪进程探测成功即标记就绪；
// 同时检查 coding 空闲租约，到期切回默认 background 组。
package scheduler

import (
	"context"
	"os/exec"
	"time"

	"llama_proxy/internal/config"

	"github.com/youcd/toolkit/log"
)
// Loop 运行就绪探活与成员级崩溃恢复：每 probeInterval 检查全部在运行进程——
// background 组成员进程意外退出时只重启该成员（其余成员继续服务）；未就绪进程
// 探测成功即标记就绪。coding 进程退出不自动重启（下次 coding 请求重新加载）。
// 同时检查 coding 空闲租约：到期则切回默认 background 组。
func (s *Scheduler) Loop(ctx context.Context) {
	s.mu.Lock()
	lease := s.lease
	s.mu.Unlock()
	if lease <= 0 {
		return
	}
	probeTicker := time.NewTicker(s.probeInterval)
	defer probeTicker.Stop()
	// 租约定时器必须跨轮次持久化：若每轮重建，探活 ticker 会先于任何 >probeInterval
	// 的租约定时器触发并 Stop 掉它，导致 idle 切回 background 的分支永不执行。
	// lease 支持热更新（SetTimings）：值变化时重建；运行中从 0 改为 >0 不生效
	//（Loop 已退出），属边界情况，需重启。
	var (
		leaseTimer *time.Timer
		curLease   time.Duration
	)
	for {
		s.mu.Lock()
		lease = s.lease
		s.mu.Unlock()
		if leaseTimer == nil || lease != curLease {
			if leaseTimer != nil {
				leaseTimer.Stop()
			}
			leaseTimer = time.NewTimer(lease)
			curLease = lease
		}
		select {
		case <-ctx.Done():
			leaseTimer.Stop()
			return
		case <-probeTicker.C:
			// 探活不触碰 leaseTimer，租约到期检查不被打断。
			s.reconcile(ctx)
		case <-leaseTimer.C:
			leaseTimer = nil // 下轮按当前 lease 重建，支持热更新后的新值。
			s.mu.Lock()
			idle := s.codingProcRunningLocked() && s.lease > 0 && !s.lastCodingAt.IsZero() && time.Since(s.lastCodingAt) >= s.lease
			codingModel := s.codingModel
			s.mu.Unlock()
			if idle {
				log.WithCtx(ctx).Infof("[scheduler] coding model %s idle timeout reached, switching to default background group", codingModel)
				group := s.cfg.DefaultBackgroundGroup()
				if len(group) > 0 {
					// 用 Loop 的 ctx：关停时取消进行中的空闲切换，不启动新模型加载。
					_, _ = s.switchTo(ctx, group, config.ModeBackground)
				} else {
					log.WithCtx(ctx).Warnf("[scheduler] no default background configured; keeping current coding process")
				}
			}
		}
	}
}

// reconcile 周期性检查全部在运行进程：background 组成员进程意外退出时只重启该
// 成员（其余成员继续服务）；未就绪进程探测成功即标记就绪并放行等待者。
// coding 进程退出不自动重启（下次 coding 请求重新触发切换）。
func (s *Scheduler) reconcile(ctx context.Context) {
	type snap struct {
		id   string
		mode string
		sl   *procSlot
	}
	s.mu.Lock()
	var items []snap
	for id, sl := range s.procs {
		e := s.cfg.Entry(id)
		if e == nil {
			continue
		}
		items = append(items, snap{id: id, mode: e.Mode, sl: sl})
	}
	s.mu.Unlock()

	for _, it := range items {
		if it.sl.cmd == nil {
			// 进程已退出：只重启 background 组成员（coding 进程由请求驱动重新加载）。
			if it.mode == config.ModeBackground {
				log.WithCtx(ctx).Warnf("[scheduler] background member %s process exited, restarting member only", it.id)
				s.restartMember(ctx, it.id)
			}
			continue
		}
		if it.sl.readyOK {
			continue
		}
		e := s.cfg.Entry(it.id)
		if e == nil || e.ReadinessURL == "" {
			continue
		}
		probeCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
		if s.probeReady(probeCtx, e.ReadinessURL, e.APIKey) {
			cancel()
			s.markSlotReady(it.id)
			log.WithCtx(ctx).Infof("[scheduler] model %s became ready (reconciled)", it.id)
			continue
		}
		cancel()
	}
}

// restartMember 原地重启已退出的 background 组成员进程：复用同一就绪 channel
// （等待中的请求在进程真正就绪后被放行），不触碰其余成员。
// 持 switchMu 与状态切换互斥；重启前二次确认成员仍处于退出状态。
func (s *Scheduler) restartMember(ctx context.Context, id string) {
	s.switchMu.Lock()
	defer s.switchMu.Unlock()
	s.mu.Lock()
	sl := s.procs[id]
	e := s.cfg.Entry(id)
	needRestart := sl != nil && sl.cmd == nil && e != nil &&
		e.Mode == config.ModeBackground && s.expected[id] && e.Command != ""
	if !needRestart {
		s.mu.Unlock()
		return
	}
	// 清理旧日志文件，重新以追加模式打开。
	if sl.logFile != nil {
		_ = sl.logFile.Close()
		sl.logFile = nil
	}
	s.mu.Unlock()

	f, err := openProcessLog(e.LogFile)
	if err != nil {
		log.WithCtx(ctx).Warnf("[scheduler] reopen log file %s failed: %v", e.LogFile, err)
	}
	cmd := exec.Command("/bin/sh", "-c", e.Command)
	if f != nil {
		cmd.Stdout = f
		cmd.Stderr = f
	}
	if err := cmd.Start(); err != nil {
		log.WithCtx(ctx).Warnf("[scheduler] restart %s command failed: %v", id, err)
		s.mu.Lock()
		if sl.logFile != nil {
			_ = sl.logFile.Close()
			sl.logFile = nil
		}
		s.mu.Unlock()
		return
	}
	log.WithCtx(ctx).Infof("[scheduler] %s process restarted pid=%d log=%v", id, cmd.Process.Pid, f != nil)
	s.mu.Lock()
	sl.cmd = cmd
	sl.readyOK = false
	sl.readyChClosed = false
	sl.logFile = f
	s.mu.Unlock()
	go s.reap(id, cmd)
}

