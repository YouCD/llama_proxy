package main

import (
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"llama_proxy/internal/model"
)

func TestDeleteRequestByIDRemovesRowAndRawFiles(t *testing.T) {
	svc, _, cleanup := newTestServer(t, "http://example.invalid")
	defer cleanup()

	reqRaw, err := svc.store.SaveRawPayload("req1", "request", []byte(`{"a":1}`))
	if err != nil {
		t.Fatalf("save request raw: %v", err)
	}
	respRaw, err := svc.store.SaveRawPayload("req1", "response", []byte(`{"b":2}`))
	if err != nil {
		t.Fatalf("save response raw: %v", err)
	}
	err = svc.store.InsertRequest(model.RequestRecord{
		ID:              "req1",
		CreatedAt:       time.Now().UTC(),
		Method:          http.MethodPost,
		Path:            "/v1/chat/completions",
		RequestRawPath:  reqRaw,
		ResponseRawPath: "",
	})
	if err != nil {
		t.Fatalf("insert request: %v", err)
	}
	err = svc.store.FinishRequest("req1", model.RequestRecord{
		Model:           "m",
		StatusCode:      http.StatusOK,
		ResponseRawPath: respRaw,
	})
	if err != nil {
		t.Fatalf("finish request: %v", err)
	}

	if err := svc.store.DeleteRequestByID("req1"); err != nil {
		t.Fatalf("delete request: %v", err)
	}

	if _, err := svc.store.GetRequestByID("req1"); err == nil {
		t.Fatal("expected deleted row to be missing")
	}
	if _, err := os.Stat(filepath.Join(svc.cfg.DataDir, reqRaw)); !os.IsNotExist(err) {
		t.Fatalf("expected request raw to be removed, got err=%v", err)
	}
	if _, err := os.Stat(filepath.Join(svc.cfg.DataDir, respRaw)); !os.IsNotExist(err) {
		t.Fatalf("expected response raw to be removed, got err=%v", err)
	}
}

func TestGetRequestsWithTokensFilter(t *testing.T) {
	svc, _, cleanup := newTestServer(t, "http://example.invalid")
	defer cleanup()

	now := time.Now().UTC()
	for _, rec := range []model.RequestRecord{
		{
			ID:               "with-tokens",
			CreatedAt:        now,
			Method:           http.MethodPost,
			Path:             "/completion",
			StatusCode:       http.StatusOK,
			PromptTokens:     10,
			CompletionTokens: 20,
			TotalTokens:      30,
		},
		{
			ID:          "no-tokens",
			CreatedAt:   now.Add(-time.Minute),
			Method:      http.MethodPost,
			Path:        "/completion",
			StatusCode:  http.StatusOK,
			TotalTokens: 0,
		},
	} {
		if err := seedRequest(t, svc, rec); err != nil {
			t.Fatalf("seed request %s: %v", rec.ID, err)
		}
	}

	items, err := svc.store.GetRequests(20, 0, model.RequestFilter{WithTokens: true})
	if err != nil {
		t.Fatalf("get requests: %v", err)
	}
	if len(items) != 1 {
		t.Fatalf("expected 1 item, got %d", len(items))
	}
	if items[0].ID != "with-tokens" {
		t.Fatalf("unexpected item id=%q", items[0].ID)
	}
}

