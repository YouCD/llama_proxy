package balancer

import (
	"testing"

	"llama_proxy/internal/config"
)

func twoNodePool() []config.BackendConfig {
	return []config.BackendConfig{
		{Name: "a", URL: "http://a:8080", Weight: 1},
		{Name: "b", URL: "http://b:8080", Weight: 1},
	}
}

// TestLeastLoadedAvoidsBusyNode 验证负载感知核心：a 上有 3 条在途时，
// 选点始终避开 a 落到 b；Leave 清空后恢复轮换。
func TestLeastLoadedAvoidsBusyNode(t *testing.T) {
	bb := New(twoNodePool(), "wrr")
	bb.Enter("a")
	bb.Enter("a")
	bb.Enter("a")
	for i := 0; i < 20; i++ {
		if b := bb.Select(); b == nil || b.Name != "b" {
			t.Fatalf("with a busy (3 inflight), select=%v, want always b", b)
		}
	}
	// 清空 a 的在途后恢复公平轮换：两个节点都应被选到。
	for i := 0; i < 3; i++ {
		bb.Leave("a")
	}
	counts := map[string]int{}
	for i := 0; i < 20; i++ {
		counts[bb.Select().Name]++
	}
	if counts["a"] == 0 || counts["b"] == 0 {
		t.Fatalf("after leaves, expected rotation over both nodes: %v", counts)
	}
}

// TestLeastLoadedPicksIdleOverBusy 不等权场景：低权重但在途为 0 的节点，
// 优先于高权重但繁忙的节点（负载优先于权重）。
func TestLeastLoadedPicksIdleOverBusy(t *testing.T) {
	backends := []config.BackendConfig{
		{Name: "a", URL: "http://a:8080", Weight: 10},
		{Name: "b", URL: "http://b:8080", Weight: 1},
	}
	bb := New(backends, "wrr")
	bb.Enter("a")
	bb.Enter("a")
	// a 在途 2 > b 在途 0：即使 a 权重高 10 倍，也应选 b。
	if b := bb.Select(); b == nil || b.Name != "b" {
		t.Fatalf("idle low-weight node should beat busy high-weight node, got %v", b)
	}
}

// TestEnterLeaveUnknownNode 未知节点名（不在池内，如 coding 模型直连）被忽略，
// 不影响计数也不 panic。
func TestEnterLeaveUnknownNode(t *testing.T) {
	bb := New(twoNodePool(), "wrr")
	bb.Enter("local-coding-model")
	if bb.InflightCount("local-coding-model") != 0 {
		t.Fatal("unknown node should not be counted")
	}
	bb.Leave("local-coding-model")
	bb.Leave("")
	if bb.InflightCount("a") != 0 || bb.InflightCount("b") != 0 {
		t.Fatal("unknown-node enter/leave must not affect known nodes")
	}
}

// TestInflightSurvivesUpdate 池同步（Update）保留存活节点的在途计数、清除已出池
// 节点的计数——本地节点每秒同步不能把正在转发的请求负载清零。
func TestInflightSurvivesUpdate(t *testing.T) {
	bb := New(twoNodePool(), "wrr")
	bb.Enter("a")
	bb.Enter("a")
	// 同池 Update：a 的计数保留。
	bb.Update(twoNodePool())
	if got := bb.InflightCount("a"); got != 2 {
		t.Fatalf("inflight not preserved across update: got %d, want 2", got)
	}
	// a 出池：其计数被清除。
	bb.Update([]config.BackendConfig{{Name: "b", URL: "http://b:8080", Weight: 1}})
	if got := bb.InflightCount("a"); got != 0 {
		t.Fatalf("removed node's inflight not cleared: got %d", got)
	}
}

// TestLeastLoadedTagPool 标签子池内同样负载感知：fast 子池里 a 繁忙时只选 b。
func TestLeastLoadedTagPool(t *testing.T) {
	backends := []config.BackendConfig{
		{Name: "a", URL: "http://a:8080", Weight: 1, Tags: []string{"fast"}},
		{Name: "b", URL: "http://b:8080", Weight: 1, Tags: []string{"fast"}},
		{Name: "c", URL: "http://c:8080", Weight: 1},
	}
	bb := New(backends, "rr")
	bb.Enter("a")
	for i := 0; i < 10; i++ {
		if b := bb.SelectFromTag("fast"); b == nil || b.Name != "b" {
			t.Fatalf("fast pool should avoid busy a, got %v", b)
		}
	}
	// c 不在 fast 池，其负载不影响 fast 子池选点。
	bb.Enter("c")
	bb.Enter("c")
	if b := bb.SelectFromTag("fast"); b == nil || b.Name != "b" {
		t.Fatalf("non-pool node load must not affect tag pool, got %v", b)
	}
}

// TestSelectExcludingRespectsLoad 故障转移排除 + 负载感知叠加：
//   - 排除 a 后 b 是唯一可用节点，即使繁忙也必须接（负载感知是多选一，不是拒绝服务）；
//   - 三节点中 a 已试失败、c 繁忙、b 空闲 → 选 b；
//   - 全部排除 → nil。
func TestSelectExcludingRespectsLoad(t *testing.T) {
	bb := New(twoNodePool(), "wrr")
	bb.Enter("b")
	if got := bb.SelectExcluding(map[string]bool{"a": true}); got == nil || got.Name != "b" {
		t.Fatalf("sole eligible node must be picked even when busy, got %v", got)
	}
	bb.Leave("b")

	three := []config.BackendConfig{
		{Name: "a", URL: "http://a:8080", Weight: 1},
		{Name: "b", URL: "http://b:8080", Weight: 1},
		{Name: "c", URL: "http://c:8080", Weight: 1},
	}
	bb3 := New(three, "wrr")
	bb3.Enter("c")
	bb3.Enter("c")
	if got := bb3.SelectExcluding(map[string]bool{"a": true}); got == nil || got.Name != "b" {
		t.Fatalf("failover should pick idle b over busy c, got %v", got)
	}
	if got := bb3.SelectExcluding(map[string]bool{"a": true, "b": true, "c": true}); got != nil {
		t.Fatalf("want nil when all excluded, got %v", got)
	}
}
