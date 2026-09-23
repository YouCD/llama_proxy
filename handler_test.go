package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"llama_proxy/internal/balancer"
	"llama_proxy/internal/config"
	"llama_proxy/internal/model"
)

func TestHandleRawReturnsSavedPayload(t *testing.T) {
	svc, _, cleanup := newTestServer(t, "http://example.invalid")
	defer cleanup()

	reqRaw, err := svc.store.SaveRawPayload("req2", "request", []byte(`{"hello":"world"}`))
	if err != nil {
		t.Fatalf("save raw: %v", err)
	}
	if err := svc.store.InsertRequest(model.RequestRecord{
		ID:             "req2",
		CreatedAt:      time.Now().UTC(),
		Method:         http.MethodPost,
		Path:           "/v1/chat/completions",
		RequestRawPath: reqRaw,
	}); err != nil {
		t.Fatalf("insert request: %v", err)
	}

	rr := httptest.NewRecorder()
	svc.handleRaw(rr, "/raw/req2/request")
	if rr.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
	if got := strings.TrimSpace(rr.Body.String()); got != `{"hello":"world"}` {
		t.Fatalf("raw body=%q", got)
	}
}

func TestHandleModels(t *testing.T) {
	backends := []config.BackendConfig{
		{Name: "qwen", URL: "http://gpu-1:8080", Weight: 1, Model: "qwen3.8"},
		{Name: "llm_proxy", URL: "http://proxy:8080", Weight: 1},
	}
	cfg := config.Config{
	}
	svc := &Server{
		cfg:      cfg,
		balancer: balancer.New(backends, "rr"),
		yamlCfg:  &config.YAMLConfig{Backends: config.BackendsConfig{List: backends}},
	}

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	svc.handleModels(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d", rec.Code)
	}
	var payload struct {
		Object string `json:"object"`
		Data   []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if payload.Object != "list" {
		t.Fatalf("object=%q", payload.Object)
	}
	// 返回占位模型 llm_prox + 已配置的具体模型 ID（qwen 后端部署 qwen3.8；
	// llm_proxy 后端未配置 model，不出现）。
	if len(payload.Data) != 2 || payload.Data[0].ID != "llm_prox" || payload.Data[1].ID != "qwen3.8" {
		t.Fatalf("expected llm_prox + qwen3.8, got %+v", payload.Data)
	}
}

// TestMonitorModelsEndpointViaAPI 验证 /_proxy/models 经 gin 路由返回库中 distinct 模型。
func TestMonitorModelsEndpointViaAPI(t *testing.T) {
	svc, _, cleanup := newTestServer(t, "http://example.invalid")
	defer cleanup()

	if err := seedRequest(t, svc, model.RequestRecord{
		ID:         "api-model",
		CreatedAt:  time.Now().UTC(),
		Method:     http.MethodPost,
		Path:       "/v1/chat/completions",
		Model:      "test-model",
		StatusCode: http.StatusOK,
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}

	proxy := httptest.NewServer(svc)
	defer proxy.Close()

	resp, err := proxy.Client().Get(proxy.URL + "/_proxy/models")
	if err != nil {
		t.Fatalf("get models: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status=%d", resp.StatusCode)
	}
	var payload struct {
		Items []string `json:"items"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(payload.Items) != 1 || payload.Items[0] != "test-model" {
		t.Fatalf("expected [test-model], got %v", payload.Items)
	}
}
