package scheduler

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	"llama_proxy/internal/config"
)

// 本文件覆盖"coding 模型启动失败"相关的调度器修复：
//  1. waitReady 监控进程存活：进程未就绪即退出时立即结束等待（不空等整个
//     startup_timeout 占着 switchMu），修复"连续切换 coding 模型卡 10 分钟"。
//  2. 启动失败退避：未就绪即退出/崩溃的模型按指数退避重试，防止必然失败的
//     启动被请求风暴反复拉起（vLLM 因 vm.overcommit_memory 取消启动的场景）。
//  3. coding 进程死亡后不再卡死 coding 态：未就绪即退出立即切回 background 组；
//     租约到期时进程已退出也切回；IsCodingActive 只认活着的进程。
//  4. coding 切 coding（不同目标）允许抢占正在加载的同级切换。

// failTestScheduler 构造双模型调度器：coding-a（coding，命令立即退出且永不就绪）
// 与 bg-model（background，sleep 常驻、立即就绪）。probeInterval 极小以加快探活。
func failTestScheduler(t *testing.T, lease time.Duration) (*Scheduler, string) {
	t.Helper()
	codingURL := readinessURLFor(freePort(t))
	bgURL := readinessURLFor(freePort(t))
	cfg := &config.SchedulingConfig{
		Models: map[string]config.ProcessConfig{
			"coding-a": {Mode: config.ModeCoding, Command: "true", ReadinessURL: codingURL, APIKey: "coding-key"},
			"bg-model": {Mode: config.ModeBackground, Command: sleepCommand, ReadinessURL: bgURL, APIKey: "bg-key"},
		},
		Default: []string{"bg-model"},
	}
	sw := config.SwitchConfig{DrainTimeout: time.Millisecond, KillTimeout: time.Millisecond, StartupTimeout: 30 * time.Second}
	s := New(cfg, lease, sw, &http.Client{})
	// 仅 background 地址就绪：coding-a 永远探测失败，但其进程立即退出。
	s.SetProbe(urlAwareProbe(urlPortMarker(bgURL)))
	s.probeInterval = 20 * time.Millisecond
	s.failBackoff = 100 * time.Millisecond
	return s, codingURL
}

