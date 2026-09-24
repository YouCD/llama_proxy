package main

import (
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"llama_proxy/internal/model"
	"llama_proxy/internal/store"
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

func TestGetStatsAggregatesRollingAndLifetime(t *testing.T) {
	svc, _, cleanup := newTestServer(t, "http://example.invalid")
	defer cleanup()

	now := time.Now().UTC()
	records := []model.RequestRecord{
		{
			ID:               "recent-ok",
			CreatedAt:        now.Add(-10 * time.Minute),
			Method:           http.MethodPost,
			Path:             "/completion",
			StatusCode:       http.StatusOK,
			RequestBytes:     100,
			ResponseBytes:    200,
			PromptTokens:     10,
			CompletionTokens: 20,
			TotalTokens:      30,
			PromptMs:         100,
			CompletionMs:     200,
			TotalMs:          400,
			FirstByteMs:      150,
			IsStreaming:      false,
		},
		{
			ID:               "recent-error",
			CreatedAt:        now.Add(-7 * time.Minute),
			Method:           http.MethodPost,
			Path:             "/completion",
			StatusCode:       http.StatusInternalServerError,
			RequestBytes:     25,
			ResponseBytes:    12,
			PromptTokens:     2,
			CompletionTokens: 0,
			TotalTokens:      2,
			ErrorText:        "backend exploded",
			IsStreaming:      false,
		},
		{
			ID:               "recent-live",
			CreatedAt:        now.Add(-5 * time.Minute),
			Method:           http.MethodPost,
			Path:             "/v1/chat/completions",
			StatusCode:       0,
			RequestBytes:     50,
			ResponseBytes:    0,
			PromptTokens:     0,
			CompletionTokens: 0,
			TotalTokens:      0,
			TotalMs:          0,
			FirstByteMs:      0,
			IsStreaming:      true,
		},
		{
			ID:               "old-ok",
			CreatedAt:        now.Add(-26 * time.Hour),
			Method:           http.MethodPost,
			Path:             "/completion",
			StatusCode:       http.StatusOK,
			RequestBytes:     70,
			ResponseBytes:    80,
			PromptTokens:     7,
			CompletionTokens: 8,
			TotalTokens:      15,
			PromptMs:         70,
			CompletionMs:     80,
			TotalMs:          170,
			FirstByteMs:      90,
			IsStreaming:      false,
		},
	}
	for _, rec := range records {
		if err := seedRequest(t, svc, rec); err != nil {
			t.Fatalf("seed request %s: %v", rec.ID, err)
		}
	}

	stats, err := svc.store.GetStats(model.RequestFilter{TimeFrom: now.Add(-24 * time.Hour), TimeTo: time.Now().UTC()})
	if err != nil {
		t.Fatalf("get stats: %v", err)
	}
	if stats["total_requests"].(int64) != 3 {
		t.Fatalf("rolling total_requests=%v", stats["total_requests"])
	}
	if stats["lifetime_total_requests"].(int64) != 4 {
		t.Fatalf("lifetime_total_requests=%v", stats["lifetime_total_requests"])
	}
	if stats["total_tokens"].(int64) != 32 {
		t.Fatalf("rolling total_tokens=%v", stats["total_tokens"])
	}
	if stats["lifetime_total_tokens"].(int64) != 47 {
		t.Fatalf("lifetime_total_tokens=%v", stats["lifetime_total_tokens"])
	}
	if stats["prompt_tokens_per_second"].(float64) <= 0 {
		t.Fatalf("prompt_tokens_per_second=%v", stats["prompt_tokens_per_second"])
	}
	if stats["decode_tokens_per_second"].(float64) <= 0 {
		t.Fatalf("decode_tokens_per_second=%v", stats["decode_tokens_per_second"])
	}
	if stats["errors_count"].(int64) != 1 {
		t.Fatalf("errors_count=%v", stats["errors_count"])
	}
	if stats["streaming_requests"].(int64) != 1 {
		t.Fatalf("streaming_requests=%v", stats["streaming_requests"])
	}
}

func TestGetStatsIgnoresLiveRequestsInErrors(t *testing.T) {
	svc, _, cleanup := newTestServer(t, "http://example.invalid")
	defer cleanup()

	now := time.Now().UTC()
	for _, rec := range []model.RequestRecord{
		{
			ID:          "live",
			CreatedAt:   now.Add(-2 * time.Minute),
			Method:      http.MethodPost,
			Path:        "/v1/chat/completions",
			StatusCode:  0,
			IsStreaming: true,
		},
		{
			ID:          "ok",
			CreatedAt:   now.Add(-1 * time.Minute),
			Method:      http.MethodPost,
			Path:        "/v1/chat/completions",
			StatusCode:  http.StatusOK,
			TotalTokens: 10,
		},
	} {
		if err := seedRequest(t, svc, rec); err != nil {
			t.Fatalf("seed request %s: %v", rec.ID, err)
		}
	}

	stats, err := svc.store.GetStats(model.RequestFilter{TimeFrom: now.Add(-24 * time.Hour), TimeTo: time.Now().UTC()})
	if err != nil {
		t.Fatalf("get stats: %v", err)
	}
	if stats["errors_count"].(int64) != 0 {
		t.Fatalf("errors_count=%v", stats["errors_count"])
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

func TestGetModelsReturnsDistinctSortedValues(t *testing.T) {
	svc, _, cleanup := newTestServer(t, "http://example.invalid")
	defer cleanup()

	now := time.Now().UTC()
	for _, rec := range []model.RequestRecord{
		{
			ID:         "m1",
			CreatedAt:  now,
			Method:     http.MethodPost,
			Path:       "/v1/chat/completions",
			Model:      "qwen-3",
			StatusCode: http.StatusOK,
		},
		{
			ID:         "m2",
			CreatedAt:  now.Add(-time.Minute),
			Method:     http.MethodPost,
			Path:       "/v1/chat/completions",
			Model:      "Gemma-4",
			StatusCode: http.StatusOK,
		},
		{
			ID:         "m3",
			CreatedAt:  now.Add(-2 * time.Minute),
			Method:     http.MethodPost,
			Path:       "/v1/chat/completions",
			Model:      "qwen-3",
			StatusCode: http.StatusOK,
		},
		{
			ID:         "m4",
			CreatedAt:  now.Add(-3 * time.Minute),
			Method:     http.MethodPost,
			Path:       "/v1/chat/completions",
			Model:      "",
			StatusCode: http.StatusOK,
		},
	} {
		if err := seedRequest(t, svc, rec); err != nil {
			t.Fatalf("seed request %s: %v", rec.ID, err)
		}
	}

	items, err := svc.store.GetModels()
	if err != nil {
		t.Fatalf("get models: %v", err)
	}
	if len(items) != 2 {
		t.Fatalf("expected 2 models, got %d (%v)", len(items), items)
	}
	if items[0] != "Gemma-4" || items[1] != "qwen-3" {
		t.Fatalf("unexpected models: %v", items)
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

func TestGetStatsRespectsFilters(t *testing.T) {
	svc, _, cleanup := newTestServer(t, "http://example.invalid")
	defer cleanup()

	now := time.Now().UTC()
	for _, rec := range []model.RequestRecord{
		{
			ID:               "stream-with-tokens",
			CreatedAt:        now,
			Method:           http.MethodPost,
			Path:             "/v1/chat/completions",
			Model:            "m1",
			IsStreaming:      true,
			StatusCode:       http.StatusOK,
			PromptTokens:     20,
			CompletionTokens: 40,
			TotalTokens:      60,
		},
		{
			ID:          "plain-no-tokens",
			CreatedAt:   now,
			Method:      http.MethodPost,
			Path:        "/completion",
			Model:       "m2",
			IsStreaming: false,
			StatusCode:  http.StatusOK,
			TotalTokens: 0,
		},
	} {
		if err := seedRequest(t, svc, rec); err != nil {
			t.Fatalf("seed request %s: %v", rec.ID, err)
		}
	}

	streamTrue := true
	statsFilter := model.RequestFilter{Streaming: &streamTrue, WithTokens: true, Path: "/v1/chat/completions", TimeFrom: now.Add(-24 * time.Hour), TimeTo: time.Now().UTC()}
	stats, err := svc.store.GetStats(statsFilter)
	if err != nil {
		t.Fatalf("get stats: %v", err)
	}
	if stats["total_requests"].(int64) != 1 {
		t.Fatalf("filtered total_requests=%v", stats["total_requests"])
	}
	if stats["matching_total_requests"].(int64) != 1 {
		t.Fatalf("matching_total_requests=%v", stats["matching_total_requests"])
	}
	if stats["total_tokens"].(int64) != 60 {
		t.Fatalf("filtered total_tokens=%v", stats["total_tokens"])
	}
	if stats["matching_total_tokens"].(int64) != 60 {
		t.Fatalf("matching_total_tokens=%v", stats["matching_total_tokens"])
	}
	if stats["streaming_requests"].(int64) != 1 {
		t.Fatalf("filtered streaming_requests=%v", stats["streaming_requests"])
	}
}

func TestCleanupDisabledWhenRetentionNonPositive(t *testing.T) {
	svc, database, cleanup := newTestServer(t, "http://example.invalid")
	defer cleanup()

	// retentionDays 在 store 构造时确定，按生产路径以 retention=0 重建 store。
	svc.cfg.RetentionDays = 0
	svc.store = store.New(database, svc.cfg.DataDir, svc.cfg.RetentionDays)
	if err := seedRequest(t, svc, model.RequestRecord{
		ID:         "old-record",
		CreatedAt:  time.Now().UTC().AddDate(0, 0, -30),
		Method:     http.MethodPost,
		Path:       "/completion",
		StatusCode: http.StatusOK,
	}); err != nil {
		t.Fatalf("seed request: %v", err)
	}

	svc.store.Cleanup()

	if _, err := svc.store.GetRequestByID("old-record"); err != nil {
		t.Fatalf("expected old record to remain, got %v", err)
	}
}

func TestGetDailyStats(t *testing.T) {
	svc, _, cleanup := newTestServer(t, "http://example.invalid")
	defer cleanup()

	now := time.Now().UTC()
	day0 := now.AddDate(0, 0, 0) // 今天
	day1 := now.AddDate(0, 0, -1)
	day2 := now.AddDate(0, 0, -2)

	records := []model.RequestRecord{
		// day2: 2 个请求，均为 200
		{ID: "d2-1", CreatedAt: day2, Method: http.MethodPost, Path: "/v1/chat/completions", StatusCode: http.StatusOK, PromptTokens: 10, CachedPromptTokens: 8, CompletionTokens: 20, TotalTokens: 30},
		{ID: "d2-2", CreatedAt: day2, Method: http.MethodPost, Path: "/v1/chat/completions", StatusCode: http.StatusOK, PromptTokens: 5, CompletionTokens: 5, TotalTokens: 10},
		// day1: 3 个请求：1 个 200、1 个 404、1 个 500
		{ID: "d1-1", CreatedAt: day1, Method: http.MethodPost, Path: "/v1/chat/completions", StatusCode: http.StatusOK, PromptTokens: 100, CompletionTokens: 0, TotalTokens: 100},
		{ID: "d1-2", CreatedAt: day1, Method: http.MethodPost, Path: "/v1/chat/completions", StatusCode: http.StatusNotFound, PromptTokens: 0, CompletionTokens: 0, TotalTokens: 0},
		{ID: "d1-3", CreatedAt: day1, Method: http.MethodPost, Path: "/v1/chat/completions", StatusCode: http.StatusInternalServerError, PromptTokens: 0, CompletionTokens: 0, TotalTokens: 0},
		// day0: 1 个 200
		{ID: "d0-1", CreatedAt: day0, Method: http.MethodPost, Path: "/v1/chat/completions", StatusCode: http.StatusOK, PromptTokens: 1, CompletionTokens: 2, TotalTokens: 3},
	}
	for _, rec := range records {
		if err := seedRequest(t, svc, rec); err != nil {
			t.Fatalf("seed %s: %v", rec.ID, err)
		}
	}

	items, err := svc.store.GetDailyStats(10, model.RequestFilter{})
	if err != nil {
		t.Fatalf("getDailyStats: %v", err)
	}
	if len(items) != 3 {
		t.Fatalf("expected 3 daily rows, got %d: %+v", len(items), items)
	}

	byDate := make(map[string]map[string]any, len(items))
	for _, it := range items {
		byDate[it["date"].(string)] = it
	}

	// 分组按 +8 时区（Asia/Shanghai）取日期，断言使用同一时区
	dayStr := func(tm time.Time) string {
		return tm.In(time.FixedZone("CST", 8*3600)).Format("2006-01-02")
	}

	if it := byDate[dayStr(day2)]; it == nil {
		t.Fatalf("missing day2 row: %+v", byDate)
	} else {
		if it["total_requests"].(int64) != 2 {
			t.Errorf("day2 total_requests=%v", it["total_requests"])
		}
		if it["total_tokens"].(int64) != 40 {
			t.Errorf("day2 total_tokens=%v", it["total_tokens"])
		}
		if it["cached_prompt_tokens"].(int64) != 8 {
			t.Errorf("day2 cached_prompt_tokens=%v", it["cached_prompt_tokens"])
		}
		if it["ok_requests"].(int64) != 2 || it["err4xx"].(int64) != 0 || it["err5xx"].(int64) != 0 {
			t.Errorf("day2 status counts=%v/%v/%v", it["ok_requests"], it["err4xx"], it["err5xx"])
		}
	}

	if it := byDate[dayStr(day1)]; it == nil {
		t.Fatalf("missing day1 row: %+v", byDate)
	} else {
		if it["total_requests"].(int64) != 3 {
			t.Errorf("day1 total_requests=%v", it["total_requests"])
		}
		if it["total_tokens"].(int64) != 100 {
			t.Errorf("day1 total_tokens=%v", it["total_tokens"])
		}
		if it["ok_requests"].(int64) != 1 || it["err4xx"].(int64) != 1 || it["err5xx"].(int64) != 1 {
			t.Errorf("day1 status counts=%v/%v/%v", it["ok_requests"], it["err4xx"], it["err5xx"])
		}
	}

	if it := byDate[dayStr(day0)]; it == nil {
		t.Fatalf("missing day0 row: %+v", byDate)
	} else {
		if it["total_requests"].(int64) != 1 {
			t.Errorf("day0 total_requests=%v", it["total_requests"])
		}
		if it["ok_requests"].(int64) != 1 || it["err4xx"].(int64) != 0 || it["err5xx"].(int64) != 0 {
			t.Errorf("day0 status counts=%v/%v/%v", it["ok_requests"], it["err4xx"], it["err5xx"])
		}
	}
}

func TestGetBackendsDistinct(t *testing.T) {
	svc, _, cleanup := newTestServer(t, "http://example.invalid")
	defer cleanup()

	now := time.Now().UTC()
	for _, rec := range []model.RequestRecord{
		{ID: "b1-a", CreatedAt: now, Method: http.MethodPost, Path: "/v1/chat/completions", BackendURL: "http://gpu-1:8080", StatusCode: http.StatusOK},
		{ID: "b1-b", CreatedAt: now, Method: http.MethodPost, Path: "/v1/chat/completions", BackendURL: "http://gpu-1:8080", StatusCode: http.StatusOK},
		{ID: "b2", CreatedAt: now, Method: http.MethodPost, Path: "/v1/chat/completions", BackendURL: "http://gpu-2:8080", StatusCode: http.StatusOK},
		{ID: "empty", CreatedAt: now, Method: http.MethodPost, Path: "/v1/chat/completions", BackendURL: "", StatusCode: http.StatusOK},
	} {
		if err := seedRequest(t, svc, rec); err != nil {
			t.Fatalf("seed: %v", err)
		}
	}

	backends, err := svc.store.GetBackends()
	if err != nil {
		t.Fatalf("getBackends: %v", err)
	}
	if len(backends) != 2 {
		t.Fatalf("expected 2 distinct backends, got %v", backends)
	}
	for _, b := range backends {
		if b == "" {
			t.Fatal("empty backend_url should be excluded")
		}
	}
}

func TestGetStatsByBackend(t *testing.T) {
	svc, _, cleanup := newTestServer(t, "http://example.invalid")
	defer cleanup()

	now := time.Now().UTC()
	for _, rec := range []model.RequestRecord{
		{ID: "s1", CreatedAt: now, Method: http.MethodPost, Path: "/v1/chat/completions",
			BackendURL: "http://gpu-1:8080", StatusCode: http.StatusOK,
			PromptTokens: 100, CompletionTokens: 50, TotalTokens: 150, FirstByteMs: 10, TotalMs: 100, IsStreaming: true},
		{ID: "s2", CreatedAt: now, Method: http.MethodPost, Path: "/v1/chat/completions",
			BackendURL: "http://gpu-1:8080", StatusCode: http.StatusBadGateway,
			ErrorText: "boom", PromptTokens: 10, CompletionTokens: 5, TotalTokens: 15, FirstByteMs: 20, TotalMs: 200, IsStreaming: false},
		{ID: "s3", CreatedAt: now, Method: http.MethodPost, Path: "/v1/chat/completions",
			BackendURL: "http://gpu-2:8080", StatusCode: http.StatusOK,
			PromptTokens: 7, CompletionTokens: 3, TotalTokens: 10, FirstByteMs: 30, TotalMs: 300, IsStreaming: true},
	} {
		if err := seedRequest(t, svc, rec); err != nil {
			t.Fatalf("seed: %v", err)
		}
	}

	items, err := svc.store.GetStatsByBackend(model.RequestFilter{TimeFrom: now.Add(-24 * time.Hour), TimeTo: time.Now().UTC()})
	if err != nil {
		t.Fatalf("getStatsByBackend: %v", err)
	}
	if len(items) != 2 {
		t.Fatalf("expected 2 backend groups, got %d: %v", len(items), items)
	}

	var gpu1, gpu2 map[string]any
	for _, it := range items {
		switch it["backend_url"] {
		case "http://gpu-1:8080":
			gpu1 = it
		case "http://gpu-2:8080":
			gpu2 = it
		}
	}
	if gpu1 == nil || gpu2 == nil {
		t.Fatalf("missing group: %v", items)
	}

	if gpu1["requests"].(int64) != 2 {
		t.Fatalf("gpu1 requests=%v", gpu1["requests"])
	}
	if gpu1["errors_count"].(int64) != 1 {
		t.Fatalf("gpu1 errors=%v", gpu1["errors_count"])
	}
	if gpu1["total_tokens"].(int64) != 165 {
		t.Fatalf("gpu1 total_tokens=%v", gpu1["total_tokens"])
	}
	if gpu1["streaming_requests"].(int64) != 1 {
		t.Fatalf("gpu1 streaming=%v", gpu1["streaming_requests"])
	}

	if gpu2["requests"].(int64) != 1 {
		t.Fatalf("gpu2 requests=%v", gpu2["requests"])
	}
	if gpu2["errors_count"].(int64) != 0 {
		t.Fatalf("gpu2 errors=%v", gpu2["errors_count"])
	}
	if gpu2["total_tokens"].(int64) != 10 {
		t.Fatalf("gpu2 total_tokens=%v", gpu2["total_tokens"])
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
