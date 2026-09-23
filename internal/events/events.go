// Package events 提供 SSE 事件广播中心。
package events

import (
	"encoding/json"
	"fmt"
	"sync"
)

type EventHub struct {
	mu      sync.Mutex
	clients map[chan string]struct{}
}

func New() *EventHub {
	return &EventHub{clients: map[chan string]struct{}{}}
}

func (h *EventHub) Subscribe() chan string {
	ch := make(chan string, 64)
	h.mu.Lock()
	h.clients[ch] = struct{}{}
	h.mu.Unlock()
	return ch
}

func (h *EventHub) Unsubscribe(ch chan string) {
	h.mu.Lock()
	if _, ok := h.clients[ch]; ok {
		delete(h.clients, ch)
		close(ch)
	}
	h.mu.Unlock()
}

type EventType string

const (
	EventTypeStats          EventType = "stats"
	EventTypeScheduler      EventType = "scheduler"
	EventTypeActive         EventType = "active"
	EventTypeRequestDeleted EventType = "request_deleted"
	EventTypeModels         EventType = "models"
	EventTypeBackends       EventType = "backends"
)

// Broadcast 按类型广播数据到所有订阅者。
// v 是需要广播的数据（任意类型），eventType 是事件类型。
func (h *EventHub) Broadcast(eventType EventType, v any) {
	b, err := json.Marshal(v)
	if err != nil {
		return
	}
	msg := string(b)

	h.mu.Lock()
	defer h.mu.Unlock()
	for ch := range h.clients {
		select {
		case ch <- msg:
		default:
		}
	}
}

// BroadcastWithType 广播带类型的事件。发送事件类型和数据的 JSON。
// 事件格式: {"event": "type", "data": {...}}
func (h *EventHub) BroadcastWithType(eventType EventType, data any) {
	b, err := json.Marshal(data)
	if err != nil {
		return
	}
	// 构造 SSE 事件格式
	msg := fmt.Sprintf(`event: %s
data: %s

`, eventType, string(b))

	h.mu.Lock()
	defer h.mu.Unlock()
	for ch := range h.clients {
		select {
		case ch <- msg:
		default:
		}
	}
}
