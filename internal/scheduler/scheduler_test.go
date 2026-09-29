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
func fakeProbe(attempts int) func(context.Context, string) bool {
	n := 0
	return func(context.Context, string) bool {
		n++
		return n > attempts
	}
}

// newTestScheduler 构造一个双模型测试调度器：coding-model（coding 模式）与
// bg-model（background 模式，唯一 background 即隐式默认）。
func newTestScheduler(t *testing.T, idle time.Duration) *Scheduler {
	t.Helper()
	cfg := &config.SchedulingConfig{
		Models: map[string]config.ProcessConfig{
			"coding-model": {
				Mode:         config.ModeCoding,
				Command:      "",
				ReadinessURL: "http://127.0.0.1:8080",
				APIKey:       "coding-key",
			},
			"bg-model": {
				Mode:         config.ModeBackground,
				Command:      "",
				ReadinessURL: "http://127.0.0.1:8080",
				APIKey:       "bg-key",
			},
		},
	}
	sw := config.SwitchConfig{DrainTimeout: time.Millisecond, KillTimeout: time.Millisecond, StartupTimeout: 500 * time.Millisecond}
	s := New(cfg, idle, sw, &http.Client{})
	s.SetProbe(fakeProbe(0)) // 立即就绪
	return s
}

func TestStartBackgroundThenEnsureCoding(t *testing.T) {
	s := newTestScheduler(t, time.Hour)
	ctx := context.Background()

	if err := s.Start(ctx); err != nil {
		t.Fatalf("start: %v", err)
	}
	if s.model != "bg-model" || !s.readyOK {
		t.Fatalf("expected background ready, got model=%s ready=%v", s.model, s.readyOK)
	}
	if got := s.ActiveAPIKey(); got != "bg-key" {
		t.Fatalf("background ActiveAPIKey=%q, want bg-key", got)
	}
	if s.IsCodingActive() {
		t.Fatal("should not be coding active initially")
	}

	ready, err := s.EnsureModel(ctx, "coding-model")
	if err != nil {
		t.Fatalf("ensure coding: %v", err)
	}
	select {
	case <-ready:
	case <-time.After(time.Second):
		t.Fatal("coding did not become ready")
	}
	if s.model != "coding-model" {
		t.Fatalf("expected coding model, got %s", s.model)
	}
	if !s.IsCodingActive() {
		t.Fatal("should be coding active after coding request")
	}
	if got := s.ActiveBaseURL(); got != "http://127.0.0.1:8080/v1" {
		t.Fatalf("unexpected base url: %s", got)
	}
	if got := s.ActiveAPIKey(); got != "coding-key" {
		t.Fatalf("coding ActiveAPIKey=%q, want coding-key", got)
	}
}

// TestSwitchBetweenBackgroundModels 验证多个 background 模型：请求另一个
// background 模型 ID 时触发模型切换，本地节点信息跟随当前加载模型。
func TestSwitchBetweenBackgroundModels(t *testing.T) {
	cfg := &config.SchedulingConfig{
		Models: map[string]config.ProcessConfig{
			"bg-a": {Mode: config.ModeBackground, Command: "", ReadinessURL: "http://127.0.0.1:8080", APIKey: "key-a"},
			"bg-b": {Mode: config.ModeBackground, Command: "", ReadinessURL: "http://127.0.0.1:8081", APIKey: "key-b"},
		},
		Default: "bg-a",
	}
	sw := config.SwitchConfig{DrainTimeout: time.Millisecond, KillTimeout: time.Millisecond, StartupTimeout: 500 * time.Millisecond}
	s := New(cfg, time.Hour, sw, &http.Client{})
	s.SetProbe(fakeProbe(0))
	ctx := context.Background()

	if err := s.Start(ctx); err != nil {
		t.Fatalf("start: %v", err)
	}
	if s.model != "bg-a" || !s.readyOK {
		t.Fatalf("expected bg-a ready, got model=%s ready=%v", s.model, s.readyOK)
	}
	if got := s.ActiveBaseURL(); got != "http://127.0.0.1:8080/v1" {
		t.Fatalf("unexpected base url: %s", got)
	}

	ready, err := s.EnsureModel(ctx, "bg-b")
	if err != nil {
		t.Fatalf("ensure bg-b: %v", err)
	}
	select {
	case <-ready:
	case <-time.After(time.Second):
		t.Fatal("bg-b did not become ready")
	}
	if s.model != "bg-b" || !s.readyOK {
		t.Fatalf("expected bg-b ready, got model=%s ready=%v", s.model, s.readyOK)
	}
	if got := s.ActiveBaseURL(); got != "http://127.0.0.1:8081/v1" {
		t.Fatalf("unexpected base url: %s", got)
	}
	if got := s.ActiveAPIKey(); got != "key-b" {
		t.Fatalf("ActiveAPIKey=%q, want key-b", got)
	}
	node, ok := s.LocalNode()
	if !ok || node.ModelID != "bg-b" || node.BaseURL != "http://127.0.0.1:8081/v1" {
		t.Fatalf("LocalNode = (%+v, %v), want bg-b on 8081", node, ok)
	}
}

