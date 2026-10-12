package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestInflightRegistry(t *testing.T) {
	svc, _, cleanup := newTestServer(t, "http://example.invalid")
	defer cleanup()

	if len(svc.inflightList()) != 0 {
		t.Fatalf("expected empty inflight list, got %+v", svc.inflightList())
	}

	info := &inflightInfo{
		ID:         "inflight-1",
		ClientIP:   "10.0.0.7",
		UserAgent:  "Mozilla/5.0 TestAgent/1.0",
		Method:     "POST",
		Path:       "/v1/chat/completions",
		Model:      "test-model",
		BackendURL: "http://example.invalid",
		Streaming:  true,
		StartedAt:  time.Now().UTC(),
	}
	svc.inflightAdd(info)

	list := svc.inflightList()
	if len(list) != 1 {
		t.Fatalf("expected 1 inflight, got %d", len(list))
	}
	if list[0].ClientIP != "10.0.0.7" || list[0].UserAgent != "Mozilla/5.0 TestAgent/1.0" {
		t.Fatalf("inflight info mismatch: %+v", list[0])
	}

	// 移除不存在的 ID 不应改变状态。
	svc.inflightRemove("no-such-id")
	if len(svc.inflightList()) != 1 {
		t.Fatalf("remove of unknown id should be a no-op")
	}

	svc.inflightRemove("inflight-1")
	if len(svc.inflightList()) != 0 {
		t.Fatalf("expected empty inflight list after remove, got %+v", svc.inflightList())
	}
}

// TestInflightProxyLifecycle 验证真实代理请求在途期间被登记（含客户端 IP /
// User-Agent），完成后注销。
func TestInflightProxyLifecycle(t *testing.T) {
	release := make(chan struct{})
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		<-release
		_, _ = io.WriteString(w, `{"model":"slow-model","content":"ok"}`)
	}))
	defer backend.Close()

	svc, _, cleanup := newTestServer(t, backend.URL)
	defer cleanup()

	proxy := httptest.NewServer(svc)
	defer proxy.Close()

	done := make(chan int, 1)
	go func() {
		req, err := http.NewRequest(http.MethodPost, proxy.URL+"/v1/chat/completions",
			strings.NewReader(`{"model":"llm_proxy","prompt":"hi"}`))
		if err != nil {
			t.Errorf("new request: %v", err)
			return
		}
		req.Header.Set("User-Agent", "TestUA/9.9")
		resp, err := proxy.Client().Do(req)
		if err != nil {
			t.Errorf("proxy request: %v", err)
			return
		}
		defer resp.Body.Close()
		_, _ = io.ReadAll(resp.Body)
		done <- resp.StatusCode
	}()

	// 等待请求进入在途登记（后端被 release 卡住，请求必然在途）。
	deadline := time.Now().Add(5 * time.Second)
	var found *inflightInfo
	for time.Now().Before(deadline) {
		for _, it := range svc.inflightList() {
			found = &it
			break
		}
		if found != nil {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if found == nil {
		t.Fatalf("request never appeared in inflight registry")
	}
	if found.ClientIP == "" {
		t.Fatalf("expected client ip to be tracked, got %+v", found)
	}
	if found.UserAgent != "TestUA/9.9" {
		t.Fatalf("user agent mismatch: %+v", found)
	}
	if found.Model != "llm_proxy" || found.BackendURL != backend.URL {
		t.Fatalf("model/backend mismatch: %+v", found)
	}

	close(release)
	if code := <-done; code != http.StatusOK {
		t.Fatalf("proxy status=%d", code)
	}

	deadline = time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if len(svc.inflightList()) == 0 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if list := svc.inflightList(); len(list) != 0 {
		t.Fatalf("inflight list not cleared after completion: %+v", list)
	}
}

// TestInflightEndpoint 验证 /_proxy/inflight 经 gin 路由返回当前在途请求。
func TestInflightEndpoint(t *testing.T) {
	svc, _, cleanup := newTestServer(t, "http://example.invalid")
	defer cleanup()

	svc.inflightAdd(&inflightInfo{
		ID:        "inflight-ep",
		ClientIP:  "192.168.1.9",
		UserAgent: "curl/8.0",
		Method:    "POST",
		Path:      "/v1/chat/completions",
		StartedAt: time.Now().UTC(),
	})

	proxy := httptest.NewServer(svc)
	defer proxy.Close()

	resp, err := proxy.Client().Get(proxy.URL + "/_proxy/inflight")
	if err != nil {
		t.Fatalf("get inflight: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status=%d", resp.StatusCode)
	}
	var payload struct {
		Items []inflightInfo `json:"items"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(payload.Items) != 1 || payload.Items[0].ClientIP != "192.168.1.9" || payload.Items[0].UserAgent != "curl/8.0" {
		t.Fatalf("unexpected inflight items: %+v", payload.Items)
	}
}
