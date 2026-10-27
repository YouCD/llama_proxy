package scheduler

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	"llama_proxy/internal/config"
)

// fakeProbe 返回一个就绪探测函数：前 attempts 次失败，之后成功。

func fakeProbe(attempts int) func(context.Context, string) bool {
	n := 0
	return func(context.Context, string) bool {
		n++
		return n > attempts
	}
}

// urlAwareProbe 返回按 URL 判定的就绪探测：仅 URL 包含任一 marker 时成功。
// 用于在同一调度器内区分"background 永不就绪 / coding 秒就绪"。
func urlAwareProbe(markers ...string) func(context.Context, string) bool {
	return func(_ context.Context, url string) bool {
		for _, m := range markers {
			if strings.Contains(url, m) {
				return true
			}
		}
		return false
	}
}

const sleepCommand = "sleep 3600"

// newTestScheduler 构造双模型测试调度器：coding-model（coding，8081）与
// bg-model（background，8080，唯一 background 即隐式默认组）。
func newTestScheduler(t *testing.T, idle time.Duration) *Scheduler {
	t.Helper()
	cfg := &config.SchedulingConfig{
		Models: map[string]config.ProcessConfig{
			"coding-model": {
				Mode:         config.ModeCoding,
				Command:      sleepCommand,
				ReadinessURL: "http://127.0.0.1:8081/v1/models",
				APIKey:       "coding-key",
			},
			"bg-model": {
				Mode:         config.ModeBackground,
				Command:      sleepCommand,
				ReadinessURL: "http://127.0.0.1:8080/v1/models",
				APIKey:       "bg-key",
			},
		},
	}
	sw := config.SwitchConfig{DrainTimeout: time.Millisecond, KillTimeout: time.Millisecond, StartupTimeout: 500 * time.Millisecond}
	s := New(cfg, idle, sw, &http.Client{})
	s.SetProbe(fakeProbe(0)) // 立即就绪
	return s
}

// snapshotState 返回 (codingModel, 各模型 ready 状态) 的调试快照。
func snapshotState(s *Scheduler) (string, map[string]bool) {
	snap := s.Snapshot()
	ready := make(map[string]bool, len(snap.Models))
	for _, m := range snap.Models {
		ready[m.ID] = m.Ready
	}
	return snap.CodingModel, ready
}


func TestStartBackgroundThenEnsureCoding(t *testing.T) {
	s := newTestScheduler(t, time.Hour)
	ctx := context.Background()

	if err := s.Start(ctx); err != nil {
		t.Fatalf("start: %v", err)
	}
	coding, ready := snapshotState(s)
	if coding != "" || !ready["bg-model"] {
		t.Fatalf("expected background ready, got coding=%q ready=%v", coding, ready)
	}
	if got := s.APIKeyFor("bg-model"); got != "bg-key" {
		t.Fatalf("background APIKeyFor=%q, want bg-key", got)
	}
	if s.IsCodingActive() {
		t.Fatal("should not be coding active initially")
	}

	readyCh, err := s.EnsureModel(ctx, "coding-model")
	if err != nil {
		t.Fatalf("ensure coding: %v", err)
	}
	select {
	case <-readyCh:
	case <-time.After(time.Second):
		t.Fatal("coding did not become ready")
	}
	coding, ready = snapshotState(s)
	if coding != "coding-model" || !ready["coding-model"] {
		t.Fatalf("expected coding model ready, got coding=%q ready=%v", coding, ready)
	}
	if !s.IsCodingActive() {
		t.Fatal("should be coding active after coding request")
	}
	if got := s.BaseURLFor("coding-model"); got != "http://127.0.0.1:8081/v1" {
		t.Fatalf("unexpected base url: %s", got)
	}
	if got := s.APIKeyFor("coding-model"); got != "coding-key" {
		t.Fatalf("coding APIKeyFor=%q, want coding-key", got)
	}
	// coding 态下 background 成员已停止：无节点、无基址。
	if nodes := s.LocalNodes(); len(nodes) != 0 {
		t.Fatalf("LocalNodes in coding state = %d, want 0", len(nodes))
	}
	if got := s.BaseURLFor("bg-model"); got != "" {
		t.Fatalf("bg BaseURLFor in coding state = %q, want empty", got)
	}
}

