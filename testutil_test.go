package main

import (
	"net/http"
	"testing"
	"time"

	"llama_proxy/internal/balancer"
	"llama_proxy/internal/config"
	"llama_proxy/internal/db"
	"llama_proxy/internal/events"
	"llama_proxy/internal/model"
	"llama_proxy/internal/store"
)

// newTestServer 装配一个最小可用的 Server：sqlite 存储 + 单后端 wrr 池。
// 返回 (Server, Database, cleanup)。
func newTestServer(t *testing.T, backendURL string) (*Server, db.Database, func()) {
	t.Helper()

	dataDir := t.TempDir()
	sqlCfg := config.DatabaseConfig{
		Type: "sqlite",
		SQLite: config.SQLiteConfig{
			Path: "proxy.db",
		},
	}
	database, err := db.NewDatabase(sqlCfg, dataDir, "debug")
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	if err := db.InitDB(database, "sqlite"); err != nil {
		t.Fatalf("init db: %v", err)
	}
	st := store.New(database, dataDir, 14)
	if err := st.Normalize(); err != nil {
		t.Fatalf("normalize db: %v", err)
	}

	backends := []config.BackendConfig{
		{Name: "test-backend", URL: backendURL, Weight: 1},
	}
	svc := &Server{
		cfg: config.Config{
			ListenAddr:          ":0",
			DataDir:             dataDir,
			RetentionDays:       14,
			MaxRequestBytes:     2 << 20,
			MaxCaptureBytes:     2 << 20,
			RequestTimeout:      15 * time.Second,
			RecordPaths:         []string{"/v1/chat/completions", "/v1/completions", "/v1/embeddings"},
		},
		store:    st,
		balancer: balancer.New(backends, "wrr"),
		client:   &http.Client{Timeout: 15 * time.Second},
		hub:      events.New(),
	}
	st.Active = func() int64 { return svc.active.Load() }

	cleanup := func() {
		_ = db.CloseDatabase(database)
	}
	return svc, database, cleanup
}

// seedRequest 插入并收尾一条请求记录（等价于一次完整的代理请求落库）。
func seedRequest(t *testing.T, svc *Server, rec model.RequestRecord) error {
	t.Helper()
	if err := svc.store.InsertRequest(model.RequestRecord{
		ID:             rec.ID,
		CreatedAt:      rec.CreatedAt,
		Method:         rec.Method,
		Path:           rec.Path,
		Query:          rec.Query,
		ClientIP:       rec.ClientIP,
		BackendURL:     rec.BackendURL,
		Model:          rec.Model,
		IsStreaming:    rec.IsStreaming,
		RequestBytes:   rec.RequestBytes,
		RequestRawPath: rec.RequestRawPath,
		UserAgent:      rec.UserAgent,
	}); err != nil {
		return err
	}
	return svc.store.FinishRequest(rec.ID, rec)
}