func TestEnsureCodingPendingSharesSwitch(t *testing.T) {
	s := newTestScheduler(t, time.Hour)
	ctx := context.Background()
	_ = s.Start(ctx)

	// 让探针暂时失败，使切换进入 pending 状态。
	s.SetProbe(fakeProbe(10))
	c1, _ := s.EnsureModel(ctx, "coding-model")
	c2, _ := s.EnsureModel(ctx, "coding-model")
	if c1 != c2 {
		t.Fatal("concurrent EnsureModel should share the same ready channel")
	}
}

func TestIdleSwitchBackToBackground(t *testing.T) {
	s := newTestScheduler(t, 50*time.Millisecond)
	ctx := context.Background()
	_ = s.Start(ctx)

	ready, _ := s.EnsureModel(ctx, "coding-model")
	<-ready
	if s.model != "coding-model" {
		t.Fatalf("expected coding model, got %s", s.model)
	}

	s.TouchCoding()
	time.Sleep(120 * time.Millisecond)
	s.ensureModel(ctx, "bg-model")
	if s.model != "bg-model" {
		t.Fatalf("expected background after idle, got %s", s.model)
	}
}

// TestLoopIdleSwitchBack 验证 Loop 中租约到期能真正触发切回默认 background：
// 租约必须大于探活周期（探活 tick 先于租约 timer 触发），回归“每轮重建
// 租约 timer 被 5s 探活 ticker 持续 Stop，idle 分支永不执行”的缺陷。
func TestLoopIdleSwitchBack(t *testing.T) {
	s := newTestScheduler(t, 300*time.Millisecond) // 租约 > 探活周期 50ms
	s.probeInterval = 50 * time.Millisecond
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := s.Start(ctx); err != nil {
		t.Fatalf("start: %v", err)
	}
	ready, _ := s.EnsureModel(ctx, "coding-model")
	<-ready
	if s.model != "coding-model" {
		t.Fatalf("expected coding model, got %s", s.model)
	}

	go s.Loop(ctx)
	ticker := time.NewTicker(50 * time.Millisecond)
	defer ticker.Stop()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		s.mu.Lock()
		m := s.model
		s.mu.Unlock()
		if m == "bg-model" {
			return
		}
		<-ticker.C
	}
	t.Fatal("Loop did not switch back to background within 3s")
}

func TestCodingActiveStateAfterSwitch(t *testing.T) {
	s := newTestScheduler(t, time.Hour)
	ctx := context.Background()
	_ = s.Start(ctx)
	ready, _ := s.EnsureModel(ctx, "coding-model")
	<-ready

	if !s.IsCodingActive() {
		t.Fatal("should be coding active")
	}
}

// TestEnsureBackground 验证 EnsureModel 在当前已是该 background 模型且就绪时
// 返回已关闭的就绪 channel，且不影响当前状态。
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
	if s.model != "bg-model" || s.IsCodingActive() {
		t.Fatalf("expected background not-coding, got model=%s codingActive=%v", s.model, s.IsCodingActive())
	}
}

