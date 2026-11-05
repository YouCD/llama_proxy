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
			leaseExpired := s.lease > 0 && !s.lastCodingAt.IsZero() && time.Since(s.lastCodingAt) >= s.lease
			codingModel := s.codingModel
			procAlive := s.codingProcRunningLocked()
			// 新一轮 coding 加载处于就绪等待且尚未就绪：跳过本次切回，避免杀掉
			// 正在加载的进程（大模型加载时长可能超过租约）。
			freshLoad := s.switchCancel != nil && s.switchMode == config.ModeCoding && !s.codingSlotReadyLocked()
			s.mu.Unlock()
			if codingModel != "" && leaseExpired && !freshLoad {
				// 进程在运行 = 正常空闲到期；进程已退出（启动失败/崩溃）= 恢复
				// 切回。旧逻辑要求进程在运行，coding 进程死亡后 codingModel 永远
				// 无法清除，系统卡在"无 coding 也无 background"的死状态。
				log.WithCtx(ctx).Infof("[scheduler] coding model %s lease expired (process running=%v), switching to default background group", codingModel, procAlive)
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
	// coding 进程未就绪即退出（启动失败）：不等待租约到期，立即切回默认
	// background 组，避免系统长时间处于"无 coding 也无 background"状态。
	// 就绪后崩溃的 coding 进程按原语义处理（下次 coding 请求重新加载）。
	s.mu.Lock()
	cm := s.codingModel
	cmSlot := s.procs[cm]
	codingSwitchInFlight := s.switchCancel != nil && s.switchMode == config.ModeCoding
	deadNeverReady := cm != "" && cmSlot != nil && !cmSlot.running() && !cmSlot.readyChClosed
	s.mu.Unlock()
	if deadNeverReady && !codingSwitchInFlight {
		group := s.cfg.DefaultBackgroundGroup()
		if len(group) > 0 {
			log.WithCtx(ctx).Warnf("[scheduler] coding model %s exited before ready, switching to default background group", cm)
			_, _ = s.switchTo(ctx, group, config.ModeBackground)
		} else {
			// 无默认 background 组：清除 coding 登记，避免每轮重复告警；
			// 下次 coding 请求会重新触发加载。
			s.mu.Lock()
			s.codingModel = ""
			s.mu.Unlock()
			log.WithCtx(ctx).Warnf("[scheduler] coding model %s exited before ready; no default background configured", cm)
		}
		return
	}

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
		e := s.cfg.Entry(it.id)
		if e == nil {
			continue
		}
		// 休眠中的成员：不探活、不自动拉起（等待直接请求经 ensureBackground 唤醒）。
		if it.sl.sleeping {
			continue
		}
		if it.sl.readyOK {
			// 就绪且未休眠：判断是否闲置到该休眠（仅 no_pool background 成员可配置
			// sleep_timeout）。
			if it.mode == config.ModeBackground && e.SleepTimeout > 0 && s.idleForSleep(it.id, e.SleepTimeout) {
				s.sleepMember(ctx, it.id)
			}
			continue
		}
		if e.ReadinessURL == "" {
			continue
		}
		// 包装进程已退出：先按端口接管可能仍在运行的真实服务（如 setsid
		// 派生、包装脚本已退出的推理服务）；接管成功则不重启。未接管到才
		// 视为真正退出：只重启 background 组成员（coding 进程由请求驱动
		// 重新加载），同时清除过期的 serverPID 登记。
		if it.sl.cmd == nil {
			adopted := false
			if it.sl.port > 0 {
				if pid, err := resolveServerPID(it.sl.port); err == nil {
					s.mu.Lock()
					if sl := s.procs[it.id]; sl != nil && sl.cmd == nil {
						sl.serverPID = pid
					}
					s.mu.Unlock()
					log.WithCtx(ctx).Infof("[scheduler] model %s wrapper exited, adopted detached server pid=%d", it.id, pid)
					adopted = true
				}
			}
			if !adopted {
				s.mu.Lock()
				if sl := s.procs[it.id]; sl != nil && sl.cmd == nil {
					sl.serverPID = 0
				}
				s.mu.Unlock()
				if it.mode == config.ModeBackground {
					log.WithCtx(ctx).Warnf("[scheduler] background member %s process exited, restarting member only", it.id)
					s.restartMember(ctx, it.id)
				}
				continue
			}
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
	// needRestart：槽位存在且无存活进程（包装命令与接管的真实服务都已退出）。
	needRestart := sl != nil && !sl.running() && e != nil &&
		e.Mode == config.ModeBackground && s.expected[id] && e.Command != ""
	// 启动失败退避窗口内不重启（reap 已记录失败并给出下次重试时间），
	// 由后续探活轮次在窗口结束后触发，避免崩溃循环被每 5 秒重启一次。
	backoff := needRestart && s.backoffActiveLocked(id)
	if !needRestart || backoff {
		s.mu.Unlock()
		return
	}
	// 清理旧日志文件，重新以追加模式打开。
	if sl.logFile != nil {
		_ = sl.logFile.Close()
		sl.logFile = nil
	}
	s.mu.Unlock()

	// per-GPU 独占仲裁：唤醒声明了 GPU 的成员时，若同卡已被另一个（按需）
	// 模型占用，先将其优雅驱逐，避免双模型同卡抢显存。
	if err := s.acquireGPU(ctx, id); err != nil {
		log.WithCtx(ctx).Warnf("[scheduler] restart %s blocked by gpu arbitration: %v", id, err)
		return
	}

	f, err := openProcessLog(e.LogFile)
	if err != nil {
		log.WithCtx(ctx).Warnf("[scheduler] reopen log file %s failed: %v", e.LogFile, err)
	}
	// 重启前同样清理端口残留监听者，避免半死服务占用端口导致新进程绑定失败。
	s.freeOccupiedPort(ctx, readinessPort(e), s.sw.KillTimeout)
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
	if sl.port == 0 {
		sl.port = readinessPort(e)
	}
	sl.cmd = cmd
	sl.readyOK = false
	// 不重置 readyChClosed，也不换新 channel（沿用"重启复用原 channel"的既有语义）：
	// 就绪前崩溃者其为 false，重启后就绪会 close channel；就绪后崩溃（含休眠唤醒）
	// 者其为 true（channel 已关），markSlotReady 不再二次 close 而 panic。等待者拿到
	// 的仍是同一 channel：就绪后崩溃再唤醒，旧等待者早已解除。
	sl.sleeping = false // 唤醒即脱离休眠态，reconcile 恢复正常探活
	sl.logFile = f
	s.mu.Unlock()
	go s.reap(id, cmd)
	// 唤醒后的就绪探测交给 reconcile 探活循环：非休眠且进程存活的成员会被 probe，
	// 就绪后由 markSlotReady 关闭（休眠时换的）新 readyCh。不在此处另起 waitReady
	// goroutine，避免在测试里对永不就绪的 sleep 进程空转到 startup_timeout 而泄漏。
}

