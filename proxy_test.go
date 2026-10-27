package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"llama_proxy/internal/balancer"
	"llama_proxy/internal/config"
	"llama_proxy/internal/db"
	"llama_proxy/internal/events"
	"llama_proxy/internal/model"
	"llama_proxy/internal/store"
)

func TestHandleProxyNonStreamingLlamaCppJSON(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, `{
			"model":"llama.cpp-actual",
			"usage":{"prompt_tokens":11,"completion_tokens":22,"total_tokens":33},
			"timings":{"prompt_ms":50,"predicted_ms":125},
			"content":"ok"
		}`)
	}))
	defer backend.Close()

	svc, _, cleanup := newTestServer(t, backend.URL)
	defer cleanup()
	svc.cfg.RecordPaths = append(svc.cfg.RecordPaths, "/completion")

	proxy := httptest.NewServer(svc)
	defer proxy.Close()

	resp, err := proxy.Client().Post(proxy.URL+"/completion", "application/json", strings.NewReader(`{"model":"llm_proxy","prompt":"hi"}`))
	if err != nil {
		t.Fatalf("proxy post: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status=%d", resp.StatusCode)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	if !strings.Contains(string(body), `"llama.cpp-actual"`) {
		t.Fatalf("unexpected response body: %s", string(body))
	}

	listResp, err := proxy.Client().Get(proxy.URL + "/_proxy/requests?limit=10")
	if err != nil {
		t.Fatalf("get requests: %v", err)
	}
	defer listResp.Body.Close()
	var payload struct {
		Items []model.RequestRecord `json:"items"`
	}
	if err := json.NewDecoder(listResp.Body).Decode(&payload); err != nil {
		t.Fatalf("decode requests: %v", err)
	}
	if len(payload.Items) == 0 {
		t.Fatal("expected at least one request")
	}
	rec := payload.Items[0]
	if rec.Model != "llama.cpp-actual" {
		t.Fatalf("model=%q", rec.Model)
	}
	if rec.TotalTokens != 33 || rec.PromptTokens != 11 || rec.CompletionTokens != 22 {
		t.Fatalf("unexpected tokens: %+v", rec)
	}
	if rec.ResponseRawPath == "" {
		t.Fatal("expected response raw path")
	}
}

