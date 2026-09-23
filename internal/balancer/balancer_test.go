package balancer

import (
	"fmt"
	"testing"

	"llama_proxy/internal/config"
)

func TestBackendBalancerWeightedSelect(t *testing.T) {
	backends := []config.BackendConfig{
		{Name: "a", URL: "http://a:8080", Weight: 50},
		{Name: "b", URL: "http://b:8080", Weight: 30},
		{Name: "c", URL: "http://c:8080", Weight: 20},
	}

	bb := New(backends, "wrr")
	if bb.Len() != 3 {
		t.Fatalf("len=%d", bb.Len())
	}
	if bb.GetStrategy() != "wrr" {
		t.Fatalf("strategy=%q", bb.GetStrategy())
	}

	counts := map[string]int{}
	for i := 0; i < 1000; i++ {
		b := bb.Select()
		if b == nil {
			t.Fatal("select returned nil")
		}
		counts[b.Name]++
	}

	if counts["a"] < 400 {
		t.Fatalf("backend a selected too few times: %v", counts)
	}
	if counts["b"] < 200 {
		t.Fatalf("backend b selected too few times: %v", counts)
	}
	if counts["c"] < 100 {
		t.Fatalf("backend c selected too few times: %v", counts)
	}
	if counts["a"]+counts["b"]+counts["c"] != 1000 {
		t.Fatalf("total=%v", counts)
	}
}

func TestBackendBalancerEmptyPool(t *testing.T) {
	bb := New(nil, "wrr")
	if bb.Len() != 0 {
		t.Fatalf("len=%d", bb.Len())
	}
	if bb.Select() != nil {
		t.Fatal("select on empty pool should be nil")
	}
	if bb.SelectFromTag("tool_call") != nil {
		t.Fatal("select tool_call on empty pool should be nil")
	}
}

// TestSelectToolCall 验证 tool_call 子池：只选带 tool_call 标签的后端，
// 且子池随 Update 重建。
func TestSelectToolCall(t *testing.T) {
	backends := []config.BackendConfig{
		{Name: "a", URL: "http://a:8080", Weight: 1, Tags: []string{"tool_call"}},
		{Name: "b", URL: "http://b:8080", Weight: 1, Tags: []string{"tool_call"}},
		{Name: "c", URL: "http://c:8080", Weight: 1}, // 不支持工具调用
	}
	bb := New(backends, "rr")

	counts := map[string]int{}
	for i := 0; i < 20; i++ {
		b := bb.SelectFromTag("tool_call")
		if b == nil {
			t.Fatal("select tool_call returned nil")
		}
		counts[b.Name]++
	}
	if counts["a"] == 0 || counts["b"] == 0 {
		t.Fatalf("tool_call sub-pool should include a and b: %v", counts)
	}
	if counts["c"] != 0 {
		t.Fatalf("non tool_call backend c was selected: %v", counts)
	}
	// 全量池不受影响：Select 仍可在 a/b/c 间选择
	if got := bb.Select(); got == nil {
		t.Fatal("full pool select should not be nil")
	}

	// 子池随 Update 重建：只剩 c 且支持工具调用
	bb.Update([]config.BackendConfig{{Name: "c", URL: "http://c:8080", Weight: 1, Tags: []string{"tool_call"}}})
	for i := 0; i < 5; i++ {
		if b := bb.SelectFromTag("tool_call"); b == nil || b.Name != "c" {
			t.Fatalf("after update select tool_call=%v, want c", b)
		}
	}

	// 池中没有任何 tool_call 后端时返回 nil
	bb.Update([]config.BackendConfig{{Name: "c", URL: "http://c:8080", Weight: 1}})
	if bb.SelectFromTag("tool_call") != nil {
		t.Fatal("expected nil when no tool_call backend")
	}
}

// TestSelectExcluding 验证 SelectExcluding 跳过已排除的后端，全部排除时返回 nil。
func TestSelectExcluding(t *testing.T) {
	backends := []config.BackendConfig{
		{Name: "a", URL: "http://a:8080", Weight: 1},
		{Name: "b", URL: "http://b:8080", Weight: 1},
		{Name: "c", URL: "http://c:8080", Weight: 1},
	}
	bb := New(backends, "rr")
	first := bb.Select()
	if first == nil {
		t.Fatal("first select returned nil")
	}
	for i := 0; i < 10; i++ {
		got := bb.SelectExcluding(map[string]bool{first.Name: true})
		if got == nil {
			t.Fatalf("select excluding returned nil (excluded %s)", first.Name)
		}
		if got.Name == first.Name {
			t.Fatalf("excluded backend was returned: %s", got.Name)
		}
	}
	if got := bb.SelectExcluding(map[string]bool{"a": true, "b": true, "c": true}); got != nil {
		t.Fatalf("expected nil when all backends excluded, got %s", got.Name)
	}
}