func waitForFailCount(t *testing.T, s *Scheduler, id string, want int) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		snap := s.Snapshot()
		for _, m := range snap.Models {
			if m.ID == id && m.FailCount == want {
				return
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("model %s failCount never reached %d", id, want)
}

// TestCodingStartupFailureExitsQuickly 回归：coding 进程未就绪即退出时，切换
// 立即以错误结束（不空等整个 startup_timeout），并记录失败计数。
func TestCodingStartupFailureExitsQuickly(t *testing.T) {
	s, _ := failTestScheduler(t, time.Hour)
	ctx := context.Background()
	if err := s.Start(ctx); err != nil {
		t.Fatalf("start: %v", err)
	}
	t0 := time.Now()
	_, err := s.EnsureModel(ctx, "coding-a")
	if err == nil {
		t.Fatal("expected startup failure error")
	}
	if !strings.Contains(err.Error(), "exited before ready") {
		t.Fatalf("expected 'exited before ready' error, got: %v", err)
	}
	if d := time.Since(t0); d > 5*time.Second {
		t.Fatalf("EnsureModel took %v; should exit on process death, not wait out the 30s startup timeout", d)
	}
	waitForFailCount(t, s, "coding-a", 1)
	s.Shutdown()
}

// TestCodingStartupFailureBackoff 回归：连续启动失败进入退避，退避窗口内
// EnsureModel 直接报错不重复拉起；窗口过后允许重试。
func TestCodingStartupFailureBackoff(t *testing.T) {
	s, _ := failTestScheduler(t, time.Hour)
	// waitReady 的探测周期为 500ms：失败记录（reap）先于 waitReady 返回，
	// 退避窗口必须大于 500ms 才能覆盖到第三次请求。
	s.failBackoff = 2 * time.Second
	ctx := context.Background()
	if err := s.Start(ctx); err != nil {
		t.Fatalf("start: %v", err)
	}
	// 两次失败 → 进入 100ms 退避窗口。
	_, _ = s.EnsureModel(ctx, "coding-a")
	_, _ = s.EnsureModel(ctx, "coding-a")
	waitForFailCount(t, s, "coding-a", 2)

	t0 := time.Now()
	_, err := s.EnsureModel(ctx, "coding-a")
	if err == nil {
		t.Fatal("expected backoff error")
	}
	if !strings.Contains(err.Error(), "failed to start") {
		t.Fatalf("expected backoff error, got: %v", err)
	}
	if d := time.Since(t0); d > 500*time.Millisecond {
		t.Fatalf("backoff EnsureModel took %v; should reject immediately", d)
	}

	// 退避窗口过后允许再次尝试（仍会失败，但不再是退避拒绝）。
	time.Sleep(2300 * time.Millisecond)
	_, err = s.EnsureModel(ctx, "coding-a")
	if err == nil || strings.Contains(err.Error(), "failed to start") {
		t.Fatalf("expected retry attempt after backoff window, got: %v", err)
	}
	s.Shutdown()
}

// TestCodingPreemptsDifferentCodingModel 回归：coding 切 coding（不同目标）
// 允许抢占正在加载/等待的同级切换，不必等旧模型加载完或空等结束。
func TestCodingPreemptsDifferentCodingModel(t *testing.T) {
	aURL := readinessURLFor(freePort(t))
	bURL := readinessURLFor(freePort(t))
	cfg := &config.SchedulingConfig{
		Models: map[string]config.ProcessConfig{
			"coding-a": {Mode: config.ModeCoding, Command: sleepCommand, ReadinessURL: aURL, APIKey: "a-key"},
			"coding-b": {Mode: config.ModeCoding, Command: sleepCommand, ReadinessURL: bURL, APIKey: "b-key"},
		},
	}
	sw := config.SwitchConfig{DrainTimeout: time.Millisecond, KillTimeout: time.Millisecond, StartupTimeout: 30 * time.Second}
	s := New(cfg, time.Hour, sw, &http.Client{})
	// coding-a 永不就绪（模拟大模型加载），coding-b 立即就绪。
	s.SetProbe(urlAwareProbe(urlPortMarker(bURL)))
	ctx := context.Background()
	_ = s.Start(ctx) // 无默认 background 组：本地进程保持空闲

	// coding-a 切换进入就绪等待（永不返回）。
	go func() { _, _ = s.EnsureModel(ctx, "coding-a") }()
	// 在途登记前需执行 drain/GPU 显存基线（nvidia-smi）等步骤，繁忙机器上可能
	// 超过 2s：用更宽的轮询窗口，避免误报。
	deadline := time.Now().Add(10 * time.Second)
	for {
		s.mu.Lock()
		inflight := s.switchTarget
		s.mu.Unlock()
		if inflight == "coding-a" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("switch did not enter readiness wait in time (target=\"coding-a\")")
		}
		time.Sleep(10 * time.Millisecond)
	}

	// coding-b 请求到达：必须抢占 coding-a 的等待，立即完成。
	t0 := time.Now()
	ready, err := s.EnsureModel(ctx, "coding-b")
	if err != nil {
		t.Fatalf("ensure coding-b: %v", err)
	}
	select {
	case <-ready:
	case <-time.After(3 * time.Second):
		t.Fatal("coding-b ready channel not closed promptly after preempting coding-a")
	}
	if d := time.Since(t0); d > 2*time.Second {
		t.Errorf("EnsureModel(coding-b) took %v; preemption should not wait out coding-a's load", d)
	}
	coding, readyMap := snapshotState(s)
	if coding != "coding-b" || !readyMap["coding-b"] {
		t.Fatalf("expected coding-b ready, got coding=%q ready=%v", coding, readyMap)
	}
	s.Shutdown()
}

