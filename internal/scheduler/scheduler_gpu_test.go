package scheduler

import (
	"context"
	"net/http"
	"syscall"
	"testing"
	"time"

	"llama_proxy/internal/config"
)

// noGPUProbe 把显存查询替换为"无 GPU 环境"，使 waitForGPURelease 立即放行
//（测试进程不占真实显存，避免每次换卡空等 ~1.5s）。测试结束恢复。
func noGPUProbe(t *testing.T) {
	t.Helper()
	orig := smiMemUsedFn
	smiMemUsedFn = func() ([]int, error) { return nil, syscall.ENOENT }
	t.Cleanup(func() { smiMemUsedFn = orig })
}

// newGPUSwapScheduler 构造 per-GPU 换入测试调度器：bg-a 在 default 组（启动即载），
// bg-b 在组外（按需），两者声明同一 GPU；coding-model 备用。全部立即就绪。
func newGPUSwapScheduler(t *testing.T) *Scheduler {
	t.Helper()
	gpu := "CUDA1"
	cfg := &config.SchedulingConfig{
		Models: map[string]config.ProcessConfig{
			"bg-a": {Mode: config.ModeBackground, Command: sleepCommand, ReadinessURL: readinessURLFor(freePort(t)), GPU: gpu, Autostart: true},
			"bg-b": {Mode: config.ModeBackground, Command: sleepCommand, ReadinessURL: readinessURLFor(freePort(t)), GPU: gpu},
		},
	}
	sw := config.SwitchConfig{DrainTimeout: time.Millisecond, KillTimeout: 500 * time.Millisecond, StartupTimeout: 500 * time.Millisecond}
	s := New(cfg, time.Hour, sw, &http.Client{})
	s.SetProbe(fakeProbe(0))
	s.probeInterval = 20 * time.Millisecond // 唤醒/崩溃恢复走探活循环，极小周期加快测试
	return s
}

// TestGPUSwapOnDemand 验证 per-GPU 互斥换入：组内 bg-a 与组外 bg-b 同卡，
// 请求 bg-b 时先驱逐 bg-a（置休眠），bg-b 加载就绪；再请求 bg-a 反向换回。
func TestGPUSwapOnDemand(t *testing.T) {
	noGPUProbe(t)
	s := newGPUSwapScheduler(t)
	ctx := context.Background()
	if err := s.Start(ctx); err != nil {
		t.Fatalf("start: %v", err)
	}
	go s.Loop(ctx) // 唤醒/崩溃恢复走探活循环

	// 请求组外 bg-b：应驱逐 bg-a 并加载 bg-b。
	readyB, err := s.EnsureModel(ctx, "bg-b")
	if err != nil {
		t.Fatalf("ensure bg-b: %v", err)
	}
	select {
	case <-readyB:
	case <-time.After(2 * time.Second):
		t.Fatal("bg-b did not become ready after swap")
	}
	s.mu.Lock()
	aSleeping := s.procs["bg-a"] != nil && s.procs["bg-a"].sleeping
	aDead := s.procs["bg-a"] == nil || !s.procs["bg-a"].running()
	bRunning := s.procs["bg-b"] != nil && s.procs["bg-b"].running()
	s.mu.Unlock()
	if !aSleeping || !aDead {
		t.Fatalf("bg-a not evicted (sleeping=%v dead=%v)", aSleeping, aDead)
	}
	if !bRunning {
		t.Fatal("bg-b not running after swap")
	}

	// 反向换回：请求 bg-a 驱逐 bg-b。
	readyA, err := s.EnsureModel(ctx, "bg-a")
	if err != nil {
		t.Fatalf("ensure bg-a: %v", err)
	}
	select {
	case <-readyA:
	case <-time.After(2 * time.Second):
		t.Fatal("bg-a did not become ready after swap-back")
	}
	s.mu.Lock()
	bSleeping := s.procs["bg-b"] != nil && s.procs["bg-b"].sleeping
	aRunning := s.procs["bg-a"] != nil && s.procs["bg-a"].running() && !s.procs["bg-a"].sleeping
	s.mu.Unlock()
	if !bSleeping || !aRunning {
		t.Fatalf("swap-back wrong state (bg-b sleeping=%v, bg-a running=%v)", bSleeping, aRunning)
	}
	s.Shutdown()
}