// TestLocalNode 验证 LocalNode 仅当当前加载模型为就绪的 background 模式时返回节点信息。
func TestLocalNode(t *testing.T) {
	s := newTestScheduler(t, time.Hour)
	ctx := context.Background()

	// 启动前未就绪。
	if _, ok := s.LocalNode(); ok {
		t.Fatal("LocalNode must be false before start")
	}

	if err := s.Start(ctx); err != nil {
		t.Fatalf("start: %v", err)
	}
	node, ok := s.LocalNode()
	if !ok || node.BaseURL != "http://127.0.0.1:8080/v1" || node.ModelID != "bg-model" {
		t.Fatalf("LocalNode after start = (%+v, %v), want bg-model on http://127.0.0.1:8080/v1", node, ok)
	}

	// 切到 coding 后，本地节点出池。
	ready, _ := s.EnsureModel(ctx, "coding-model")
	<-ready
	if _, ok := s.LocalNode(); ok {
		t.Fatal("LocalNode must be false while in coding mode")
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
				ReadinessURL: "http://127.0.0.1:8080",
				LogFile:      logPath,
			},
		},
	}
	sw := config.SwitchConfig{DrainTimeout: time.Millisecond, KillTimeout: time.Millisecond, StartupTimeout: 500 * time.Millisecond}
	s := New(cfg, time.Hour, sw, &http.Client{})
	s.SetProbe(fakeProbe(0))
	ctx := context.Background()

	// 用真实命令写入 stdout/stderr，验证日志文件接管。
	_, err := s.ensureModel(ctx, "log-model")
	if err != nil {
		t.Fatalf("ensureModel: %v", err)
	}
	// 等待进程输出落盘。
	time.Sleep(200 * time.Millisecond)
	s.stopProcess(ctx)

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

func TestReconcileReadyAfterStartupTimeout(t *testing.T) {
	cfg := &config.SchedulingConfig{
		Models: map[string]config.ProcessConfig{
			"coding-model": {Mode: config.ModeCoding, Command: "sleep 30", ReadinessURL: "http://127.0.0.1:8080"},
		},
	}
	sw := config.SwitchConfig{DrainTimeout: time.Millisecond, KillTimeout: time.Millisecond, StartupTimeout: 300 * time.Millisecond}
	s := New(cfg, time.Hour, sw, &http.Client{})
	// 先让 probe 一直失败，触发启动超时（readyOK 保持 false）。
	s.SetProbe(func(context.Context, string) bool { return false })
	ctx := context.Background()

	if _, err := s.ensureModel(ctx, "coding-model"); err == nil {
		t.Fatal("expected startup timeout error")
	}
	if s.readyOK {
		t.Fatal("expected readyOK=false after startup timeout")
	}
	if s.ActiveBaseURL() != "" {
		t.Fatal("expected empty ActiveBaseURL before ready")
	}

	// 进程实际就绪后，reconcileReady 应恢复 readyOK 并放行等待者。
	s.SetProbe(func(context.Context, string) bool { return true })
	s.reconcileReady(ctx)

	if !s.readyOK {
		t.Fatal("expected readyOK=true after reconcile")
	}
	if got := s.ActiveBaseURL(); got != "http://127.0.0.1:8080/v1" {
		t.Fatalf("unexpected ActiveBaseURL: %q", got)
	}
	select {
	case <-s.getReadyCh():
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

func TestMarkReadyClosedConcurrent(t *testing.T) {
	cfg := &config.SchedulingConfig{
		Models: map[string]config.ProcessConfig{
			"coding-model": {Mode: config.ModeCoding, Command: "sleep 30", ReadinessURL: "http://127.0.0.1:8080"},
		},
	}
	sw := config.SwitchConfig{DrainTimeout: time.Millisecond, KillTimeout: time.Millisecond, StartupTimeout: time.Second}
	s := New(cfg, time.Hour, sw, &http.Client{})

	ch := make(chan struct{})
	s.mu.Lock()
	s.readyCh = ch
	s.mu.Unlock()

	var wg sync.WaitGroup
	// 并发对当前 ready channel 调用 markReadyClosed，不应 panic（double-close 防护）。
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			s.markReadyClosed(ch)
		}()
	}
	wg.Wait()
	select {
	case <-ch:
	default:
		t.Fatal("expected channel to be closed")
	}
}

