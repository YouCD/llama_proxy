package scheduler

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"syscall"
	"testing"
	"time"

	"llama_proxy/internal/config"
)

// freePort 申请一个当前空闲的 TCP 端口（监听后立即释放）。
func freePort(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	port := l.Addr().(*net.TCPAddr).Port
	_ = l.Close()
	return port
}

func TestReadinessPort(t *testing.T) {
	cases := map[string]int{
		"http://127.0.0.1:8000/v1/models": 8000,
		"http://127.0.0.1:8000/health":    8000,
		"http://10.0.0.2:9123":            9123,
		"":                                0,
		"not a url":                       0,
	}
	for in, want := range cases {
		if got := readinessPort(&config.ProcessConfig{ReadinessURL: in}); got != want {
			t.Errorf("readinessPort(%q) = %d, want %d", in, got, want)
		}
	}
}

func TestResolveServerPIDSelf(t *testing.T) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer l.Close()
	port := l.Addr().(*net.TCPAddr).Port
	pid, err := resolveServerPID(port)
	if err != nil {
		t.Fatalf("resolveServerPID: %v", err)
	}
	if pid != os.Getpid() {
		t.Fatalf("resolveServerPID = %d, want self %d", pid, os.Getpid())
	}
}

func TestResolveServerPIDFreePort(t *testing.T) {
	port := freePort(t)
	if _, err := resolveServerPID(port); err == nil {
		t.Fatal("expected error for free port, got none")
	}
	if !portListenerGone(port) {
		t.Fatal("portListenerGone = false for free port")
	}
}

// TestKillTreeGroupLeader 验证 setsid 派生的进程（自身为进程组 leader）被整组终止。
func TestKillTreeGroupLeader(t *testing.T) {
	cmd := exec.Command("sleep", "30")
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		t.Skipf("start: %v", err)
	}
	defer func() { _ = cmd.Process.Kill(); _ = cmd.Wait() }()
	time.Sleep(150 * time.Millisecond)
	pid := cmd.Process.Pid
	group, err := procGroupID(pid)
	if err != nil {
		t.Fatalf("procGroupID: %v", err)
	}
	if group != pid {
		t.Fatalf("setsid child should be its own group leader: pid=%d group=%d", pid, group)
	}
	if err := killTree(pid, group, syscall.SIGTERM); err != nil {
		t.Fatalf("killTree: %v", err)
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("process not terminated by killTree")
	}
}

// TestKillTreeDescendants 验证非组 leader 目标按后代递归终止（sh 及其后台 sleep）。
func TestKillTreeDescendants(t *testing.T) {
	cmd := exec.Command("sh", "-c", "sleep 30 & wait")
	if err := cmd.Start(); err != nil {
		t.Skipf("start: %v", err)
	}
	time.Sleep(200 * time.Millisecond)
	pid := cmd.Process.Pid
	group, err := procGroupID(pid)
	if err != nil {
		t.Fatalf("procGroupID: %v", err)
	}
	if group == pid {
		t.Skipf("child unexpectedly its own group leader")
	}
	children := processChildren(pid)
	if len(children) != 1 {
		t.Fatalf("expected 1 child (sleep), got %d", len(children))
	}
	sleepPID := children[0]
	if err := killTree(pid, group, syscall.SIGTERM); err != nil {
		t.Fatalf("killTree: %v", err)
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("sh not terminated by killTree")
	}
	deadline := time.Now().Add(3 * time.Second)
	for pidAlive(sleepPID) && time.Now().Before(deadline) {
		time.Sleep(50 * time.Millisecond)
	}
	if pidAlive(sleepPID) {
		t.Fatalf("descendant sleep pid=%d survived killTree", sleepPID)
	}
}

// detachedCommand 模拟 vLLM launcher 的行为：以 setsid 派生独立会话的服务进程后
// 包装脚本立即退出（服务存活、脱离 exec.Cmd 的进程树）。
// 注意：Python 3.14 的 http.server 把 -p 改为协议版本，端口是位置参数。
func detachedCommand(port int) string {
	return fmt.Sprintf(
		"setsid python3 -m http.server --bind 127.0.0.1 %d </dev/null >/dev/null 2>&1 &", port)
}

// TestSwitchKillsDetachedServer 端到端回归：包装脚本退出后调度器仍能接管真实服务、
// 正确展示其 PID、切换模型时真正杀掉脱离的服务并释放端口。
func TestSwitchKillsDetachedServer(t *testing.T) {
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("python3 not available")
	}
	if _, err := exec.LookPath("setsid"); err != nil {
		t.Skip("setsid not available")
	}
	port := freePort(t)
	cfg := &config.SchedulingConfig{
		Models: map[string]config.ProcessConfig{
			"detached-a": {
				Mode:         config.ModeCoding,
				Command:      detachedCommand(port),
				ReadinessURL: fmt.Sprintf("http://127.0.0.1:%d/v1/models", port),
			},
			"detached-b": {
				Mode:         config.ModeBackground,
				Command:      detachedCommand(port),
				ReadinessURL: fmt.Sprintf("http://127.0.0.1:%d/v1/models", port),
			},
		},
		Default: []string{"detached-b"},
	}
	sw := config.SwitchConfig{DrainTimeout: 200 * time.Millisecond, KillTimeout: 10 * time.Second, StartupTimeout: 30 * time.Second}
	s := New(cfg, time.Hour, sw, &http.Client{})
	// 不注入 fake probe：用真实 HTTP 探测（http.server 对任意路径返回 4xx，
	// probeReady 视 <500 为就绪）。
	ctx := context.Background()
	t.Cleanup(s.Shutdown)

	if _, err := s.ensureCoding(ctx, "detached-a"); err != nil {
		t.Fatalf("ensureCoding: %v", err)
	}
	// 等待包装脚本退出且真实服务被接管（reap 同步接管，快照 PID 即服务 PID）。
	var pidA int
	deadline := time.Now().Add(15 * time.Second)
	for {
		for _, m := range s.Snapshot().Models {
			if m.ID == "detached-a" {
				pidA = m.PID
			}
		}
		if pidA > 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("detached server pid never adopted")
		}
		time.Sleep(200 * time.Millisecond)
	}
	if !pidAlive(pidA) {
		t.Fatal("detached server not alive after adoption")
	}
	// 接管后再次请求同模型：不应触发重新切换（直接放行）。
	if ch, err := s.ensureCoding(ctx, "detached-a"); err != nil {
		t.Fatalf("ensureCoding again: %v", err)
	} else {
		select {
		case <-ch:
		case <-time.After(2 * time.Second):
			t.Fatal("re-ensure did not pass through without restart")
		}
	}
	if !pidAlive(pidA) {
		t.Fatal("re-ensure unexpectedly restarted the server")
	}
	// 切换到同端口的另一模型：必须真正杀掉已脱离的服务并释放端口。
	if _, err := s.switchTo(ctx, []string{"detached-b"}, config.ModeBackground); err != nil {
		t.Fatalf("switchTo detached-b: %v", err)
	}
	if pidAlive(pidA) {
		t.Fatal("old detached server survived the switch")
	}
	// 端口此时应被新模型的服务持有（而非旧进程）。
	if owner, err := resolveServerPID(port); err != nil || owner == pidA {
		t.Fatalf("port owner after switch = %d (err=%v), want new server != old %d", owner, err, pidA)
	}
	for _, m := range s.Snapshot().Models {
		if m.ID == "detached-b" && !m.Ready {
			t.Fatal("detached-b not ready after switch")
		}
	}
}
