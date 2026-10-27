package main

import (
	"net/http"
	"testing"
	"time"

	"llama_proxy/internal/model"
	"llama_proxy/internal/store"
)

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

func TestGetStatsTokenBreakdown(t *testing.T) {
	svc, _, cleanup := newTestServer(t, "http://example.invalid")
	defer cleanup()

	now := time.Now().UTC()
	records := []model.RequestRecord{
		{
			ID:                 "qwen-1",
			CreatedAt:          now.Add(-10 * time.Minute),
			Method:             http.MethodPost,
			Path:               "/v1/chat/completions",
			Model:              "qwen",
			StatusCode:         http.StatusOK,
			RequestBytes:       100,
			ResponseBytes:      200,
			PromptTokens:       100,
			CachedPromptTokens: 60,
			CompletionTokens:   40,
			TotalTokens:        140,
			IsStreaming:        false,
		},
		{
			ID:                 "deepseek-1",
			CreatedAt:          now.Add(-20 * time.Minute),
			Method:             http.MethodPost,
			Path:               "/v1/chat/completions",
			Model:              "deepseek",
			StatusCode:         http.StatusOK,
			RequestBytes:       100,
			ResponseBytes:      200,
			PromptTokens:       50,
			CachedPromptTokens: 10,
			CompletionTokens:   10,
			TotalTokens:        60,
			IsStreaming:        false,
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
	if stats["total_prompt_tokens"].(int64) != 150 {
		t.Fatalf("total_prompt_tokens=%v", stats["total_prompt_tokens"])
	}
	if stats["total_cached_tokens"].(int64) != 70 {
		t.Fatalf("total_cached_tokens=%v", stats["total_cached_tokens"])
	}
	if stats["total_input_tokens"].(int64) != 80 {
		t.Fatalf("total_input_tokens=%v", stats["total_input_tokens"])
	}
	if got := stats["cache_hit_pct"].(float64); got < 46 || got > 47 {
		t.Fatalf("cache_hit_pct=%v", got)
	}
	if stats["lifetime_cached_tokens"].(int64) != 70 {
		t.Fatalf("lifetime_cached_tokens=%v", stats["lifetime_cached_tokens"])
	}

	byModel, ok := stats["token_by_model"].([]map[string]any)
	if !ok || len(byModel) != 2 {
		t.Fatalf("token_by_model=%v", stats["token_by_model"])
	}
	if byModel[0]["model"].(string) != "qwen" {
		t.Fatalf("first model=%v (expected qwen, largest by tokens)", byModel[0]["model"])
	}
	if byModel[0]["input_tokens"].(int64) != 40 || byModel[0]["cached_tokens"].(int64) != 60 {
		t.Fatalf("qwen breakdown=%v", byModel[0])
	}
	if got := byModel[0]["cache_hit_pct"].(float64); got < 60 || got > 60.01 {
		t.Fatalf("qwen cache_hit_pct=%v", got)
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

