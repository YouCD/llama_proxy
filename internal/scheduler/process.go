// process.go 进程生命周期：进程启动日志（openProcessLog）、切换前的优雅排空
//（drain）、整组停止（stopAll）、僵尸进程回收（reap）、就绪等待（waitReady）
// 与就绪探测（probeReady）。
package scheduler

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"time"

	"llama_proxy/internal/config"

	"github.com/youcd/toolkit/log"
)
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

// stopAll 停止当前全部子进程（共享一个 kill_timeout 总时限），关闭日志文件，
// 清空进程槽位。切换后 expected/procs 由调用方（switchTo）重新登记。
// 幸存者（保留槽位与 expected 登记，不随切换停止）：
//   - resident background 成员：与 coding 模型并行运行（显存共存），卡被它钉死；
//   - 未声明 gpu 的 background 成员（纯 CPU）：不占显存，coding 无需为其让位，
//     天然幸存——“coding 切换只停占卡的模型”。
// switchTo 对幸存的在运行成员复用原槽位，不重启。
func (s *Scheduler) stopAll(ctx context.Context) {
	s.mu.Lock()
	var slots []*procSlot
	kept := make(map[string]*procSlot)
	keptExpected := make(map[string]bool)
	for id, sl := range s.procs {
		e := s.cfg.Entry(id)
		if e != nil && e.Mode == config.ModeBackground && (e.Resident || e.GPU == "") {
			kept[id] = sl
			keptExpected[id] = true
			continue
		}
		slots = append(slots, sl)
	}
	s.procs = kept
	s.expected = keptExpected
	s.codingModel = ""
	s.mu.Unlock()

	deadline := time.Now().Add(s.sw.KillTimeout)
	for _, sl := range slots {
		s.stopSlotProcess(sl, deadline)
		if sl.logFile != nil {
			_ = sl.logFile.Close()
		}
	}
	if len(slots) > 0 {
		log.WithCtx(ctx).Infof("[scheduler] stopped %d previous process(es)", len(slots))
	}
}