// TestGPUSwapNoReconcileFlap 验证驱逐不引发复活循环：bg-a 被 bg-b 驱逐后，
// 多轮 reconcile 不会把休眠的 bg-a 拉起来与 bg-b 抢卡。
func TestGPUSwapNoReconcileFlap(t *testing.T) {
	noGPUProbe(t)
	s := newGPUSwapScheduler(t)
	ctx := context.Background()
	if err := s.Start(ctx); err != nil {
		t.Fatalf("start: %v", err)
	}
	go s.Loop(ctx) // 探活循环：验证休眠的驱逐受害者不会被自动拉起
	readyB, err := s.EnsureModel(ctx, "bg-b")
	if err != nil {
		t.Fatalf("ensure bg-b: %v", err)
	}
	<-readyB
	s.mu.Lock()
	bPID := 0
	if sl := s.procs["bg-b"]; sl != nil && sl.cmd != nil && sl.cmd.Process != nil {
		bPID = sl.cmd.Process.Pid
	}
	s.mu.Unlock()

	for i := 0; i < 5; i++ {
		s.reconcile(ctx)
		s.mu.Lock()
		aRevived := s.procs["bg-a"] != nil && s.procs["bg-a"].running()
		bStill := bPID != 0 && s.procs["bg-b"] != nil && s.procs["bg-b"].cmd != nil &&
			s.procs["bg-b"].cmd.Process != nil && s.procs["bg-b"].cmd.Process.Pid == bPID
		s.mu.Unlock()
		if aRevived {
			t.Fatalf("reconcile %d: evicted bg-a revived (gpu flap)", i)
		}
		if !bStill {
			t.Fatalf("reconcile %d: bg-b process churned (pid changed)", i)
		}
	}
	s.Shutdown()
}

// TestGPUResidentPinRefusesSwap 验证 resident 独占卡：resident 模型占用的 gpu
// 上按需换入其他模型被拒绝（返回明确错误，不杀 resident）。
// 注：该组合在 config.Load 校验阶段即被拦截，此处直接构造配置验证运行时防线。
func TestGPUResidentPinRefusesSwap(t *testing.T) {
	noGPUProbe(t)
	gpu := "CUDA0"
	cfg := &config.SchedulingConfig{
		Models: map[string]config.ProcessConfig{
			"pin-model":   {Mode: config.ModeBackground, Command: sleepCommand, ReadinessURL: readinessURLFor(freePort(t)), GPU: gpu, Resident: true, Autostart: true},
			"invader":     {Mode: config.ModeBackground, Command: sleepCommand, ReadinessURL: readinessURLFor(freePort(t)), GPU: gpu},
		},
	}
	sw := config.SwitchConfig{DrainTimeout: time.Millisecond, KillTimeout: 500 * time.Millisecond, StartupTimeout: 500 * time.Millisecond}
	s := New(cfg, time.Hour, sw, &http.Client{})
	s.SetProbe(fakeProbe(0))
	s.probeInterval = 20 * time.Millisecond
	ctx := context.Background()
	if err := s.Start(ctx); err != nil {
		t.Fatalf("start: %v", err)
	}
	pidBefore := residentPIDOf(t, s, "pin-model")
	if pidBefore == 0 {
		t.Fatal("pin-model not running after start")
	}

	_, err := s.EnsureModel(ctx, "invader")
	if err == nil {
		t.Fatal("expected gpu pin rejection, got nil error")
	}
	if pidAfter := residentPIDOf(t, s, "pin-model"); pidAfter != pidBefore {
		t.Fatalf("resident pin-model killed by swap (pid %d -> %d)", pidBefore, pidAfter)
	}
	s.Shutdown()
}

// residentPIDOf 返回指定模型当前包装进程 PID（无进程时为 0）。
func residentPIDOf(t *testing.T, s *Scheduler, id string) int {
	t.Helper()
	s.mu.Lock()
	defer s.mu.Unlock()
	if sl := s.procs[id]; sl != nil && sl.cmd != nil && sl.cmd.Process != nil {
		return sl.cmd.Process.Pid
	}
	return 0
}

// TestGPUAbsentNoArbitration 验证未声明 gpu 的模型不参与仲裁：同卡"冲突"不存在，
// 按需加载直接放行，不影响其他模型。
func TestGPUAbsentNoArbitration(t *testing.T) {
	noGPUProbe(t)
	cfg := &config.SchedulingConfig{
		Models: map[string]config.ProcessConfig{
			"cpu-a": {Mode: config.ModeBackground, Command: sleepCommand, ReadinessURL: readinessURLFor(freePort(t)), Autostart: true},
			"cpu-b": {Mode: config.ModeBackground, Command: sleepCommand, ReadinessURL: readinessURLFor(freePort(t))},
		},
	}
	sw := config.SwitchConfig{DrainTimeout: time.Millisecond, KillTimeout: 500 * time.Millisecond, StartupTimeout: 500 * time.Millisecond}
	s := New(cfg, time.Hour, sw, &http.Client{})
	s.SetProbe(fakeProbe(0))
	s.probeInterval = 20 * time.Millisecond
	ctx := context.Background()
	if err := s.Start(ctx); err != nil {
		t.Fatalf("start: %v", err)
	}
	readyB, err := s.EnsureModel(ctx, "cpu-b")
	if err != nil {
		t.Fatalf("ensure cpu-b: %v", err)
	}
	<-readyB
	s.mu.Lock()
	aAlive := s.procs["cpu-a"] != nil && s.procs["cpu-a"].running()
	bAlive := s.procs["cpu-b"] != nil && s.procs["cpu-b"].running()
	s.mu.Unlock()
	if !aAlive || !bAlive {
		t.Fatalf("cpu models must coexist without gpu arbitration (a=%v b=%v)", aAlive, bAlive)
	}
	s.Shutdown()
}
