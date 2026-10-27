package main

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"llama_proxy/internal/config"
)

func doGoClawProxy(t *testing.T, proxyURL, ua string) int {
	t.Helper()
	req, _ := http.NewRequest(http.MethodPost, proxyURL+"/v1/chat/completions",
		strings.NewReader(`{"model":"llm_proxy","messages":[]}`))
	req.Header.Set("Content-Type", "application/json")
	if ua != "" {
		req.Header.Set("User-Agent", ua)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("proxy do: %v", err)
	}
	defer resp.Body.Close()
	return resp.StatusCode
}

// TestGoClawToolCallRouting 验证配置了 User-Agent 规则后：GoClaw 请求
// 只路由到带 tool_call 标签的后端；UA 不包含配置子串时不命中规则。
func TestGoClawToolCallRouting(t *testing.T) {
	tagged, taggedRec := newRecordingBackend()
	plain, plainRec := newRecordingBackend()
	defer tagged.Close()
	defer plain.Close()

	backends := []config.BackendConfig{
		{Name: "tagged", URL: tagged.URL, Weight: 1, Tags: []string{"tool_call"}},
		{Name: "plain", URL: plain.URL, Weight: 1},
	}
	routing := &config.RoutingConfig{Rules: []config.RoutingRule{{
		Name:   "goclaw",
		Header: map[string]string{"User-Agent": "GoClaw/2.1"},
		Pool:   "tool_call",
	}}}
	svc, cleanup := newRoutingTestServer(t, backends, routing)
	defer cleanup()
	proxy := httptest.NewServer(svc)
	defer proxy.Close()

	// UA 包含 GoClaw/2.1 → 命中规则 → 只去 tagged
	for i := 0; i < 6; i++ {
		if code := doGoClawProxy(t, proxy.URL, "GoClaw/2.1"); code != http.StatusOK {
			t.Fatalf("GoClaw request #%d status=%d, want 200", i, code)
		}
	}
	// UA 不包含 GoClaw/2.1 → 不命中规则，走全量池
	if code := doGoClawProxy(t, proxy.URL, "GoClaw/2.2"); code != http.StatusOK {
		t.Fatalf("non-matching UA request status=%d, want 200", code)
	}
	// 普通 UA：全量池 rr，两个后端各一半
	for i := 0; i < 3; i++ {
		if code := doGoClawProxy(t, proxy.URL, "curl/8.5"); code != http.StatusOK {
			t.Fatalf("normal request #%d status=%d, want 200", i, code)
		}
	}

	tc, _ := taggedRec.snapshot()
	pc, _ := plainRec.snapshot()
	// GoClaw/2.1 共 6 个，全部命中 tagged；
	// 普通流量 4 个（UA 不匹配 1 + curl 3）走全量池 {tagged, plain}，rr 各半（tagged 2 / plain 2）。
	// 若 GoClaw/2.1 泄漏到 plain，pc 会大于 2。
	if tc != 8 {
		t.Fatalf("tagged received=%d, want 8 (6 GoClaw + 2 normal rr share)", tc)
	}
	if pc != 2 {
		t.Fatalf("plain received=%d, want 2 (only normal rr share, no GoClaw)", pc)
	}
}

// TestGoClawNoToolCallBackend 验证命中规则但池中没有任何 tool_call 后端时，请求返回 400。
func TestGoClawNoToolCallBackend(t *testing.T) {
	backend, _ := newRecordingBackend()
	defer backend.Close()
	routing := &config.RoutingConfig{Rules: []config.RoutingRule{{
		Name:   "goclaw",
		Header: map[string]string{"User-Agent": "GoClaw/2.1"},
		Pool:   "tool_call",
	}}}
	svc, cleanup := newRoutingTestServer(t, []config.BackendConfig{{Name: "b", URL: backend.URL, Weight: 1}}, routing)
	defer cleanup()

	proxy := httptest.NewServer(svc)
	defer proxy.Close()

	req, _ := http.NewRequest(http.MethodPost, proxy.URL+"/v1/chat/completions",
		strings.NewReader(`{"model":"llm_proxy","messages":[]}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "GoClaw/2.1")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("proxy do: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status=%d, want 400", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(body), "tool_call") {
		t.Fatalf("error body=%s, want mention of tool_call", body)
	}
}

// newRoutingTestServer 构建带 routing 规则配置的测试 Server（无调度器，纯转发 + 路由规则）。
func TestGoClawFailoverStaysInToolCallPool(t *testing.T) {
	tc2 := okJSONBackend(t, `{"model":"tc2-model","content":"ok"}`)
	defer tc2.Close()
	plain := okJSONBackend(t, `{"model":"plain-model","content":"ok"}`)
	defer plain.Close()

	routing := &config.RoutingConfig{Rules: []config.RoutingRule{{
		Name:   "goclaw",
		Header: map[string]string{"User-Agent": "GoClaw/2.1"},
		Pool:   "tool_call",
	}}}
	svc, cleanup := newRoutingTestServer(t, []config.BackendConfig{
		{Name: "plain", URL: plain.URL, Weight: 1},
		{Name: "tc-1", URL: deadBackendURL(t), Weight: 1, Tags: []string{"tool_call"}},
		{Name: "tc-2", URL: tc2.URL, Weight: 1, Tags: []string{"tool_call"}},
	}, routing)
	defer cleanup()

	proxy := httptest.NewServer(svc)
	defer proxy.Close()

	req, _ := http.NewRequest(http.MethodPost, proxy.URL+"/v1/chat/completions",
		strings.NewReader(`{"model":"llm_proxy","messages":[]}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "GoClaw/2.1")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("proxy do: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status=%d, want 200 after failover within tool_call pool", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(body), "tc2-model") || strings.Contains(string(body), "plain-model") {
		t.Fatalf("body=%s, want response from tc-2, not the plain backend", body)
	}
}
