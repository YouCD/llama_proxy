package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"llama_proxy/internal/config"
)

// newSwitchTestServer 装配带两个调度模型（background + coding）的测试 Server。
func newSwitchTestServer(t *testing.T) *Server {
	t.Helper()
	cfg := &config.SchedulingConfig{
		Models: map[string]config.ProcessConfig{
			"bg-model": {
				Mode:         config.ModeBackground,
				Command:      "",
				ReadinessURL: "http://127.0.0.1:1/v1/models",
			},
			"coding-m": {
				Mode:         config.ModeCoding,
				Command:      "",
				ReadinessURL: "http://127.0.0.1:2/v1/models",
			},
		},
		Lease:  config.LeaseConfig{CodingIdleTimeout: time.Hour},
		Switch: config.SwitchConfig{DrainTimeout: time.Millisecond, KillTimeout: time.Millisecond, StartupTimeout: 2 * time.Second},
	}
	return newPoolTestServer(t, cfg, nil, "wrr")
}

func doSwitch(t *testing.T, proxyURL, body string) (int, map[string]any) {
	t.Helper()
	req, _ := http.NewRequest(http.MethodPost, proxyURL+"/_proxy/scheduler/switch", bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("switch request: %v", err)
	}
	defer resp.Body.Close()
	var out map[string]any
	dec := json.NewDecoder(resp.Body)
	if err := dec.Decode(&out); err != nil {
		t.Fatalf("decode switch response: %v", err)
	}
	return resp.StatusCode, out
}

func TestSchedulerSwitchDisabled(t *testing.T) {
	svc, _, cleanup := newTestServer(t, "http://example.invalid")
	defer cleanup()
	proxy := httptest.NewServer(svc)
	defer proxy.Close()

	code, _ := doSwitch(t, proxy.URL, `{"model":"x"}`)
	if code != http.StatusNotFound {
		t.Fatalf("disabled scheduler switch = %d, want 404", code)
	}
}

func TestSchedulerSwitchValidation(t *testing.T) {
	svc := newSwitchTestServer(t)
	proxy := httptest.NewServer(svc)
	defer proxy.Close()

	if code, _ := doSwitch(t, proxy.URL, `{"model":"unknown-model"}`); code != http.StatusBadRequest {
		t.Fatalf("unknown model = %d, want 400", code)
	}
	if code, _ := doSwitch(t, proxy.URL, `{}`); code != http.StatusBadRequest {
		t.Fatalf("empty body = %d, want 400", code)
	}
}

func TestSchedulerSwitchStatusList(t *testing.T) {
	svc := newSwitchTestServer(t)
	proxy := httptest.NewServer(svc)
	defer proxy.Close()

	resp, err := http.Get(proxy.URL + "/_proxy/scheduler")
	if err != nil {
		t.Fatalf("get scheduler: %v", err)
	}
	defer resp.Body.Close()
	var status map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&status); err != nil {
		t.Fatalf("decode scheduler: %v", err)
	}
	models, _ := status["models"].([]any)
	if len(models) != 2 {
		t.Fatalf("models = %v, want 2 entries", status["models"])
	}
}

func TestSchedulerSwitchToCoding(t *testing.T) {
	svc := newSwitchTestServer(t)
	proxy := httptest.NewServer(svc)
	defer proxy.Close()

	code, out := doSwitch(t, proxy.URL, `{"model":"coding-m"}`)
	if code != http.StatusAccepted {
		t.Fatalf("coding switch = %d (%v), want 202", code, out)
	}
	if out["status"] != "switching" {
		t.Fatalf("status = %v, want switching", out["status"])
	}
	// 假 probe 立即就绪：轮询等待调度器加载完成。
	deadline := time.Now().Add(5 * time.Second)
	for {
		snap := svc.scheduler.Snapshot()
		if snap.Model == "coding-m" {
			if !snap.Ready {
				t.Fatalf("coding-m switched but not ready")
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for coding-m, model=%q", snap.Model)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func TestSchedulerSwitchBackgroundSkippedWhenCodingLeaseActive(t *testing.T) {
	svc := newSwitchTestServer(t)
	proxy := httptest.NewServer(svc)
	defer proxy.Close()

	// 续期 coding 租约：当前 background 模型在租约内不得被 background 切换打断。
	svc.scheduler.TouchCoding()
	code, out := doSwitch(t, proxy.URL, `{"model":"bg-model"}`)
	if code != http.StatusOK {
		t.Fatalf("background switch with active lease = %d, want 200", code)
	}
	if out["status"] != "skipped" {
		t.Fatalf("status = %v, want skipped", out["status"])
	}
}

// 校验 EnsureModel 对未配置模型返回错误（经端点外路径的防御性检查）。
func TestSchedulerSwitchUnknownModelError(t *testing.T) {
	svc := newSwitchTestServer(t)
	if _, err := svc.scheduler.EnsureModel(context.Background(), "nope"); err == nil {
		t.Fatal("EnsureModel(unknown) = nil error, want error")
	}
}