// TestMarkReadyClosedIgnoresStaleChannel 验证被新切换覆盖的旧 channel 不会被关闭，
// 避免旧 channel 的关闭标志吞掉当前 channel 的关闭。
func TestMarkReadyClosedIgnoresStaleChannel(t *testing.T) {
	s := newTestScheduler(t, time.Hour)
	stale := make(chan struct{})
	cur := make(chan struct{})

	s.mu.Lock()
	s.readyCh = cur
	s.readyChClosed = false
	s.mu.Unlock()

	// 过期 channel 的关闭请求必须被忽略。
	s.markReadyClosed(stale)
	select {
	case <-stale:
		t.Fatal("stale channel must not be closed")
	default:
	}

	// 当前 channel 正常关闭，且重复调用安全。
	s.markReadyClosed(cur)
	s.markReadyClosed(cur)
	select {
	case <-cur:
	default:
		t.Fatal("current ready channel must be closed")
	}
}

// modelAwareProbe 返回一个按当前模型判定的就绪探测：仅当前加载模型为 coding
// 模式且 ctx 未取消时视为就绪。用于模拟“background 大模型加载极慢（永不就绪）、
// coding 秒就绪”。
func modelAwareProbe(s *Scheduler) func(context.Context, string) bool {
	return func(ctx context.Context, string string) bool {
		if ctx.Err() != nil {
			return false
		}
		s.mu.Lock()
		model := s.model
		s.mu.Unlock()
		return s.modelModeLocked(model) == config.ModeCoding
	}
}

// waitSwitchInflight 轮询等待指定模型 ID 的切换进入就绪等待阶段（switchTarget 已登记）。
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
			t.Fatalf("%s switch did not enter readiness wait in time", target)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// TestCodingPreemptsBackgroundStartup 验证方案 C：代理启动后 background 大模型仍在
