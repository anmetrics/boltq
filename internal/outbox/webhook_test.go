package outbox

import (
	"context"
	"encoding/json"
	"github.com/boltq/boltq/internal/presence"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestNewWebhookNotifierRequiresURL(t *testing.T) {
	if _, err := NewWebhookNotifier(WebhookOptions{}); err == nil {
		t.Error("empty webhook URL was accepted")
	}
}

func TestNewWebhookNotifierDefaults(t *testing.T) {
	n, err := NewWebhookNotifier(WebhookOptions{URL: "http://example.com"})
	if err != nil {
		t.Fatalf("new: %v", err)
	}
	if n.client.Timeout != 10*time.Second {
		t.Errorf("default timeout = %v, want 10s", n.client.Timeout)
	}
}

func TestWebhookSendsCorrectPayload(t *testing.T) {
	type received struct {
		Notifications []Notification `json:"notifications"`
		Count         int            `json:"count"`
		SentAt        int64          `json:"sent_at"`
	}

	var got received
	var contentType, auth, method string

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		method = r.Method
		contentType = r.Header.Get("Content-Type")
		auth = r.Header.Get("Authorization")
		body, _ := io.ReadAll(r.Body)
		json.Unmarshal(body, &got)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	n, _ := NewWebhookNotifier(WebhookOptions{URL: srv.URL, AuthHeader: "Bearer svc"})
	batch := []Notification{
		{UserID: "bob", MessageID: "m1", ConversationID: "c1", SenderID: "alice",
			ConvTopic: "chat.direct.c1", ConvPartition: 3, ConvSeq: 42, At: 1700, Attempt: 1},
		{UserID: "carol", MessageID: "m2", ConversationID: "c1", SenderID: "alice", ConvSeq: 43},
	}

	if err := n.Notify(context.Background(), batch); err != nil {
		t.Fatalf("Notify: %v", err)
	}

	if method != http.MethodPost {
		t.Errorf("method = %s", method)
	}
	if contentType != "application/json" {
		t.Errorf("content-type = %q", contentType)
	}
	if auth != "Bearer svc" {
		t.Errorf("auth header = %q", auth)
	}
	if got.Count != 2 || len(got.Notifications) != 2 {
		t.Fatalf("payload count = %d, notifications = %d", got.Count, len(got.Notifications))
	}
	if got.SentAt == 0 {
		t.Error("sent_at was not stamped")
	}

	first := got.Notifications[0]
	if first.UserID != "bob" || first.MessageID != "m1" {
		t.Errorf("first notification = %+v", first)
	}
	// The coordinates a client needs to jump straight to the message must
	// survive the round trip.
	if first.ConvTopic != "chat.direct.c1" || first.ConvPartition != 3 || first.ConvSeq != 42 {
		t.Errorf("conversation coordinates lost: %+v", first)
	}
}

func TestWebhookEmptyBatchIsNoOp(t *testing.T) {
	var called atomic.Bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		called.Store(true)
	}))
	defer srv.Close()

	n, _ := NewWebhookNotifier(WebhookOptions{URL: srv.URL})
	if err := n.Notify(context.Background(), nil); err != nil {
		t.Errorf("empty batch: %v", err)
	}
	if called.Load() {
		t.Error("an empty batch still made an HTTP call")
	}
}

func TestWebhookAcceptsAll2xx(t *testing.T) {
	for _, code := range []int{200, 201, 202, 204, 299} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(code)
		}))
		n, _ := NewWebhookNotifier(WebhookOptions{URL: srv.URL})
		if err := n.Notify(context.Background(), []Notification{{UserID: "bob"}}); err != nil {
			t.Errorf("status %d was treated as a failure: %v", code, err)
		}
		srv.Close()
	}
}

func TestWebhookRejectsNon2xx(t *testing.T) {
	for _, code := range []int{300, 400, 401, 403, 429, 500, 502, 503} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(code)
			w.Write([]byte("rejected"))
		}))
		n, _ := NewWebhookNotifier(WebhookOptions{URL: srv.URL})
		err := n.Notify(context.Background(), []Notification{{UserID: "bob"}})
		if err == nil {
			t.Errorf("status %d was treated as success", code)
			srv.Close()
			continue
		}
		if !strings.Contains(err.Error(), "rejected") {
			t.Errorf("status %d: error omits the response body: %v", code, err)
		}
		srv.Close()
	}
}

