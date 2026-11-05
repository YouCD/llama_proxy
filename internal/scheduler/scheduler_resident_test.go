package scheduler

import (
	"context"
	"net/http"
	"testing"
	"time"

	"llama_proxy/internal/config"
)

// newResidentTestScheduler 构造三模型调度器：coding-model（coding）、
// resident-model（background+resident 常驻）、bg-model（background 非常驻、声明
// CUDA0，coding 切换时被停止让显存）。全部立即就绪。
func newResidentTestScheduler(t *testing.T, idle time.Duration) *Scheduler {
	t.Helper()
	cfg := &config.SchedulingConfig{
		Models: map[string]config.ProcessConfig{
			"coding-model":    {Mode: config.ModeCoding, Command: sleepCommand, ReadinessURL: readinessURLFor(freePort(t)), APIKey: "coding-key"},
			"resident-model":  {Mode: config.ModeBackground, Command: sleepCommand, ReadinessURL: readinessURLFor(freePort(t)), APIKey: "resident-key", Resident: true, Autostart: true},
			"bg-model":        {Mode: config.ModeBackground, Command: sleepCommand, ReadinessURL: readinessURLFor(freePort(t)), APIKey: "bg-key", GPU: "CUDA0", Autostart: true},
		},
	}
	sw := config.SwitchConfig{DrainTimeout: time.Millisecond, KillTimeout: time.Millisecond, StartupTimeout: 500 * time.Millisecond}
	s := New(cfg, idle, sw, &http.Client{})
	s.SetProbe(fakeProbe(0))
	return s
}

// residentPID 返回 resident-model 当前包装进程 PID（无进程时为 0）。
func residentPID(t *testing.T, s *Scheduler) int {
	t.Helper()
	s.mu.Lock()
	defer s.mu.Unlock()
	if sl := s.procs["resident-model"]; sl != nil && sl.cmd != nil && sl.cmd.Process != nil {
		return sl.cmd.Process.Pid
	}
	return 0
}

// TestResidentSurvivesCodingSwitch 验证：coding 切换时 resident 成员不停止
// （进程 PID 不变、就绪 channel 仍关闭），非常驻 background 成员被停止，
// coding 模型就绪。
func TestResidentSurvivesCodingSwitch(t *testing.T) {
	s := newResidentTestScheduler(t, time.Hour)
	ctx := context.Background()
	if err := s.Start(ctx); err != nil {
		t.Fatalf("start: %v", err)
	}
	if coding, ready := snapshotState(s); coding != "" || !ready["resident-model"] || !ready["bg-model"] {
		t.Fatalf("expected both background ready, got coding=%q ready=%v", coding, ready)
	}
	pidBefore := residentPID(t, s)
	if pidBefore == 0 {
		t.Fatal("resident-model not running before coding switch")
	}

	ready, err := s.EnsureModel(ctx, "coding-model")
	if err != nil {
		t.Fatalf("ensure coding: %v", err)
	}
	select {
	case <-ready:
	case <-time.After(2 * time.Second):
		t.Fatal("coding did not become ready")
	}

	// resident 成员存活：PID 不变、仍就绪。
	if pidAfter := residentPID(t, s); pidAfter != pidBefore {
		t.Fatalf("resident pid changed %d -> %d, want unchanged (resident survives coding switch)", pidBefore, pidAfter)
	}
	coding, readyMap := snapshotState(s)
	if coding != "coding-model" || !readyMap["coding-model"] {
		t.Fatalf("expected coding ready, got coding=%q ready=%v", coding, readyMap)
	}
	if readyMap["resident-model"] {
		// resident 仍就绪（readyOK 保持 true）。
	} else {
		t.Fatal("resident-model lost ready state during coding switch")
	}
	// 非常驻 background 成员被停止：无进程、未就绪。
	s.mu.Lock()
	bgStopped := s.procs["bg-model"] == nil || !s.procs["bg-model"].running()
	s.mu.Unlock()
	if !bgStopped {
		t.Fatal("non-resident bg-model should be stopped during coding switch")
	}
	// coding 态下 resident 仍有基址（可直连）。
	if got := s.BaseURLFor("resident-model"); got == "" {
		t.Fatal("resident-model BaseURLFor empty in coding state, want its base")
	}
}