func TestGetRequestsChatCompletionsOnlyFilter(t *testing.T) {
	svc, _, cleanup := newTestServer(t, "http://example.invalid")
	defer cleanup()

	now := time.Now().UTC()
	for _, rec := range []model.RequestRecord{
		{
			ID:          "chat",
			CreatedAt:   now,
			Method:      http.MethodPost,
			Path:        "/v1/chat/completions",
			StatusCode:  http.StatusOK,
			TotalTokens: 42,
		},
		{
			ID:          "other-path",
			CreatedAt:   now.Add(-time.Minute),
			Method:      http.MethodPost,
			Path:        "/v1/completions",
			StatusCode:  http.StatusOK,
			TotalTokens: 42,
		},
		{
			ID:          "other-method",
			CreatedAt:   now.Add(-2 * time.Minute),
			Method:      http.MethodGet,
			Path:        "/v1/chat/completions",
			StatusCode:  http.StatusOK,
			TotalTokens: 42,
		},
	} {
		if err := seedRequest(t, svc, rec); err != nil {
			t.Fatalf("seed request %s: %v", rec.ID, err)
		}
	}

	items, err := svc.store.GetRequests(20, 0, model.RequestFilter{ChatCompletionsOnly: true})
	if err != nil {
		t.Fatalf("get requests: %v", err)
	}
	if len(items) != 1 {
		t.Fatalf("expected 1 item, got %d", len(items))
	}
	if items[0].ID != "chat" {
		t.Fatalf("unexpected item id=%q", items[0].ID)
	}
}

func TestCacheFieldsPersistThroughDB(t *testing.T) {
	svc, _, cleanup := newTestServer(t, "http://example.invalid")
	defer cleanup()

	now := time.Now().UTC()
	if err := seedRequest(t, svc, model.RequestRecord{
		ID:                 "cache-row",
		CreatedAt:          now,
		Method:             http.MethodPost,
		Path:               "/v1/chat/completions",
		StatusCode:         http.StatusOK,
		PromptTokens:       100,
		CachedPromptTokens: 40,
		CacheHitPct:        40,
		CompletionTokens:   20,
		TotalTokens:        120,
	}); err != nil {
		t.Fatalf("seed request: %v", err)
	}

	rec, err := svc.store.GetRequestByID("cache-row")
	if err != nil {
		t.Fatalf("get request: %v", err)
	}
	if rec.CachedPromptTokens != 40 {
		t.Fatalf("cached_prompt_tokens=%v", rec.CachedPromptTokens)
	}
	if rec.CacheHitPct != 40 {
		t.Fatalf("cache_hit_pct=%v", rec.CacheHitPct)
	}
}

func TestRepairStuckRequestsBackfillsCachedTokens(t *testing.T) {
	svc, _, cleanup := newTestServer(t, "http://example.invalid")
	defer cleanup()

	body := []byte(strings.Join([]string{
		`data: {"choices":[{"finish_reason":null,"index":0,"delta":{"content":"hi"}}]}`,
		`data: {"choices":[],"created":1776808981,"id":"chatcmpl-l1GS0rzBrGyTnkYcRsQPVe4glheJV3i6","model":"gemma-4-26B-A4B-it-heretic-ara-v2.i1-IQ4_XS.gguf","object":"chat.completion.chunk","usage":{"completion_tokens":696,"prompt_tokens":27597,"total_tokens":28293,"prompt_tokens_details":{"cached_tokens":26972}},"timings":{"cache_n":26972,"prompt_n":625,"prompt_ms":795.72,"prompt_per_second":1739.05,"predicted_n":696,"predicted_ms":14394.43,"predicted_per_second":48.35}}`,
		`data: [DONE]`,
		"",
	}, "\n"))
	respRaw, err := svc.store.SaveRawPayload("repair-row", "response", body)
	if err != nil {
		t.Fatalf("save raw: %v", err)
	}
	if err := svc.store.InsertRequest(model.RequestRecord{
		ID:             "repair-row",
		CreatedAt:      time.Now().UTC().Add(-5 * time.Minute),
		Method:         http.MethodPost,
		Path:           "/v1/chat/completions",
		StatusCode:     0,
		Model:          "proxy-9091",
		RequestBytes:   1024,
		RequestRawPath: "",
	}); err != nil {
		t.Fatalf("insert request: %v", err)
	}

	if err := svc.store.RepairStuckRequests(); err != nil {
		t.Fatalf("repair stuck requests: %v", err)
	}

	rec, err := svc.store.GetRequestByID("repair-row")
	if err != nil {
		t.Fatalf("get request: %v", err)
	}
	if rec.StatusCode != http.StatusOK {
		t.Fatalf("status=%d", rec.StatusCode)
	}
	if rec.CachedPromptTokens != 26972 {
		t.Fatalf("cached_prompt_tokens=%v", rec.CachedPromptTokens)
	}
	if rec.PromptTokens != 27597 || rec.CompletionTokens != 696 || rec.TotalTokens != 28293 {
		t.Fatalf("unexpected tokens: %+v", rec)
	}
	if rec.ResponseRawPath == "" {
		t.Fatal("expected response raw path")
	}
	if rec.ResponseRawPath != respRaw {
		t.Fatalf("response_raw_path=%q want %q", rec.ResponseRawPath, respRaw)
	}
	if rec.CacheHitPct < 97.7 || rec.CacheHitPct > 97.8 {
		t.Fatalf("cache_hit_pct=%v", rec.CacheHitPct)
	}
}

