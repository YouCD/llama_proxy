package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"llama_proxy/internal/balancer"
	"llama_proxy/internal/config"
)

func TestSelectBackendWithBalancer(t *testing.T) {
	backends := []config.BackendConfig{
		{Name: "lb-1", URL: "http://lb1.example:8080", Weight: 1},
		{Name: "lb-2", URL: "http://lb2.example:8080", Weight: 1},
	}

	cfg := config.Config{}

	svc := &Server{
		cfg:      cfg,
		balancer: balancer.New(backends, "rr"),
	}

	seen := map[string]bool{}
	for i := 0; i < 20; i++ {
		req := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
		cand, _, _, err := svc.selectBackend(nil, req, context.Background(), "")
		if err != nil {
			t.Fatalf("selectBackend: %v", err)
		}
		seen[cand.url] = true
	}

	if len(seen) != 2 {
		t.Fatalf("expected both backends to be selected, got %v", seen)
	}
}

func TestSelectBackendWithBalancerNoDefault(t *testing.T) {
	// No default_url, only load balancer - must still work.
	backends := []config.BackendConfig{
		{Name: "a", URL: "http://a.example:8080", Weight: 1},
		{Name: "b", URL: "http://b.example:8080", Weight: 1},
	}

	svc := &Server{
		cfg:      config.Config{},
		balancer: balancer.New(backends, "rr"),
	}

	for i := 0; i < 10; i++ {
		req := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
		cand, _, _, err := svc.selectBackend(nil, req, context.Background(), "")
		if err != nil {
			t.Fatalf("selectBackend: %v", err)
		}
		if cand.cfg == nil || cand.url == "" {
			t.Fatalf("expected balancer backend, got backend=%q cfg=%v", cand.url, cand.cfg)
		}
	}
}

func TestSelectBackendNoBackendConfigured(t *testing.T) {
	// No balancer -> error.
	svc := &Server{}
	req := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	if _, _, _, err := svc.selectBackend(nil, req, context.Background(), ""); err == nil {
		t.Fatal("expected error when no backend available")
	}
}

func TestSelectBackendBalancerReturnsConfig(t *testing.T) {
	backends := []config.BackendConfig{
		{Name: "qwen", URL: "http://gpu-server-1:8080", Weight: 50, Model: "qwen", APIKey: "key-1"},
		{Name: "deepseek", URL: "http://gpu-server-2:8080", Weight: 50, Model: "deepseek", APIKey: "key-2"},
	}

	// Use weighted random so each backend can be hit many times and we can
	// verify config is attached to the right URL.
	svc := &Server{
		cfg:      config.Config{},
		balancer: balancer.New(backends, "random"),
	}

	found := map[string]*config.BackendConfig{}
	for i := 0; i < 200; i++ {
		req := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
		cand, _, _, err := svc.selectBackend(nil, req, context.Background(), "")
		if err != nil {
			t.Fatalf("selectBackend: %v", err)
		}
		if cand.cfg == nil {
			t.Fatal("expected non-nil backend config from balancer")
		}
		bc := cand.cfg
		if strings.TrimRight(bc.URL, "/") != cand.url {
			t.Fatalf("backend url mismatch: bc=%q sel=%q", bc.URL, cand.url)
		}
		found[bc.Name] = bc
	}

	if len(found) != 2 {
		t.Fatalf("expected both backends, got %v", found)
	}
	if found["qwen"].Model != "qwen" || found["qwen"].APIKey != "key-1" {
		t.Fatalf("qwen config wrong: %+v", found["qwen"])
	}
	if found["deepseek"].Model != "deepseek" || found["deepseek"].APIKey != "key-2" {
		t.Fatalf("deepseek config wrong: %+v", found["deepseek"])
	}
}