// TestResidentAccessibleInCodingState 验证：coding 态（租约活跃）下请求 resident
// 模型能直接拿到已关闭的就绪 channel（不挂起、不返回未关闭 channel），
// 而非被 coding 租约保护跳过。
func TestResidentAccessibleInCodingState(t *testing.T) {
	s := newResidentTestScheduler(t, time.Hour) // 长租约：coding 态下租约恒活跃
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
		t.Fatal("expected coding active")
	}

	// coding 态下请求 resident 模型：必须立即放行（channel 已关闭），不得挂起。
	rch, err := s.EnsureModel(ctx, "resident-model")
	if err != nil {
		t.Fatalf("ensure resident in coding state: %v", err)
	}
	select {
	case <-rch:
	case <-time.After(2 * time.Second):
		t.Fatal("resident ready channel not closed in coding state (lease-skip regression)")
	}
}

// TestResidentNotRestartedOnSwitchBack 验证：coding 空闲租约到期切回 background
// 组时，已在运行的 resident 成员不被重启（PID 不变），非常驻成员重新加载。
func TestResidentNotRestartedOnSwitchBack(t *testing.T) {
	s := newResidentTestScheduler(t, 60*time.Millisecond) // 短租约便于触发空闲切回
	ctx := context.Background()
	if err := s.Start(ctx); err != nil {
		t.Fatalf("start: %v", err)
	}
	pidBefore := residentPID(t, s)
	if pidBefore == 0 {
		t.Fatal("resident-model not running initially")
	}

	ready, err := s.EnsureModel(ctx, "coding-model")
	if err != nil {
		t.Fatalf("ensure coding: %v", err)
	}
	<-ready
	// 等 coding 租约过期，空闲切回 background 组才合法。
	time.Sleep(150 * time.Millisecond)

	bgReady, err := s.EnsureModel(ctx, "bg-model")
	if err != nil {
		t.Fatalf("ensure background after idle: %v", err)
	}
	select {
	case <-bgReady:
	case <-time.After(2 * time.Second):
		t.Fatal("background group did not become ready after switch back")
	}

	// resident 成员未被重启：PID 不变。
	if pidAfter := residentPID(t, s); pidAfter != pidBefore {
		t.Fatalf("resident pid changed %d -> %d on switch back, want unchanged (no restart)", pidBefore, pidAfter)
	}
	coding, readyMap := snapshotState(s)
	if coding != "" || !readyMap["bg-model"] || !readyMap["resident-model"] {
		t.Fatalf("expected background group ready, got coding=%q ready=%v", coding, readyMap)
	}
}

// TestNoPoolExcludedFromLocalNodes 验证：no_pool 背景模型不出现在 LocalNodes
// （不加入代理池），普通背景模型照常入池。
func TestNoPoolExcludedFromLocalNodes(t *testing.T) {
	cfg := &config.SchedulingConfig{
		Models: map[string]config.ProcessConfig{
			"pool-model":   {Mode: config.ModeBackground, Command: sleepCommand, ReadinessURL: readinessURLFor(freePort(t)), Autostart: true},
			"nopool-model": {Mode: config.ModeBackground, Command: sleepCommand, ReadinessURL: readinessURLFor(freePort(t)), NoPool: true, Autostart: true},
		},
	}
	sw := config.SwitchConfig{DrainTimeout: time.Millisecond, KillTimeout: time.Millisecond, StartupTimeout: 500 * time.Millisecond}
	s := New(cfg, time.Hour, sw, &http.Client{})
	s.SetProbe(fakeProbe(0))
	ctx := context.Background()
	if err := s.Start(ctx); err != nil {
		t.Fatalf("start: %v", err)
	}
	nodes := s.LocalNodes()
	if len(nodes) != 1 {
		t.Fatalf("LocalNodes = %d, want 1 (no_pool excluded)", len(nodes))
	}
	if nodes[0].ModelID != "pool-model" {
		t.Fatalf("LocalNodes[0] = %q, want pool-model", nodes[0].ModelID)
	}
	// no_pool 模型仍按模型 ID 可直连（BaseURLFor 非空），只是不入池。
	if got := s.BaseURLFor("nopool-model"); got == "" {
		t.Fatal("no_pool model BaseURLFor empty, want direct-access base")
	}
}