// TestStartBackgroundGroupParallel 验证 background 组态：default 组内两个成员
// 并行常驻，各自就绪后各作为一个代理池节点。
func TestStartBackgroundGroupParallel(t *testing.T) {
	cfg := &config.SchedulingConfig{
		Models: map[string]config.ProcessConfig{
			"bg-a": {Mode: config.ModeBackground, Command: sleepCommand, ReadinessURL: "http://127.0.0.1:8080/v1/models", APIKey: "key-a", Weight: 2},
			"bg-b": {Mode: config.ModeBackground, Command: sleepCommand, ReadinessURL: "http://127.0.0.1:8081/v1/models", APIKey: "key-b"},
		},
		Default: []string{"bg-a", "bg-b"},
	}
	sw := config.SwitchConfig{DrainTimeout: time.Millisecond, KillTimeout: time.Millisecond, StartupTimeout: 500 * time.Millisecond}
	s := New(cfg, time.Hour, sw, &http.Client{})
	s.SetProbe(fakeProbe(0))
	ctx := context.Background()

	if err := s.Start(ctx); err != nil {
		t.Fatalf("start: %v", err)
	}
	coding, ready := snapshotState(s)
	if coding != "" || !ready["bg-a"] || !ready["bg-b"] {
		t.Fatalf("expected both background members ready, got coding=%q ready=%v", coding, ready)
	}
	nodes := s.LocalNodes()
	if len(nodes) != 2 {
		t.Fatalf("LocalNodes = %d, want 2", len(nodes))
	}
	byID := map[string]LocalNode{}
	for _, n := range nodes {
		byID[n.ModelID] = n
	}
	if n := byID["bg-a"]; n.BaseURL != "http://127.0.0.1:8080/v1" || n.Weight != 2 {
		t.Fatalf("bg-a node = %+v, want base 8080 weight 2", n)
	}
	if n := byID["bg-b"]; n.BaseURL != "http://127.0.0.1:8081/v1" || n.Weight != 0 {
		t.Fatalf("bg-b node = %+v, want base 8081 weight 0 (pass-through)", n)
	}
	snap := s.Snapshot()
	if len(snap.Group) != 2 {
		t.Fatalf("Snapshot.Group = %v, want 2 members", snap.Group)
	}
}

// TestRestartMemberOnly 验证成员级崩溃恢复：杀掉一个 background 成员后，
// reconcile 只重启该成员，其余成员进程不受影响。
func TestRestartMemberOnly(t *testing.T) {
	cfg := &config.SchedulingConfig{
		Models: map[string]config.ProcessConfig{
			"bg-a": {Mode: config.ModeBackground, Command: sleepCommand, ReadinessURL: "http://127.0.0.1:8080/v1/models"},
			"bg-b": {Mode: config.ModeBackground, Command: sleepCommand, ReadinessURL: "http://127.0.0.1:8081/v1/models"},
		},
		Default: []string{"bg-a", "bg-b"},
	}
	sw := config.SwitchConfig{DrainTimeout: time.Millisecond, KillTimeout: time.Millisecond, StartupTimeout: 500 * time.Millisecond}
	s := New(cfg, time.Hour, sw, &http.Client{})
	s.SetProbe(fakeProbe(0))
	ctx := context.Background()
	if err := s.Start(ctx); err != nil {
		t.Fatalf("start: %v", err)
	}

	s.mu.Lock()
	pidB := s.procs["bg-b"].cmd.Process.Pid
	_ = s.procs["bg-a"].cmd.Process.Kill()
	s.mu.Unlock()

	// 等待 reap 清空 bg-a 的 cmd（进程已死）。
	deadline := time.Now().Add(2 * time.Second)
	for {
		s.mu.Lock()
		dead := s.procs["bg-a"].cmd == nil
		s.mu.Unlock()
		if dead {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("bg-a cmd not reaped after kill")
		}
		time.Sleep(5 * time.Millisecond)
	}

	s.reconcile(ctx)

	s.mu.Lock()
	aAlive := s.procs["bg-a"].cmd != nil
	newPIDB := 0
	if s.procs["bg-b"].cmd != nil {
		newPIDB = s.procs["bg-b"].cmd.Process.Pid
	}
	s.mu.Unlock()
	if !aAlive {
		t.Fatal("bg-a not restarted by reconcile")
	}
	if newPIDB != pidB {
		t.Fatalf("bg-b pid changed %d -> %d, want unchanged (only bg-a restarts)", pidB, newPIDB)
	}
	// 重启的成员复用原 readyCh，探针成功后放行。
	<-s.procs["bg-a"].readyCh
}


func waitSwitchInflight(t *testing.T, s *Scheduler, target string) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for {
		s.mu.Lock()
		inflight := s.switchTarget
		s.mu.Unlock()
		if inflight == target {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("switch did not enter readiness wait in time (target=%q)", target)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// TestCodingPreemptsBackgroundStartup 验证：代理启动后 background 组仍在加载
//（永不就绪），此时 coding 请求到达应直接抢占 background 组加载，而不是排队等
// 它加载完。coding 应立即就绪，且 Start 不得把这种抢占当致命错误。
