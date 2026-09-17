package handlers

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"bknetwork/internal/events"

	"github.com/gorilla/websocket"
)

type websocketTestHub struct {
	mu           sync.Mutex
	subscribers  map[chan events.Event]struct{}
	unsubscribed chan struct{}
}

func newWebsocketTestHub() *websocketTestHub {
	return &websocketTestHub{
		subscribers:  make(map[chan events.Event]struct{}),
		unsubscribed: make(chan struct{}, 1),
	}
}

func (h *websocketTestHub) Subscribe(buffer int) chan events.Event {
	if buffer <= 0 {
		buffer = 8
	}
	ch := make(chan events.Event, buffer)
	h.mu.Lock()
	h.subscribers[ch] = struct{}{}
	h.mu.Unlock()
	return ch
}

func (h *websocketTestHub) Unsubscribe(ch chan events.Event) {
	h.mu.Lock()
	if _, ok := h.subscribers[ch]; ok {
		delete(h.subscribers, ch)
		close(ch)
		select {
		case h.unsubscribed <- struct{}{}:
		default:
		}
	}
	h.mu.Unlock()
}

func (h *websocketTestHub) publish(event events.Event) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for ch := range h.subscribers {
		select {
		case ch <- event:
		default:
		}
	}
}

func (h *websocketTestHub) subscriberCount() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.subscribers)
}

func newWebsocketTestServer(t *testing.T, hub websocketEventHub, collect websocketSnapshotCollector) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		serveWebSocket(r.Context(), c, hub, collect)
	}))
}

func dialWebsocketTestServer(t *testing.T, server *httptest.Server) *websocket.Conn {
	t.Helper()
	url := "ws" + strings.TrimPrefix(server.URL, "http")
	c, response, err := websocket.DefaultDialer.Dial(url, nil)
	if err != nil {
		if response != nil {
			t.Fatalf("websocket dial: %v (HTTP %s)", err, response.Status)
		}
		t.Fatalf("websocket dial: %v", err)
	}
	return c
}

func readWebsocketTestEvent(t *testing.T, c *websocket.Conn) events.Event {
	t.Helper()
	if err := c.SetReadDeadline(time.Now().Add(2 * time.Second)); err != nil {
		t.Fatalf("set websocket read deadline: %v", err)
	}
	var event events.Event
	if err := c.ReadJSON(&event); err != nil {
		t.Fatalf("read websocket event: %v", err)
	}
	return event
}

func waitWebsocketUnsubscribe(t *testing.T, hub *websocketTestHub) {
	t.Helper()
	select {
	case <-hub.unsubscribed:
	case <-time.After(2 * time.Second):
		t.Fatalf("websocket subscription was not removed; count=%d", hub.subscriberCount())
	}
	if got := hub.subscriberCount(); got != 0 {
		t.Fatalf("websocket subscriber count = %d; want 0", got)
	}
}

func TestServeWebSocketSendsHelloSnapshotAndEventsThenUnsubscribes(t *testing.T) {
	hub := newWebsocketTestHub()
	server := newWebsocketTestServer(t, hub, func(context.Context) (networkSnapshot, error) {
		return networkSnapshot{CollectedAt: "test"}, nil
	})
	defer server.Close()

	c := dialWebsocketTestServer(t, server)
	defer c.Close()

	if event := readWebsocketTestEvent(t, c); event.Type != "hello" || event.Message != "connected to BKNetwork" {
		t.Fatalf("hello event = %#v", event)
	}
	if event := readWebsocketTestEvent(t, c); event.Type != "network.status" || event.Message != "network snapshot" {
		t.Fatalf("snapshot event = %#v", event)
	}

	hub.publish(events.Event{Type: "settings.ok", Message: "changed"})
	if event := readWebsocketTestEvent(t, c); event.Type != "settings.ok" || event.Message != "changed" {
		t.Fatalf("published event = %#v", event)
	}

	// No further event is needed to notice the close. The read pump must wake
	// the handler and release its subscription while the connection is idle.
	if err := c.Close(); err != nil {
		t.Fatalf("close websocket client: %v", err)
	}
	waitWebsocketUnsubscribe(t, hub)
}

func TestServeWebSocketCancelsSnapshotWhenClientDisconnects(t *testing.T) {
	hub := newWebsocketTestHub()
	collectorStarted := make(chan struct{})
	collectorCanceled := make(chan struct{})
	server := newWebsocketTestServer(t, hub, func(ctx context.Context) (networkSnapshot, error) {
		close(collectorStarted)
		<-ctx.Done()
		close(collectorCanceled)
		return networkSnapshot{}, ctx.Err()
	})
	defer server.Close()

	c := dialWebsocketTestServer(t, server)
	defer c.Close()
	if event := readWebsocketTestEvent(t, c); event.Type != "hello" {
		t.Fatalf("first event = %#v; want hello", event)
	}
	select {
	case <-collectorStarted:
	case <-time.After(2 * time.Second):
		t.Fatal("snapshot collector did not start")
	}

	if err := c.Close(); err != nil {
		t.Fatalf("close websocket client: %v", err)
	}
	select {
	case <-collectorCanceled:
	case <-time.After(2 * time.Second):
		t.Fatal("snapshot collector was not canceled after client disconnect")
	}
	waitWebsocketUnsubscribe(t, hub)
}