// TestDeadNeverReadyCodingSwitchesBack 回归：coding 进程未就绪即退出（启动失败）
// 时，探活循环立即切回默认 background 组，不等待租约到期——修复"无 coding 也
// 无 background"的死状态（旧逻辑下 codingModel 永不清除，只能重启服务恢复）。
func TestDeadNeverReadyCodingSwitchesBack(t *testing.T) {
	s, _ := failTestScheduler(t, 10*time.Minute) // 长租约：3s 内租约分支不可能触发
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := s.Start(ctx); err != nil {
		t.Fatalf("start: %v", err)
	}
	if _, err := s.EnsureModel(ctx, "coding-a"); err == nil {
		t.Fatal("expected startup failure")
	}

	go s.Loop(ctx)
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		coding, ready := snapshotState(s)
		if coding == "" && ready["bg-model"] {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("Loop did not switch back to background after coding startup failure (within 3s, lease is 10m)")
	s.Shutdown()
}

// TestLeaseExpiredWithDeadCodingProcess 回归：租约到期时 coding 进程已退出
//（就绪后崩溃），同样切回 background 组（旧逻辑要求进程在运行，死亡后永卡）。
func TestLeaseExpiredWithDeadCodingProcess(t *testing.T) {
	s := newTestScheduler(t, 300*time.Millisecond) // 租约 300ms > 探活 50ms
	s.probeInterval = 50 * time.Millisecond
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := s.Start(ctx); err != nil {
		t.Fatalf("start: %v", err)
	}
	ready, err := s.EnsureModel(ctx, "coding-model")
	if err != nil {
		t.Fatalf("ensure coding: %v", err)
	}
	<-ready

	// 杀掉 coding 进程并等 reap 完成。
	s.mu.Lock()
	if s.procs["coding-model"].cmd == nil || s.procs["coding-model"].cmd.Process == nil {
		t.Fatal("coding process not running")
	}
	_ = s.procs["coding-model"].cmd.Process.Kill()
	s.mu.Unlock()
	deadline := time.Now().Add(2 * time.Second)
	for {
		s.mu.Lock()
		dead := s.procs["coding-model"].cmd == nil
		s.mu.Unlock()
		if dead {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("coding process cmd not reaped after kill")
		}
		time.Sleep(5 * time.Millisecond)
	}

	go s.Loop(ctx)
	deadline = time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		coding, ready := snapshotState(s)
		if coding == "" && ready["bg-model"] {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("Loop did not switch back to background after lease expiry with dead coding process")
	s.Shutdown()
}

// TestIsCodingActiveFalseAfterProcessDeath 回归：coding 进程退出后 IsCodingActive
// 立即为 false（旧逻辑租约内仍为 true，background 请求被持续 503）。
func TestIsCodingActiveFalseAfterProcessDeath(t *testing.T) {
	s := newTestScheduler(t, time.Hour)
	ctx := context.Background()
	if err := s.Start(ctx); err != nil {
		t.Fatalf("start: %v", err)
	}
	ready, err := s.EnsureModel(ctx, "coding-model")
	if err != nil {
		t.Fatalf("ensure coding: %v", err)
	}
	<-ready
	if !s.IsCodingActive() {
		t.Fatal("coding should be active while running")
	}
	s.mu.Lock()
	_ = s.procs["coding-model"].cmd.Process.Kill()
	s.mu.Unlock()
	deadline := time.Now().Add(2 * time.Second)
	for {
		s.mu.Lock()
		dead := s.procs["coding-model"].cmd == nil
		s.mu.Unlock()
		if dead {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("coding process cmd not reaped after kill")
		}
		time.Sleep(5 * time.Millisecond)
	}
	if s.IsCodingActive() {
		t.Fatal("coding process is dead; IsCodingActive should be false (lease still fresh)")
	}
	s.Shutdown()
}

// TestRestartMemberBackoff 回归：background 成员连续启动失败进入退避，窗口内
// reconcile 不重复拉起；窗口过后恢复重启。
func TestRestartMemberBackoff(t *testing.T) {
	url := readinessURLFor(freePort(t))
	cfg := &config.SchedulingConfig{
		Models: map[string]config.ProcessConfig{
			"bg-fail": {Mode: config.ModeBackground, Command: "true", ReadinessURL: url},
		},
		Default: []string{"bg-fail"},
	}
	sw := config.SwitchConfig{DrainTimeout: time.Millisecond, KillTimeout: time.Millisecond, StartupTimeout: 30 * time.Second}
	s := New(cfg, time.Hour, sw, &http.Client{})
	s.SetProbe(urlAwareProbe()) // 永不就绪
	s.probeInterval = 20 * time.Millisecond
	s.failBackoff = 100 * time.Millisecond
	ctx := context.Background()

	// 首次启动即失败：Start 返回错误（测试内可忽略）。
	_ = s.Start(ctx)
	waitForFailCount(t, s, "bg-fail", 1)

	// 第 1 次失败：无退避，reconcile 立即重启 → 再次失败。
	s.reconcile(ctx)
	waitForFailCount(t, s, "bg-fail", 2)

	// 第 2 次失败：100ms 退避窗口内 reconcile 不重启。
	s.reconcile(ctx)
	s.mu.Lock()
	noRestart := s.procs["bg-fail"].cmd == nil
	s.mu.Unlock()
	if !noRestart {
		t.Fatal("member restarted inside backoff window")
	}

	// 窗口过后恢复重启。
	time.Sleep(250 * time.Millisecond)
	s.reconcile(ctx)
	s.mu.Lock()
	restarted := s.procs["bg-fail"].cmd != nil
	s.mu.Unlock()
	if !restarted {
		t.Fatal("member not restarted after backoff window")
	}
	s.Shutdown()
}
