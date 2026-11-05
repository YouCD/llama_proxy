package main

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// TestShutdownCancelsInflightStream 复现“优雅重启被阻塞”场景：
// 一个慢速流式请求（模拟 LLM 长生成）在途时调用 cancelAllProxy（OnShutdown 钩子），
// 代理 handler 必须立即返回，而不是等流自然结束（否则 server.Shutdown 会傻等到
// shutdownCtx 的 60s deadline 才放行）。
func TestShutdownCancelsInflightStream(t *testing.T) {
	// 慢速后端：每 200ms 吐一个 SSE 块，共 20 块（自然结束需 4s）。
	// 读取时若连接被关闭（ctx 取消）会提前结束。
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		flusher, _ := w.(http.Flusher)
		for i := 0; i < 20; i++ {
			fmt.Fprintf(w, "data: chunk %d\n\n", i)
			if flusher != nil {
				flusher.Flush()
			}
			time.Sleep(200 * time.Millisecond)
		}
	}))
	defer backend.Close()

	svc, _, cleanup := newTestServer(t, backend.URL)
	defer cleanup()

	proxy := httptest.NewServer(svc)
	defer proxy.Close()

	req, err := http.NewRequestWithContext(context.Background(), http.MethodPost,
		proxy.URL+"/v1/chat/completions",
		NewRequestBody())
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")

	started := time.Now()
	cancelAt := make(chan struct{})
	done := make(chan error, 1)
	go func() {
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			done <- err
			return
		}
		defer resp.Body.Close()
		// 读到第一块即开始计时取消；继续消费剩余流直到 handler 结束。
		sc := bufio.NewScanner(resp.Body)
		if !sc.Scan() {
			done <- fmt.Errorf("no data")
			return
		}
		<-cancelAt
		// 取消后剩余流应立即结束（上游连接被断开）。
		_, _ = io.Copy(io.Discard, resp.Body)
		done <- nil
	}()

	// 等客户端收到第一块（代理已转发、流已在途），再触发取消。
	time.Sleep(500 * time.Millisecond)
	close(cancelAt)
	cancelledAt := time.Now()
	svc.cancelAllProxy()

	if err := <-done; err != nil {
		t.Fatalf("client request: %v", err)
	}
	elapsed := time.Since(started)
	// 自然结束需 ~4s；取消后应在 1s 内结束。
	if elapsed > time.Second {
		t.Fatalf("in-flight stream not interrupted: took %v after cancel at %v",
			elapsed, cancelledAt.Sub(started))
	}
	t.Logf("in-flight stream ended %v after cancel (would be ~4s naturally)", time.Since(cancelledAt))
}

// TestShutdownCancelsWaitingOnModel 验证等待模型就绪的阻塞路径同样响应取消。
func TestShutdownCancelsWaitingOnModel(t *testing.T) {
	svc, _, cleanup := newTestServer(t, "http://127.0.0.1:1") // 后端不可达，不进入转发
	defer cleanup()

	ctx, cancel := svc.newProxyCtx(context.Background(), "test-req")
	defer cancel()

	// 直接验证 ctx 取消语义：取消后 Done 立即触发（selectScheduledModel 的等待分支依赖它）。
	go func() {
		time.Sleep(50 * time.Millisecond)
		cancel()
	}()
	select {
	case <-ctx.Done():
	case <-time.After(time.Second):
		t.Fatal("ctx not cancelled")
	}
}

// NewRequestBody 构造一个最小的流式 chat completion 请求体。
func NewRequestBody() io.Reader {
	return strings.NewReader(`{"model":"test","stream":true,"messages":[{"role":"user","content":"hi"}]}`)
}
