package main

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"llama_proxy/internal/balancer"
	"llama_proxy/internal/config"
	"llama_proxy/internal/model"
)

func deadBackendURL(t *testing.T) string {
	t.Helper()
	dead := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, "unreachable")
	}))
	u := dead.URL
	dead.Close()
	return u
}

// okJSONBackend 返回一个恒定回 200 JSON 的后端。
func okJSONBackend(t *testing.T, payload string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, payload)
	}))
	return srv
}

// waitForRecords 轮询请求记录，直到数量与状态码满足条件（FinishRequest 在响应体
// 传完之后才执行，直接查询存在竞态）。
func waitForRecords(t *testing.T, svc *Server, n int, wantStatus int) []model.RequestRecord {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		recs, err := svc.store.GetRequests(100, 0, model.RequestFilter{})
		if err != nil {
			t.Fatalf("getRequests: %v", err)
		}
		if len(recs) == n && recs[0].StatusCode == wantStatus {
			return recs
		}
		time.Sleep(20 * time.Millisecond)
	}
	recs, _ := svc.store.GetRequests(100, 0, model.RequestFilter{})
	t.Fatalf("timed out waiting for %d record(s) with status %d, got %+v", n, wantStatus, recs)
	return nil
}

func postCompletion(t *testing.T, proxyURL string, extraHeaders map[string]string) *http.Response {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, proxyURL+"/v1/completions",
		strings.NewReader(`{"model":"llm_proxy","prompt":"hi"}`))
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	for k, v := range extraHeaders {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("proxy do: %v", err)
	}
	return resp
}

// TestHandleProxyFailoverOnConnectionError 验证首个后端连接失败时自动转移到下一个
// 后端，且落库记录指向实际服务的后端。
func TestHandleProxyFailoverOnConnectionError(t *testing.T) {
	ok := okJSONBackend(t, `{"model":"ok-model","content":"ok"}`)
	defer ok.Close()

	svc, _, cleanup := newTestServer(t, ok.URL)
	defer cleanup()
	// rr 策略 + 字典序保证先选中 a-down。
	svc.balancer = balancer.New([]config.BackendConfig{
		{Name: "a-down", URL: deadBackendURL(t), Weight: 1},
		{Name: "b-ok", URL: ok.URL, Weight: 1},
	}, "rr")

	proxy := httptest.NewServer(svc)
	defer proxy.Close()

	resp := postCompletion(t, proxy.URL, nil)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status=%d, want 200 after failover", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(body), "ok-model") {
		t.Fatalf("body=%s, want response from the healthy backend", body)
	}

	recs := waitForRecords(t, svc, 1, http.StatusOK)
	if recs[0].BackendURL != ok.URL {
		t.Fatalf("record backend=%q, want %q", recs[0].BackendURL, ok.URL)
	}
}

// TestHandleProxyFailoverOn5xx 验证首个后端返回 5xx 时自动转移到下一个后端。
func TestHandleProxyFailoverOn5xx(t *testing.T) {
	failing := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	}))
	defer failing.Close()
	ok := okJSONBackend(t, `{"model":"ok-model","content":"ok"}`)
	defer ok.Close()

	svc, _, cleanup := newTestServer(t, ok.URL)
	defer cleanup()
	svc.balancer = balancer.New([]config.BackendConfig{
		{Name: "a-fail", URL: failing.URL, Weight: 1},
		{Name: "b-ok", URL: ok.URL, Weight: 1},
	}, "rr")

	proxy := httptest.NewServer(svc)
	defer proxy.Close()

	resp := postCompletion(t, proxy.URL, nil)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status=%d, want 200 after failover", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(body), "ok-model") {
		t.Fatalf("body=%s, want response from the healthy backend", body)
	}

	recs := waitForRecords(t, svc, 1, http.StatusOK)
	if recs[0].BackendURL != ok.URL {
		t.Fatalf("record backend=%q, want %q", recs[0].BackendURL, ok.URL)
	}
}