// TestSelectToolCallExcluding 验证 tool_call 子池的排除选点：跳过已排除后端，
// 且不选中不带 tool_call 标签的后端；子池全部排除时返回 nil。
func TestSelectToolCallExcluding(t *testing.T) {
	backends := []config.BackendConfig{
		{Name: "a", URL: "http://a:8080", Weight: 1, Tags: []string{"tool_call"}},
		{Name: "b", URL: "http://b:8080", Weight: 1, Tags: []string{"tool_call"}},
		{Name: "c", URL: "http://c:8080", Weight: 1}, // 不支持工具调用
	}
	bb := New(backends, "rr")
	first := bb.SelectFromTag("tool_call")
	if first == nil {
		t.Fatal("first tool_call select returned nil")
	}
	for i := 0; i < 10; i++ {
		got := bb.SelectFromTagExcluding("tool_call", map[string]bool{first.Name: true})
		if got == nil {
			t.Fatalf("select tool_call excluding returned nil (excluded %s)", first.Name)
		}
		if got.Name == first.Name || !got.EffectiveTags()["tool_call"] {
			t.Fatalf("bad selection: %+v", got)
		}
	}
	if got := bb.SelectFromTagExcluding("tool_call", map[string]bool{"a": true, "b": true}); got != nil {
		t.Fatalf("expected nil when all tool_call backends excluded, got %s", got.Name)
	}
}

// TestSelectFromTag 验证标签子池按 tags 与 tool_call 的并集划分，且随 Update 重建。
func TestSelectFromTag(t *testing.T) {
	backends := []config.BackendConfig{
		{Name: "a", URL: "http://a:8080", Weight: 1, Tags: []string{"fast"}},
		{Name: "b", URL: "http://b:8080", Weight: 1, Tags: []string{"fast", "tool_call"}},
		{Name: "c", URL: "http://c:8080", Weight: 1},
	}
	bb := New(backends, "rr")

	counts := map[string]int{}
	for i := 0; i < 20; i++ {
		b := bb.SelectFromTag("fast")
		if b == nil {
			t.Fatal("select tag fast returned nil")
		}
		counts[b.Name]++
	}
	if counts["a"] == 0 || counts["b"] == 0 {
		t.Fatalf("fast sub-pool should include a and b: %v", counts)
	}
	if counts["c"] != 0 {
		t.Fatalf("non-fast backend c was selected: %v", counts)
	}

	// tool_call 标签后端进入 tool_call 子池
	if b := bb.SelectFromTag("tool_call"); b == nil || b.Name != "b" {
		t.Fatalf("select tool_call=%v, want b", b)
	}

	// 排除：跳过 a 只剩 b；全部排除返回 nil；未知标签返回 nil
	if b := bb.SelectFromTagExcluding("fast", map[string]bool{"a": true}); b == nil || b.Name != "b" {
		t.Fatalf("excluding a from fast pool got %v, want b", b)
	}
	if bb.SelectFromTagExcluding("fast", map[string]bool{"a": true, "b": true}) != nil {
		t.Fatal("expected nil when all fast backends excluded")
	}
	if bb.SelectFromTag("no-such-tag") != nil {
		t.Fatal("expected nil for unknown tag")
	}

	// 子池随 Update 重建：a 去掉标签后出池
	bb.Update([]config.BackendConfig{
		{Name: "a", URL: "http://a:8080", Weight: 1},
		{Name: "b", URL: "http://b:8080", Weight: 1, Tags: []string{"fast", "tool_call"}},
	})
	for i := 0; i < 5; i++ {
		if b := bb.SelectFromTag("fast"); b == nil || b.Name != "b" {
			t.Fatalf("after update fast pool select=%v, want b", b)
		}
	}
}

// TestPoolSummary 验证池摘要日志：strategy + 全量池与各标签池的成员及权重（确定性排序）。
func TestPoolSummary(t *testing.T) {
	var lines []string
	backends := []config.BackendConfig{
		{Name: "a", URL: "http://a:8080", Weight: 5, Tags: []string{"fast"}},
		{Name: "b", URL: "http://b:8080", Weight: 3, Tags: []string{"fast", "tool_call"}},
		{Name: "c", URL: "http://c:8080", Weight: 1},
	}
	bb := New(backends, "wrr")
	bb.SetLogger(func(format string, args ...any) {
		lines = append(lines, fmt.Sprintf(format, args...))
	})
	bb.LogPools()
	bb.Update(backends)

	want := "backend pool: strategy=wrr all=a(w=5),b(w=3),c(w=1) fast=a(w=5),b(w=3) tool_call=b(w=3)"
	if len(lines) != 2 {
		t.Fatalf("got %d log lines, want 2: %v", len(lines), lines)
	}
	for i, got := range lines {
		if got != want {
			t.Fatalf("line %d=%q, want %q", i, got, want)
		}
	}
}