func TestWebhookUnreachableIsAnError(t *testing.T) {
	n, _ := NewWebhookNotifier(WebhookOptions{URL: "http://127.0.0.1:1", Timeout: 200 * time.Millisecond})
	if err := n.Notify(context.Background(), []Notification{{UserID: "bob"}}); err == nil {
		t.Error("an unreachable webhook returned no error")
	}
}

func TestWebhookTimeout(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		time.Sleep(500 * time.Millisecond)
	}))
	defer srv.Close()

	n, _ := NewWebhookNotifier(WebhookOptions{URL: srv.URL, Timeout: 50 * time.Millisecond})
	start := time.Now()
	err := n.Notify(context.Background(), []Notification{{UserID: "bob"}})

	if err == nil {
		t.Error("a slow webhook did not time out")
	}
	if elapsed := time.Since(start); elapsed > 300*time.Millisecond {
		t.Errorf("timeout fired after %v, configured for 50ms", elapsed)
	}
}

func TestWebhookContextCancellation(t *testing.T) {
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		<-release
	}))
	defer srv.Close()
	defer close(release)

	n, _ := NewWebhookNotifier(WebhookOptions{URL: srv.URL, Timeout: 10 * time.Second})
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(50 * time.Millisecond)
		cancel()
	}()

	if err := n.Notify(ctx, []Notification{{UserID: "bob"}}); err == nil {
		t.Error("a cancelled context did not abort the webhook call")
	}
}

func TestWebhookConnectionReuse(t *testing.T) {
	var conns int64
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	srv.Config.ConnState = func(_ net.Conn, state http.ConnState) {
		if state == http.StateNew {
			atomic.AddInt64(&conns, 1)
		}
	}
	srv.Start()
	defer srv.Close()

	n, _ := NewWebhookNotifier(WebhookOptions{URL: srv.URL})
	for i := 0; i < 20; i++ {
		n.Notify(context.Background(), []Notification{{UserID: "bob"}})
	}
	if got := atomic.LoadInt64(&conns); got > 3 {
		t.Errorf("20 calls opened %d connections — the body is not being drained for reuse", got)
	}
}

func TestWebhookDrainsLargeErrorBody(t *testing.T) {
	// A webhook returning a huge error body must not be able to make the
	// notifier read it all into memory.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		w.Write([]byte(strings.Repeat("x", 1<<20)))
	}))
	defer srv.Close()

	n, _ := NewWebhookNotifier(WebhookOptions{URL: srv.URL})
	err := n.Notify(context.Background(), []Notification{{UserID: "bob"}})
	if err == nil {
		t.Fatal("500 was treated as success")
	}
	if len(err.Error()) > 8192 {
		t.Errorf("error message is %d bytes — the response body was not capped", len(err.Error()))
	}
}

func TestWebhookIntegratesWithDispatcher(t *testing.T) {
	var batches int64
	var users []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt64(&batches, 1)
		var payload struct {
			Notifications []Notification `json:"notifications"`
		}
		json.NewDecoder(r.Body).Decode(&payload)
		for _, n := range payload.Notifications {
			users = append(users, n.UserID)
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	notifier, err := NewWebhookNotifier(WebhookOptions{URL: srv.URL})
	if err != nil {
		t.Fatalf("notifier: %v", err)
	}

	f := newFixture(t, Config{GraceDelay: 0})
	d, err := New(Options{
		Log: f.log, Cursors: f.cursors, Presence: presence.LocalLookup{Registry: f.presence},
		Notifier: notifier, Config: Config{GraceDelay: 0},
	})
	if err != nil {
		t.Fatalf("dispatcher: %v", err)
	}
	defer d.Close()

	topic := "chat.inbox.bob"
	f.log.GetOrCreateTopic(topic)
	d.Watch(topic)
	time.Sleep(50 * time.Millisecond)

	f.appendInboxPointer(t, "bob", "conv-1", "msg-1")

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if atomic.LoadInt64(&batches) > 0 {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Error("the dispatcher never called the webhook")
}