// TestHandleProxyFailoverOn4xx 验证 4xx 响应（非 200）同样触发故障转移。
func TestHandleProxyFailoverOn4xx(t *testing.T) {
	notFound := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "no such model", http.StatusNotFound)
	}))
	defer notFound.Close()
	ok := okJSONBackend(t, `{"model":"ok-model","content":"ok"}`)
	defer ok.Close()

	svc, _, cleanup := newTestServer(t, ok.URL)
	defer cleanup()
	svc.balancer = balancer.New([]config.BackendConfig{
		{Name: "a-404", URL: notFound.URL, Weight: 1},
		{Name: "b-ok", URL: ok.URL, Weight: 1},
	}, "rr")

	proxy := httptest.NewServer(svc)
	defer proxy.Close()

	resp := postCompletion(t, proxy.URL, nil)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status=%d, want 200 after failover on 404", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(body), "ok-model") {
		t.Fatalf("body=%s, want response from the healthy backend", body)
	}

	recs := waitForRecords(t, svc, 1, http.StatusOK)
	if recs[0].BackendURL != ok.URL {
		t.Fatalf("record backend=%q, want %q", recs[0].BackendURL, ok.URL)
	}
}

// TestHandleProxyFailoverExhaustedMixedStatus 验证所有后端都返回非 200 时，
// 按后端分组返回每个后端的真实状态码与响应内容，顶层状态码沿用最后一个后端的。
func TestHandleProxyFailoverExhaustedMixedStatus(t *testing.T) {
	first404 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "first says no", http.StatusNotFound)
	}))
	defer first404.Close()
	second500 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "second says no", http.StatusInternalServerError)
	}))
	defer second500.Close()

	svc, _, cleanup := newTestServer(t, first404.URL)
	defer cleanup()
	svc.balancer = balancer.New([]config.BackendConfig{
		{Name: "a-404", URL: first404.URL, Weight: 1},
		{Name: "b-500", URL: second500.URL, Weight: 1},
	}, "rr")

	proxy := httptest.NewServer(svc)
	defer proxy.Close()

	resp := postCompletion(t, proxy.URL, nil)
	defer resp.Body.Close()
	// 顶层状态码 = 最后一个后端（b-500）的真实状态码。
	if resp.StatusCode != http.StatusInternalServerError {
		t.Fatalf("status=%d, want 500 (last backend's status)", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	s := string(body)
	if !strings.Contains(s, "\"attempts\"") || !strings.Contains(s, "all backends returned non-200") {
		t.Fatalf("body=%s, want grouped attempts report", s)
	}
	// 每个后端的真实状态码与响应内容都要在报告里。
	for _, want := range []string{
		"\"status_code\":404", "\"status_code\":500",
		first404.URL, second500.URL,
		"first says no", "second says no",
	} {
		if !strings.Contains(s, want) {
			t.Fatalf("body=%s, want to contain %q", s, want)
		}
	}

	recs := waitForRecords(t, svc, 1, http.StatusInternalServerError)
	if recs[0].BackendURL != second500.URL {
		t.Fatalf("record backend=%q, want %q (last attempted)", recs[0].BackendURL, second500.URL)
	}
}

// TestHandleProxyFailoverExhausted 验证所有后端都失败时客户端收到 502。
func TestHandleProxyFailoverExhausted(t *testing.T) {
	deadA, deadB := deadBackendURL(t), deadBackendURL(t)
	svc, _, cleanup := newTestServer(t, deadA)
	defer cleanup()
	svc.balancer = balancer.New([]config.BackendConfig{
		{Name: "a-down", URL: deadA, Weight: 1},
		{Name: "b-down", URL: deadB, Weight: 1},
	}, "rr")

	proxy := httptest.NewServer(svc)
	defer proxy.Close()

	resp := postCompletion(t, proxy.URL, nil)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadGateway {
		t.Fatalf("status=%d, want 502 when all backends fail", resp.StatusCode)
	}
	// 所有后端连接失败：按后端分组返回每个后端的失败原因。
	body, _ := io.ReadAll(resp.Body)
	s := string(body)
	if !strings.Contains(s, "\"attempts\"") || !strings.Contains(s, "all backends failed") {
		t.Fatalf("body=%s, want grouped attempts report", s)
	}
	if !strings.Contains(s, deadA) || !strings.Contains(s, deadB) {
		t.Fatalf("body=%s, want both backends reported", s)
	}

	recs := waitForRecords(t, svc, 1, http.StatusBadGateway)
	if recs[0].ErrorText == "" {
		t.Fatalf("expected 502 record with error text, got %+v", recs)
	}
}

// TestGoClawFailoverStaysInToolCallPool 验证命中 tool_call 规则后故障转移只在
// tool_call 子池内转移，不会落到普通后端。
