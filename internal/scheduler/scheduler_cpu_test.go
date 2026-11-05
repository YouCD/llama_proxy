package scheduler

import (
	"context"
	"net/http"
	"testing"
	"time"

	"llama_proxy/internal/config"
)

// TestCPUBackgroundSurvivesCodingSwitch 验证：未声明 gpu 的纯 CPU background 模型
// （不占显存）在 coding 切换时不停止——PID 不变、coding 态下基址仍可直连。
// 无需 resident 标记："coding 切换只停占卡的模型"。
func TestCPUBackgroundSurvivesCodingSwitch(t *testing.T) {
	cfg := &config.SchedulingConfig{
		Models: map[string]config.ProcessConfig{
			"coding-model": {Mode: config.ModeCoding, Command: sleepCommand, ReadinessURL: readinessURLFor(freePort(t)), APIKey: "coding-key"},
			"cpu-model":    {Mode: config.ModeBackground, Command: sleepCommand, ReadinessURL: readinessURLFor(freePort(t)), APIKey: "cpu-key", Autostart: true},
		},
	}
	sw := config.SwitchConfig{DrainTimeout: time.Millisecond, KillTimeout: time.Millisecond, StartupTimeout: 500 * time.Millisecond}
	s := New(cfg, time.Hour, sw, &http.Client{})
	s.SetProbe(fakeProbe(0))
	ctx := context.Background()
	if err := s.Start(ctx); err != nil {
		t.Fatalf("start: %v", err)
	}

	s.mu.Lock()
	cpuPID := 0
	if sl := s.procs["cpu-model"]; sl != nil && sl.cmd != nil && sl.cmd.Process != nil {
		cpuPID = sl.cmd.Process.Pid
	}
	s.mu.Unlock()
	if cpuPID == 0 {
		t.Fatal("cpu-model not running initially")
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
	if !s.IsCodingActive() {
		t.Fatal("expected coding active")
	}

	// 纯 CPU 模型幸存：PID 不变。
	s.mu.Lock()
	cpuPIDAfter := 0
	if sl := s.procs["cpu-model"]; sl != nil && sl.cmd != nil && sl.cmd.Process != nil {
		cpuPIDAfter = sl.cmd.Process.Pid
	}
	s.mu.Unlock()
	if cpuPIDAfter != cpuPID {
		t.Fatalf("cpu-model pid changed %d -> %d, want unchanged (cpu models survive coding switch)", cpuPID, cpuPIDAfter)
	}
	// coding 态下仍可直连。
	if s.BaseURLFor("cpu-model") == "" {
		t.Fatal("cpu-model BaseURLFor empty in coding state, want its base")
	}
}