// 加载（永不就绪），此时 coding 请求到达应直接抢占 background 加载，而不是排队等
// 它加载完。coding 应立即就绪，且 Start 不得把这种抢占当致命错误（否则 main.go
// 会 Fatalf 杀掉代理）。
func TestCodingPreemptsBackgroundStartup(t *testing.T) {
	cfg := &config.SchedulingConfig{
		Models: map[string]config.ProcessConfig{
			"coding-model": {Mode: config.ModeCoding, Command: "", ReadinessURL: "http://127.0.0.1:8080", APIKey: "coding-key"},
			"bg-model":     {Mode: config.ModeBackground, Command: "", ReadinessURL: "http://127.0.0.1:8080", APIKey: "bg-key"},
		},
	}
	sw := config.SwitchConfig{DrainTimeout: time.Millisecond, KillTimeout: time.Millisecond, StartupTimeout: 3 * time.Second}
	s := New(cfg, time.Hour, sw, &http.Client{})
	s.SetProbe(modelAwareProbe(s)) // background 永不就绪，coding 立即就绪
	ctx := context.Background()

	startDone := make(chan error, 1)
	go func() { startDone <- s.Start(ctx) }()

	// 等 background 切换进入就绪等待（模拟大模型仍在加载）。
	waitSwitchInflight(t, s, "bg-model")

	// coding 请求到达：必须插队抢占，而不是等 background 加载完（3s）。
	t0 := time.Now()
	ready, err := s.EnsureModel(ctx, "coding-model")
	if err != nil {
		t.Fatalf("ensure coding: %v", err)
	}
	select {
	case <-ready:
	case <-time.After(2 * time.Second):
		t.Fatal("coding ready channel not closed promptly after preemption")
	}
	if d := time.Since(t0); d > 2*time.Second {
		t.Errorf("EnsureModel took %v; preemption should not wait out the 3s background load", d)
	}

	if s.model != "coding-model" || !s.readyOK {
		t.Fatalf("expected coding ready, got model=%s ready=%v", s.model, s.readyOK)
	}
	if got := s.ActiveAPIKey(); got != "coding-key" {
		t.Fatalf("ActiveAPIKey=%q, want coding-key", got)
	}

	// Start 必须把"首次默认 background 加载被抢占"视为非致命并返回 nil。
	select {
	case err := <-startDone:
		if err != nil {
			t.Fatalf("Start should not fail when initial background load is preempted: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Start did not return after preemption")
	}
}

// TestCodingPreemptsIdleSwitchBack 验证方案 C 的另一半：运行期空闲切回 background
// 的切换在加载期间被新的 coding 请求抢占，background 切换应快速以"被抢占"错误返回，
// coding 切换随即完成。
func TestCodingPreemptsIdleSwitchBack(t *testing.T) {
	// 短租约：空闲切回 background 只在租约过期后才发生（与 Loop 的触发条件一致）。
	s := newTestScheduler(t, 50*time.Millisecond)
	ctx := context.Background()

	// 先让 coding 就绪。
	ready, err := s.ensureModel(ctx, "coding-model")
	if err != nil {
		t.Fatalf("ensure coding: %v", err)
	}
	select {
	case <-ready:
	case <-time.After(2 * time.Second):
		t.Fatal("initial coding did not become ready")
	}
	if s.model != "coding-model" || !s.readyOK {
		t.Fatalf("expected coding ready, got model=%s ready=%v", s.model, s.readyOK)
	}

	// 等 coding 租约过期，空闲切回 background 才合法（否则会被租约保护跳过）。
	time.Sleep(120 * time.Millisecond)

	// background 永不就绪（模拟大模型加载慢）。
	s.SetProbe(modelAwareProbe(s))

	bgErrCh := make(chan error, 1)
	go func() {
		_, e := s.ensureModel(ctx, "bg-model")
		bgErrCh <- e
	}()
	waitSwitchInflight(t, s, "bg-model")

	// coding 请求到达，抢占在途的 background 切换。
	t0 := time.Now()
	ready2, err := s.EnsureModel(ctx, "coding-model")
	if err != nil {
		t.Fatalf("ensure coding: %v", err)
	}
	select {
	case <-ready2:
	case <-time.After(2 * time.Second):
		t.Fatal("coding ready channel not closed promptly after preemption")
	}
	if d := time.Since(t0); d > 2*time.Second {
		t.Errorf("EnsureModel took %v; preemption should cut the background load short", d)
	}

	select {
	case bgErr := <-bgErrCh:
		if bgErr == nil || !strings.Contains(bgErr.Error(), "preempted") {
			t.Errorf("background switch should report preemption, got: %v", bgErr)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("background switch did not return after preemption")
	}

	if s.model != "coding-model" || !s.readyOK {
		t.Fatalf("expected coding ready, got model=%s ready=%v", s.model, s.readyOK)
	}
	if got := s.ActiveAPIKey(); got != "coding-key" {
		t.Fatalf("ActiveAPIKey=%q, want coding-key", got)
	}
}

// TestStartBackgroundThenEnsureCodingStaleClose 复现生产事故：代理启动时
// background 切换先完成并关闭其 ready channel 后，首个 coding 请求再触发切换。
// 旧实现中 background 的关闭标志（readyChClosed）吞掉了 coding channel 的关闭，
// 导致所有 coding 请求永久挂起。
func TestStartBackgroundThenEnsureCodingStaleClose(t *testing.T) {
	s := newTestScheduler(t, time.Hour)
	ctx := context.Background()

	var probeMu sync.Mutex
	probeOK := false
	s.SetProbe(func(context.Context, string) bool {
		probeMu.Lock()
		defer probeMu.Unlock()
		return probeOK
	})

	// 启动 background 切换（模拟 Start），初始探针失败使其等待。
	bgDone := make(chan struct{})
	go func() {
		defer close(bgDone)
		_, _ = s.ensureModel(ctx, "bg-model")
	}()
	time.Sleep(50 * time.Millisecond)

	// 让 background 就绪并等待其切换完成。
	probeMu.Lock()
	probeOK = true
	probeMu.Unlock()
	<-bgDone

	if s.model != "bg-model" || !s.readyOK {
		t.Fatalf("expected background ready, got model=%s ready=%v", s.model, s.readyOK)
	}

	// 现在触发 coding 切换：channel 必须被关闭，不得永久阻塞。
	ready, err := s.EnsureModel(ctx, "coding-model")
	if err != nil {
		t.Fatalf("ensure coding: %v", err)
	}
	select {
	case <-ready:
	case <-time.After(2 * time.Second):
		t.Fatal("coding ready channel not closed after background switch")
	}

	if s.model != "coding-model" {
		t.Fatalf("expected coding model, got %s", s.model)
	}
	if !s.readyOK {
		t.Fatal("expected readyOK=true")
	}
	select {
	case <-s.getReadyCh():
	default:
		t.Fatal("expected current ready channel to be closed")
	}
}
