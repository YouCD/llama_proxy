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
func (s *Scheduler) stopAll(ctx context.Context) {
	s.mu.Lock()
	slots := make([]*procSlot, 0, len(s.procs))
	for _, sl := range s.procs {
		slots = append(slots, sl)
	}
	s.procs = make(map[string]*procSlot)
	s.expected = make(map[string]bool)
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
	sl.readyOK = false
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