func TestGetRequestsBackendFilter(t *testing.T) {
	svc, _, cleanup := newTestServer(t, "http://example.invalid")
	defer cleanup()

	now := time.Now().UTC()
	for _, rec := range []model.RequestRecord{
		{ID: "f1", CreatedAt: now, Method: http.MethodPost, Path: "/v1/chat/completions", BackendURL: "http://gpu-1:8080", StatusCode: http.StatusOK},
		{ID: "f2", CreatedAt: now, Method: http.MethodPost, Path: "/v1/chat/completions", BackendURL: "http://gpu-2:8080", StatusCode: http.StatusOK},
	} {
		if err := seedRequest(t, svc, rec); err != nil {
			t.Fatalf("seed: %v", err)
		}
	}

	f := model.RequestFilter{Backend: "http://gpu-1:8080"}
	recs, err := svc.store.GetRequests(100, 0, f)
	if err != nil {
		t.Fatalf("getRequests: %v", err)
	}
	if len(recs) != 1 {
		t.Fatalf("expected 1 request for gpu-1, got %d", len(recs))
	}
	if recs[0].BackendURL != "http://gpu-1:8080" {
		t.Fatalf("backend=%q", recs[0].BackendURL)
	}
}

func TestGetRequestsUserAgentFilter(t *testing.T) {
	svc, _, cleanup := newTestServer(t, "http://example.invalid")
	defer cleanup()

	now := time.Now().UTC()
	for _, rec := range []model.RequestRecord{
		{ID: "ua-1", CreatedAt: now, Method: http.MethodPost, Path: "/v1/chat/completions", UserAgent: "GoClaw/2.1 (linux)", StatusCode: http.StatusOK},
		{ID: "ua-2", CreatedAt: now.Add(time.Second), Method: http.MethodPost, Path: "/v1/chat/completions", UserAgent: "curl/8.5.0", StatusCode: http.StatusOK},
		{ID: "ua-3", CreatedAt: now.Add(2 * time.Second), Method: http.MethodPost, Path: "/v1/chat/completions", UserAgent: "", StatusCode: http.StatusOK},
	} {
		if err := seedRequest(t, svc, rec); err != nil {
			t.Fatalf("seed: %v", err)
		}
	}

	// 完整 UA 精确命中
	recs, err := svc.store.GetRequests(100, 0, model.RequestFilter{UserAgent: "GoClaw/2.1 (linux)"})
	if err != nil {
		t.Fatalf("getRequests: %v", err)
	}
	if len(recs) != 1 || recs[0].ID != "ua-1" {
		t.Fatalf("expected only ua-1, got %+v", recs)
	}

	// 子串匹配：GoClaw 前缀命中，curl 与空 UA 不命中
	recs, err = svc.store.GetRequests(100, 0, model.RequestFilter{UserAgent: "GoClaw"})
	if err != nil {
		t.Fatalf("getRequests: %v", err)
	}
	if len(recs) != 1 || recs[0].ID != "ua-1" {
		t.Fatalf("expected only ua-1 for substring, got %+v", recs)
	}
}
