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
		if sl.cmd != nil && sl.cmd.Process != nil {
			done := make(chan struct{})
			go func(cmd *exec.Cmd) {
				_ = cmd.Process.Kill()
				_, _ = cmd.Process.Wait()
				close(done)
			}(sl.cmd)
			remaining := time.Until(deadline)
			if remaining <= 0 {
				select {
				case <-done:
				case <-time.After(500 * time.Millisecond):
				}
				continue
			}
			select {
			case <-done:
			case <-time.After(remaining):
			}
		}
		if sl.logFile != nil {
			_ = sl.logFile.Close()
		}
	}
	if len(slots) > 0 {
		log.WithCtx(ctx).Infof("[scheduler] stopped %d previous process(es)", len(slots))
	}
}

// reap 回收已退出的子进程，防止僵尸进程。进程意外退出时清空槽位的 cmd，
// 使探活循环（reconcile）能识别退出并按成员级策略处理（background 成员重启，
// coding 进程留给下次请求重新加载）。正常切换时 stopAll 已清空 procs，不会误触。
func (s *Scheduler) reap(id string, cmd *exec.Cmd) {
	_ = cmd.Wait()
	s.mu.Lock()
	if sl := s.procs[id]; sl != nil && sl.cmd == cmd {
		sl.cmd = nil
		sl.readyOK = false
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

