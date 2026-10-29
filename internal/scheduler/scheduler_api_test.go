package scheduler

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"llama_proxy/internal/config"
)

// fakeProbe 返回一个就绪探测函数：前 attempts 次失败，之后成功。

func TestEnsureBackground(t *testing.T) {
	s := newTestScheduler(t, time.Hour)
	ctx := context.Background()
	if err := s.Start(ctx); err != nil {
		t.Fatalf("start: %v", err)
	}
	ready, err := s.EnsureModel(ctx, "bg-model")
	if err != nil {
		t.Fatalf("ensure background: %v", err)
	}
	select {
	case <-ready:
	case <-time.After(time.Second):
		t.Fatal("background ready channel not closed")
	}
	coding, readyMap := snapshotState(s)
	if coding != "" || !readyMap["bg-model"] || s.IsCodingActive() {
		t.Fatalf("expected background not-coding, got coding=%s ready=%v codingActive=%v", coding, readyMap, s.IsCodingActive())
	}
}

// TestLocalNodes 验证 LocalNodes 仅在 background 组态下返回就绪成员节点。
func TestLocalNodes(t *testing.T) {
	s := newTestScheduler(t, time.Hour)
	ctx := context.Background()

	// 启动前未就绪。
	if nodes := s.LocalNodes(); len(nodes) != 0 {
		t.Fatalf("LocalNodes before start = %d, want 0", len(nodes))
	}

	if err := s.Start(ctx); err != nil {
		t.Fatalf("start: %v", err)
	}
	nodes := s.LocalNodes()
	wantBase := apiBaseFromReadiness(s.cfg.Entry("bg-model").ReadinessURL)
	if len(nodes) != 1 || nodes[0].BaseURL != wantBase || nodes[0].ModelID != "bg-model" {
		t.Fatalf("LocalNodes after start = %+v, want bg-model on %s", nodes, wantBase)
	}

	// 切到 coding 后，本地节点出池。
	ready, _ := s.EnsureModel(ctx, "coding-model")
	<-ready
	if nodes := s.LocalNodes(); len(nodes) != 0 {
		t.Fatalf("LocalNodes while in coding mode = %d, want 0", len(nodes))
	}
}

func TestProcessLogFileCaptured(t *testing.T) {
	dir := t.TempDir()
	logPath := filepath.Join(dir, "coding.log")

	cfg := &config.SchedulingConfig{
		Models: map[string]config.ProcessConfig{
			"log-model": {
				Mode:         config.ModeBackground,
				Command:      "printf 'hello-stdout\\n' && printf 'hello-stderr\\n' >&2",
				ReadinessURL: readinessURLFor(freePort(t)),
				LogFile:      logPath,
			},
		},
	}
	sw := config.SwitchConfig{DrainTimeout: time.Millisecond, KillTimeout: time.Millisecond, StartupTimeout: 500 * time.Millisecond}
	s := New(cfg, time.Hour, sw, &http.Client{})
	s.SetProbe(fakeProbe(0))
	ctx := context.Background()

	// 用真实命令写入 stdout/stderr，验证日志文件接管。
	_, err := s.EnsureModel(ctx, "log-model")
	if err != nil {
		t.Fatalf("EnsureModel: %v", err)
	}
	// 等待进程输出落盘。
	time.Sleep(200 * time.Millisecond)
	s.Shutdown()

	data, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("read log file: %v", err)
	}
	out := string(data)
	if !strings.Contains(out, "hello-stdout") || !strings.Contains(out, "hello-stderr") {
		t.Fatalf("log file missing output: %q", out)
	}
}

func TestOpenProcessLogEmpty(t *testing.T) {
	f, err := openProcessLog("")
	if err != nil {
		t.Fatalf("open empty log: %v", err)
	}
	if f != nil {
		t.Fatal("expected nil file for empty log path")
	}
}

// TestReconcileReadyAfterStartupTimeout 验证启动超时后进程实际就绪时，
// reconcile 恢复就绪并放行等待者（readyCh 关闭、基址可用）。
func TestReconcileReadyAfterStartupTimeout(t *testing.T) {
	cfg := &config.SchedulingConfig{
		Models: map[string]config.ProcessConfig{
			"coding-model": {Mode: config.ModeCoding, Command: "sleep 30", ReadinessURL: readinessURLFor(freePort(t))},
		},
	}
	sw := config.SwitchConfig{DrainTimeout: time.Millisecond, KillTimeout: time.Millisecond, StartupTimeout: 300 * time.Millisecond}
	s := New(cfg, time.Hour, sw, &http.Client{})
	// 先让 probe 一直失败，触发启动超时。
	s.SetProbe(func(context.Context, string) bool { return false })
	ctx := context.Background()

	if _, err := s.EnsureModel(ctx, "coding-model"); err == nil {
		t.Fatal("expected startup timeout error")
	}
	s.mu.Lock()
	notReady := !s.procs["coding-model"].readyOK
	s.mu.Unlock()
	if !notReady {
		t.Fatal("expected not ready after startup timeout")
	}
	if s.BaseURLFor("coding-model") != "" {
		t.Fatal("expected empty BaseURLFor before ready")
	}

	// 进程实际就绪后，reconcile 应恢复就绪并放行等待者。
	s.SetProbe(func(context.Context, string) bool { return true })
	s.reconcile(ctx)

	s.mu.Lock()
	readyNow := s.procs["coding-model"].readyOK
	s.mu.Unlock()
	if !readyNow {
		t.Fatal("expected ready after reconcile")
	}
	if got := s.BaseURLFor("coding-model"); got != apiBaseFromReadiness(cfg.Entry("coding-model").ReadinessURL) {
		t.Fatalf("unexpected BaseURLFor: %q", got)
	}
	select {
	case <-s.procs["coding-model"].readyCh:
	default:
		t.Fatal("expected readyCh to be closed after reconcile")
	}
	s.Shutdown()
}