func TestHandleProxyStreamingLifecycle(t *testing.T) {
	backendStarted := make(chan struct{}, 1)
	releaseBackend := make(chan struct{})

	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		flusher, _ := w.(http.Flusher)
		_, _ = io.WriteString(w, "data: {\"model\":\"backend-final\",\"choices\":[{\"delta\":{\"content\":\"x\"}}]}\n\n")
		flusher.Flush()
		select {
		case backendStarted <- struct{}{}:
		default:
		}
		<-releaseBackend
		_, _ = io.WriteString(w, "data: {\"usage\":{\"prompt_tokens\":10,\"completion_tokens\":20,\"total_tokens\":30},\"timings\":{\"prompt_ms\":50,\"predicted_ms\":200}}\n\n")
		_, _ = io.WriteString(w, "data: [DONE]\n\n")
		flusher.Flush()
	}))
	defer backend.Close()

	svc, _, cleanup := newTestServer(t, backend.URL)
	defer cleanup()

	proxy := httptest.NewServer(svc)
	defer proxy.Close()

	client := proxy.Client()
	reqBody := `{"model":"llm_proxy","stream":true,"messages":[{"role":"user","content":"hi"}]}`

	reqDone := make(chan error, 1)
	go func() {
		resp, err := client.Post(proxy.URL+"/v1/chat/completions", "application/json", strings.NewReader(reqBody))
		if err != nil {
			reqDone <- err
			return
		}
		defer resp.Body.Close()
		_, err = io.ReadAll(resp.Body)
		reqDone <- err
	}()

	select {
	case <-backendStarted:
	case <-time.After(3 * time.Second):
		t.Fatal("backend request did not start")
	}

	var liveID string
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		resp, err := client.Get(proxy.URL + "/_proxy/requests?limit=10")
		if err != nil {
			t.Fatalf("load live requests: %v", err)
		}
		var payload struct {
			Items []model.RequestRecord `json:"items"`
		}
		if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
			resp.Body.Close()
			t.Fatalf("decode requests: %v", err)
		}
		resp.Body.Close()
		for _, item := range payload.Items {
			if item.Path == "/v1/chat/completions" {
				if item.StatusCode != 0 {
					t.Fatalf("expected in-flight status 0, got %d", item.StatusCode)
				}
				if item.ResponseRawPath != "" {
					t.Fatalf("expected empty response path while inflight, got %q", item.ResponseRawPath)
				}
				liveID = item.ID
				break
			}
		}
		if liveID != "" {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if liveID == "" {
		t.Fatal("did not observe live request in monitor list")
	}

	close(releaseBackend)

	select {
	case err := <-reqDone:
		if err != nil {
			t.Fatalf("proxy request failed: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("proxy request did not finish")
	}

	resp, err := client.Get(proxy.URL + "/_proxy/request/" + liveID)
	if err != nil {
		t.Fatalf("load final request: %v", err)
	}
	defer resp.Body.Close()
	var rec model.RequestRecord
	if err := json.NewDecoder(resp.Body).Decode(&rec); err != nil {
		t.Fatalf("decode final request: %v", err)
	}
	if rec.StatusCode != http.StatusOK {
		t.Fatalf("status=%d", rec.StatusCode)
	}
	if rec.Model != "backend-final" {
		t.Fatalf("model=%q", rec.Model)
	}
	if rec.TotalTokens != 30 || rec.PromptTokens != 10 || rec.CompletionTokens != 20 {
		t.Fatalf("unexpected tokens: %+v", rec)
	}
	if rec.ResponseRawPath == "" {
		t.Fatal("expected response raw path after completion")
	}
}

func TestProxyRecordPaths(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, `{"model":"llm_proxy","usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`)
	}))
	defer backend.Close()

	svc, _, cleanup := newTestServer(t, backend.URL)
	defer cleanup()
	svc.cfg.RecordPaths = []string{"/v1/chat/completions", "/v1/completions"}

	proxy := httptest.NewServer(svc)
	defer proxy.Close()

	postJSON := func(path string) *http.Response {
		resp, err := proxy.Client().Post(proxy.URL+path, "application/json",
			strings.NewReader(`{"model":"llm_proxy","messages":[{"role":"user","content":"hi"}]}`))
		if err != nil {
			t.Fatalf("post %s: %v", path, err)
		}
		return resp
	}

	resp := postJSON("/v1/embeddings")
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("embeddings status=%d want 404 (not in whitelist)", resp.StatusCode)
	}

	recs, err := svc.store.GetRequests(100, 0, model.RequestFilter{})
	if err != nil {
		t.Fatalf("getRequests: %v", err)
	}
	if len(recs) != 0 {
		t.Fatalf("whitelisted out path recorded: got %d", len(recs))
	}

	resp2 := postJSON("/v1/chat/completions")
	resp2.Body.Close()
	if resp2.StatusCode != http.StatusOK {
		t.Fatalf("chat status=%d", resp2.StatusCode)
	}

	recs2, err := svc.store.GetRequests(100, 0, model.RequestFilter{})
	if err != nil {
		t.Fatalf("getRequests: %v", err)
	}
	if len(recs2) != 1 || recs2[0].Path != "/v1/chat/completions" {
		t.Fatalf("expected whitelisted request recorded, got %+v", recs2)
	}
}