// stopSlotProcess 终止单个槽位关联的全部进程并确认端口释放：
//   - 先按端口重新解析真实服务进程（setsid 派生、包装脚本已退出的场景下
//     槽位缓存可能缺失或过期），与包装命令进程一并作为终止目标；
//   - 对每个目标按进程树发送 SIGTERM（会话/进程组 leader 杀整组，否则递归
//     后代），等待目标全部退出且端口监听释放，直至 deadline；
//   - 超时后对仍存活的目标发送 SIGKILL 并再做一次端口确认。
//
// 包装命令进程的僵尸回收由 reap 协程负责，此处不重复 Wait。
func (s *Scheduler) stopSlotProcess(sl *procSlot, deadline time.Time) {
	// 调用方（stopAll/Shutdown）均在调用前释放 s.mu，且 reap 协程会并发改写
	// cmd/serverPID：终止目标必须在锁内快照，kill 等待在锁外进行。
	// 槽位端口终身不变，无竞态。
	s.mu.Lock()
	if sl.port > 0 {
		if pid, err := resolveServerPID(sl.port); err == nil {
			sl.serverPID = pid
		}
	}
	var targets []int
	self := os.Getpid()
	if sl.serverPID != 0 && pidAlive(sl.serverPID) && sl.serverPID != self {
		targets = append(targets, sl.serverPID)
	}
	if sl.cmd != nil && sl.cmd.Process != nil {
		if pid := sl.cmd.Process.Pid; pid != self && !pidIn(targets, pid) {
			targets = append(targets, pid)
		}
	}
	s.mu.Unlock()
	if len(targets) == 0 {
		return
	}
	for _, pid := range targets {
		group, _ := procGroupID(pid)
		_ = killTree(pid, group, syscall.SIGTERM)
	}
	for anyAlive(targets) || !portListenerGone(sl.port) {
		if time.Now().After(deadline) {
			for _, pid := range targets {
				if !pidAlive(pid) {
					continue
				}
				group, _ := procGroupID(pid)
				_ = killTree(pid, group, syscall.SIGKILL)
			}
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// reap 回收已退出的包装命令进程，防止僵尸进程。包装脚本退出后按端口重新解析
// 真实服务进程：服务仍存活（如 vLLM launcher 以 setsid 派生的服务在 launcher
// 退出后继续运行）则接管为槽位进程、维持运行态与就绪态；确认无监听者才视为
// 真正退出（清空槽位的 cmd 与 serverPID），使探活循环（reconcile）能识别退出
// 并按成员级策略处理（background 成员重启，coding 进程留给下次请求重新加载）。
// 正常切换时 stopAll 已清空 procs，不会误触。
func (s *Scheduler) reap(id string, cmd *exec.Cmd) {
	_ = cmd.Wait()
	s.mu.Lock()
	defer s.mu.Unlock()
	sl := s.procs[id]
	if sl == nil || sl.cmd != cmd {
		return
	}
	sl.cmd = nil
	if sl.port > 0 {
		if pid, err := resolveServerPID(sl.port); err == nil {
			sl.serverPID = pid
			log.WithCtx(context.Background()).Infof("[scheduler] %s wrapper exited, adopted detached server pid=%d", id, pid)
			return
		}
	}
	sl.serverPID = 0
	wasReady := sl.readyOK
	sl.readyOK = false
	// 主动休眠（sleepMember 先置 sleeping 再杀进程）：不是启动失败/崩溃，不记失败、
	// 不退避，由直接请求经 ensureBackground→restartMember 唤醒。
	if sl.sleeping {
		return
	}
	nxt := s.recordStartupFailLocked(id)
	n := s.failCount[id]
	if wasReady {
		log.WithCtx(context.Background()).Warnf("[scheduler] %s process crashed after ready (fail #%d)", id, n)
	} else if nxt.IsZero() {
		log.WithCtx(context.Background()).Warnf("[scheduler] %s process exited before ready (fail #%d)", id, n)
	} else {
		log.WithCtx(context.Background()).Warnf("[scheduler] %s process exited before ready (fail #%d, retrying after %s)", id, n, nxt.Format(time.RFC3339))
	}
}

// sleepMember 主动停止一个闲置的 background 成员进程以释放显存（休眠）。与崩溃
// 不同：它是计划内停止，reap 观察到 sleeping 标记不记失败、不退避，reconcile 不再
// 自动拉起，仅由直接请求经 ensureBackground→restartMember 唤醒。停之前要求成员已
// 就绪且无在途请求（调用方 reconcile 已判定，此处防御性再确认）。持 switchMu 与
// 整组切换互斥。
func (s *Scheduler) sleepMember(ctx context.Context, id string) {
	s.switchMu.Lock()
	defer s.switchMu.Unlock()
	s.mu.Lock()
	sl := s.procs[id]
	e := s.cfg.Entry(id)
	if sl == nil || !sl.running() || !sl.readyOK || sl.sleeping || e == nil || s.inflight[id] > 0 {
		s.mu.Unlock()
		return
	}
	s.mu.Unlock()
	s.suspendSlot(ctx, id, sl)
	log.WithCtx(ctx).Infof("[scheduler] model %s idle, sleeping (process stopped, memory released)", id)
}

// suspendSlot 停止槽位进程并将其置为休眠态（计划内停止：reap 不记失败不退避，
// reconcile 不自动拉起，组切换不打扰，仅由直接请求唤醒）。先置 sleeping 再杀
// 进程，使 reap 在包装脚本退出时能识别为休眠而非崩溃。调用方须持有 switchMu，
// 槽位须存在且进程存活。
func (s *Scheduler) suspendSlot(ctx context.Context, id string, sl *procSlot) {
	s.mu.Lock()
	sl.sleeping = true
	// 立即可观测为未就绪：唤醒前的请求拿到 "not ready"，而非转发到垂死进程。
	sl.readyOK = false
	s.mu.Unlock()
	deadline := time.Now().Add(s.sw.KillTimeout)
	s.stopSlotProcess(sl, deadline)
	s.mu.Lock()
	sl.cmd = nil
	sl.serverPID = 0
	if sl.logFile != nil {
		_ = sl.logFile.Close()
		sl.logFile = nil
	}
	// 休眠/驱逐 = 不再就绪。旧的 readyCh 已关闭（就绪过）且无在途等待者，换成一个
	// 全新的未关闭 channel：唤醒后 markSlotReady 会 close 它，使等待者真正等到
	// 重载完成，而非拿到旧的已关闭 channel 的虚假“就绪”。崩溃恢复路径不经过
	// 这里，仍复用原 channel（保持既有测试语义）。
	sl.readyCh = make(chan struct{})
	sl.readyChClosed = false
	s.clearStartupFailLocked(id)
	s.mu.Unlock()
}

// waitReady 轮询配置的 readinessURL，直到成功或超时。watchProc 为真时等待
// 期间持续检查槽位进程存活：进程在就绪前退出（启动失败/崩溃，如 vLLM 因
// vm.overcommit_memory 取消启动）立即返回错误，而不是空等整个 startup_timeout
// 占着 switchMu——否则后续每一次模型切换都要先等上一次失败模型的空等结束。
func (s *Scheduler) waitReady(ctx context.Context, id, baseURL, apiKey string, timeout time.Duration, watchProc bool) error {
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
		if watchProc && s.slotDead(id) {
			return fmt.Errorf("process exited before ready")
		}
		select {
		case <-probeCtx.Done():
			return probeCtx.Err()
		case <-ticker.C:
		}
	}
}

// slotDead 报告槽位进程是否已确认退出（见 slotDeadLocked）。
func (s *Scheduler) slotDead(id string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.slotDeadLocked(id)
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