func TestWaitReadySendsAuthorization(t *testing.T) {
	var gotAuth string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	cfg := &config.SchedulingConfig{
		Models: map[string]config.ProcessConfig{
			"coding-model": {Mode: config.ModeCoding, Command: "", ReadinessURL: server.URL},
		},
	}
	sw := config.SwitchConfig{DrainTimeout: time.Millisecond, KillTimeout: time.Millisecond, StartupTimeout: time.Second}
	s := New(cfg, time.Hour, sw, server.Client())

	if err := s.waitReady(context.Background(), server.URL, "secret-token", time.Second); err != nil {
		t.Fatalf("waitReady: %v", err)
	}
	if gotAuth != "Bearer secret-token" {
		t.Fatalf("expected Authorization header %q, got %q", "Bearer secret-token", gotAuth)
	}
}

func TestAPIBaseFromReadiness(t *testing.T) {
	cases := []struct {
		in, want string
	}{
		{"http://127.0.0.1:8080", "http://127.0.0.1:8080/v1"},
		{"http://127.0.0.1:8080/", "http://127.0.0.1:8080/v1"},
		{"http://127.0.0.1:8080/health", "http://127.0.0.1:8080/v1"},
		{"http://127.0.0.1:8080/v1/models", "http://127.0.0.1:8080/v1"},
		{"https://example.com:8443/v1/chat/completions", "https://example.com:8443/v1"},
		{"", ""},
		{"not a url", ""},
	}
	for _, c := range cases {
		if got := apiBaseFromReadiness(c.in); got != c.want {
			t.Errorf("apiBaseFromReadiness(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

// TestMarkSlotReadyConcurrent 验证并发对同一槽位调用 markSlotReady 不应 panic
//（double-close 防护）。
func TestMarkSlotReadyConcurrent(t *testing.T) {
	cfg := &config.SchedulingConfig{
		Models: map[string]config.ProcessConfig{
			"coding-model": {Mode: config.ModeCoding, Command: "sleep 30", ReadinessURL: readinessURLFor(freePort(t))},
		},
	}
	sw := config.SwitchConfig{DrainTimeout: time.Millisecond, KillTimeout: time.Millisecond, StartupTimeout: time.Second}
	s := New(cfg, time.Hour, sw, &http.Client{})

	ch := make(chan struct{})
	s.mu.Lock()
	s.procs = map[string]*procSlot{"coding-model": {readyCh: ch}}
	s.mu.Unlock()

	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			s.markSlotReady("coding-model")
		}()
	}
	wg.Wait()
	select {
	case <-ch:
	default:
		t.Fatal("expected channel to be closed")
	}
}

// TestMarkSlotReadyReusesRestartChannel 验证成员重启复用新 channel 时，关闭只作用于
// 当前 channel，旧 channel 不受影响。
func TestMarkSlotReadyReusesRestartChannel(t *testing.T) {
	s := newTestScheduler(t, time.Hour)
	stale := make(chan struct{})

	s.mu.Lock()
	s.procs = map[string]*procSlot{"bg-model": {readyCh: stale}}
	s.mu.Unlock()

	// 模拟 restartMember：换成新 channel 并重置关闭标志。
	cur := make(chan struct{})
	s.mu.Lock()
	s.procs["bg-model"].readyCh = cur
	s.procs["bg-model"].readyChClosed = false
	s.mu.Unlock()

	// 当前 channel 正常关闭，且重复调用安全；stale channel 不受影响。
	s.markSlotReady("bg-model")
	s.markSlotReady("bg-model")
	select {
	case <-cur:
	default:
		t.Fatal("current ready channel must be closed")
	}
	select {
	case <-stale:
		t.Fatal("stale channel must not be closed")
	default:
	}
}

// waitSwitchInflight 轮询等待在途切换登记的目标描述（background 组为 "group"，
// coding 为模型 ID）。