func TestShouldRecordProxy(t *testing.T) {
	svc, _, cleanup := newTestServer(t, "http://example.invalid")
	defer cleanup()

	// 默认白名单：仅 OpenAI 兼容路径被转发记录，其余一律 404
	for _, p := range []string{"/v1/chat/completions", "/v1/completions", "/v1/embeddings"} {
		if !svc.shouldRecordProxy(p) {
			t.Fatalf("whitelisted path %s should record", p)
		}
	}
	for _, p := range []string{"/favicon.ico", "/metrics", "/.well-known/openid-configuration", "/", "/v1/foo"} {
		if svc.shouldRecordProxy(p) {
			t.Fatalf("non-whitelisted path %s should not record", p)
		}
	}

	// 自定义白名单覆盖默认
	svc.cfg.RecordPaths = []string{"/v1/chat/completions"}
	if !svc.shouldRecordProxy("/v1/chat/completions") {
		t.Fatal("whitelisted path should record")
	}
	if svc.shouldRecordProxy("/v1/embeddings") {
		t.Fatal("non-whitelisted path should not record")
	}
}

func TestHandleProxyWithModelRewriteAndAPIKey(t *testing.T) {
	var gotModel string
	var gotAuth string
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		body, _ := io.ReadAll(r.Body)
		var m map[string]any
		_ = json.Unmarshal(body, &m)
		if v, ok := m["model"].(string); ok {
			gotModel = v
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, `{"model":"qwen","usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`)
	}))
	defer backend.Close()

	dataDir := t.TempDir()
	sqlCfg := config.DatabaseConfig{Type: "sqlite", SQLite: config.SQLiteConfig{Path: "proxy.db"}}
	database, err := db.NewDatabase(sqlCfg, dataDir, "debug")
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	defer db.CloseDatabase(database)
	if err := db.InitDB(database, "sqlite"); err != nil {
		t.Fatalf("init db: %v", err)
	}
	st := store.New(database, dataDir, 14)

	backends := []config.BackendConfig{
		{Name: "qwen", URL: backend.URL, Weight: 1, Model: "qwen", APIKey: "secret-key"},
	}

	svc := &Server{
		cfg: config.Config{
			ListenAddr:      ":0",
			DataDir:         dataDir,
			RetentionDays:   14,
			MaxRequestBytes: 2 << 20,
			MaxCaptureBytes: 2 << 20,
			RequestTimeout:  15 * time.Second,
			RecordPaths:     []string{"/v1/chat/completions", "/v1/completions", "/v1/embeddings"},
		},
		store:    st,
		balancer: balancer.New(backends, "wrr"),
		client:   &http.Client{Timeout: 15 * time.Second},
		hub:      events.New(),
	}

	proxy := httptest.NewServer(svc)
	defer proxy.Close()

	resp, err := proxy.Client().Post(proxy.URL+"/v1/chat/completions", "application/json",
		strings.NewReader(`{"model":"llm_proxy","messages":[]}`))
	if err != nil {
		t.Fatalf("proxy post: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("status=%d body=%s", resp.StatusCode, string(body))
	}

	if gotModel != "qwen" {
		t.Fatalf("backend received model=%q, want qwen (rewritten)", gotModel)
	}
	if gotAuth != "Bearer secret-key" {
		t.Fatalf("backend received Authorization=%q, want Bearer secret-key", gotAuth)
	}
}

func TestHandleProxyBackendKeyDoesNotOverrideClientKeyWhenEmpty(t *testing.T) {
	var gotAuth string
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, `{"model":"llm_proxy","usage":{}}`)
	}))
	defer backend.Close()

	dataDir := t.TempDir()
	sqlCfg := config.DatabaseConfig{Type: "sqlite", SQLite: config.SQLiteConfig{Path: "proxy.db"}}
	database, err := db.NewDatabase(sqlCfg, dataDir, "debug")
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	defer db.CloseDatabase(database)
	if err := db.InitDB(database, "sqlite"); err != nil {
		t.Fatalf("init db: %v", err)
	}
	st := store.New(database, dataDir, 14)

	// No api_key configured on the backend -> client's Authorization passes through.
	backends := []config.BackendConfig{
		{Name: "plain", URL: backend.URL, Weight: 1, Model: "m"},
	}

	svc := &Server{
		cfg: config.Config{
			ListenAddr:      ":0",
			DataDir:         dataDir,
			RetentionDays:   14,
			MaxRequestBytes: 2 << 20,
			MaxCaptureBytes: 2 << 20,
			RequestTimeout:  15 * time.Second,
			RecordPaths:     []string{"/v1/chat/completions", "/v1/completions", "/v1/embeddings"},
		},
		store:    st,
		balancer: balancer.New(backends, "wrr"),
		client:   &http.Client{Timeout: 15 * time.Second},
		hub:      events.New(),
	}

	proxy := httptest.NewServer(svc)
	defer proxy.Close()

	req, _ := http.NewRequest(http.MethodPost, proxy.URL+"/v1/chat/completions",
		strings.NewReader(`{"model":"llm_proxy","messages":[]}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer client-key")
	resp, err := proxy.Client().Do(req)
	if err != nil {
		t.Fatalf("proxy do: %v", err)
	}
	defer resp.Body.Close()
	if gotAuth != "Bearer client-key" {
		t.Fatalf("backend received Authorization=%q, want client-key to pass through", gotAuth)
	}
}

func TestHandleProxyStripsVersionPrefixWhenBackendHasV1(t *testing.T) {
	var gotPath string
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, `{"model":"llm_proxy","usage":{}}`)
	}))
	defer backend.Close()

	dataDir := t.TempDir()
	sqlCfg := config.DatabaseConfig{Type: "sqlite", SQLite: config.SQLiteConfig{Path: "proxy.db"}}
	database, err := db.NewDatabase(sqlCfg, dataDir, "debug")
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	defer db.CloseDatabase(database)
	if err := db.InitDB(database, "sqlite"); err != nil {
		t.Fatalf("init db: %v", err)
	}
	st := store.New(database, dataDir, 14)

	backends := []config.BackendConfig{
		{Name: "qwen", URL: backend.URL + "/v1", Weight: 1, Model: "qwen"},
	}

	svc := &Server{
		cfg: config.Config{
			ListenAddr:      ":0",
			DataDir:         dataDir,
			RetentionDays:   14,
			MaxRequestBytes: 2 << 20,
			MaxCaptureBytes: 2 << 20,
			RequestTimeout:  15 * time.Second,
			RecordPaths:     []string{"/v1/chat/completions", "/v1/completions", "/v1/embeddings"},
		},
		store:    st,
		balancer: balancer.New(backends, "wrr"),
		client:   &http.Client{Timeout: 15 * time.Second},
		hub:      events.New(),
	}

	proxy := httptest.NewServer(svc)
	defer proxy.Close()

	resp, err := proxy.Client().Post(proxy.URL+"/v1/chat/completions", "application/json",
		strings.NewReader(`{"model":"llm_proxy","messages":[]}`))
	if err != nil {
		t.Fatalf("proxy post: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("status=%d body=%s", resp.StatusCode, string(body))
	}

	if gotPath != "/v1/chat/completions" {
		t.Fatalf("backend received path=%q, want /v1/chat/completions (no duplicate /v1)", gotPath)
	}
}

func TestBuildProxyPath(t *testing.T) {
	cases := []struct {
		backend, in, want string
	}{
		{"http://host/v1", "/v1/chat/completions", "/chat/completions"},
		{"http://host/v1", "/v1/completions", "/completions"},
		{"http://host/v1", "/v1/embeddings", "/embeddings"},
		{"http://host/v1", "/v1", "/"},
		{"http://host", "/v1/chat/completions", "/v1/chat/completions"},
		{"http://host", "/chat/completions", "/chat/completions"},
		{"http://host/v1", "/v2/models", "/v2/models"},
		{"http://host", "/", "/"},
	}
	for _, c := range cases {
		if got := buildProxyPath(c.backend, c.in); got != c.want {
			t.Errorf("buildProxyPath(%q, %q)=%q, want %q", c.backend, c.in, got, c.want)
		}
	}
}

// doGoClawProxy 经代理发起一次 chat 请求；ua 指定 User-Agent。
