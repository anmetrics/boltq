package api

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/boltq/boltq/internal/broker"
	"github.com/boltq/boltq/internal/config"
	"github.com/boltq/boltq/internal/metrics"
)

func newTestServer() *HTTPServer {
	b := broker.New(broker.Config{
		MaxRetry:   3,
		AckTimeout: 30 * time.Second,
		QueueCap:   1024,
	})
	cfg := config.Default().Server
	return NewHTTPServer(b, metrics.Global(), cfg, "")
}

func TestHealthEndpoint(t *testing.T) {
	s := newTestServer()
	req := httptest.NewRequest("GET", "/health", nil)
	w := httptest.NewRecorder()
	s.mux.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
}

func TestStatsEndpoint(t *testing.T) {
	s := newTestServer()
	req := httptest.NewRequest("GET", "/stats", nil)
	w := httptest.NewRecorder()
	s.mux.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
}

func TestOverviewEndpoint(t *testing.T) {
	s := newTestServer()
	req := httptest.NewRequest("GET", "/overview", nil)
	w := httptest.NewRecorder()
	s.mux.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
}

func TestAPIKeyAuth(t *testing.T) {
	b := broker.New(broker.Config{MaxRetry: 3, AckTimeout: 30 * time.Second, QueueCap: 1024})
	cfg := config.Default().Server
	s := NewHTTPServer(b, metrics.Global(), cfg, "secret-key")

	req := httptest.NewRequest("GET", "/stats", nil)
	w := httptest.NewRecorder()
	s.mux.ServeHTTP(w, req)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d", w.Code)
	}

	req = httptest.NewRequest("GET", "/stats", nil)
	req.Header.Set("X-API-Key", "secret-key")
	w = httptest.NewRecorder()
	s.mux.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
}

func TestMetricsEndpoint(t *testing.T) {
	s := newTestServer()
	req := httptest.NewRequest("GET", "/metrics", nil)
	w := httptest.NewRecorder()
	s.mux.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
}

func TestClusterStatusDisabled(t *testing.T) {
	s := newTestServer()
	req := httptest.NewRequest("GET", "/cluster/status", nil)
	w := httptest.NewRecorder()
	s.mux.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
}

func TestCORSHeaders(t *testing.T) {
	s := newTestServer()
	req := httptest.NewRequest("OPTIONS", "/health", nil)
	w := httptest.NewRecorder()
	s.mux.ServeHTTP(w, req)
	if w.Header().Get("Access-Control-Allow-Origin") != "*" {
		t.Fatal("expected CORS header")
	}
	if w.Code != http.StatusNoContent {
		t.Fatalf("expected 204 for OPTIONS, got %d", w.Code)
	}
}

// TestMessagingEndpointsDoNotBlock replaces a test that asserted these
// endpoints had been removed. They have not — they are registered deliberately
// for the web console and load tests — so the old assertion was stale, and
// because /consume blocked forever on an empty topic the test hung rather than
// failing.
//
// The property worth asserting is the one whose absence caused that hang: an
// HTTP handler must always return. A handler that waits for a message to arrive
// pins a goroutine and a connection indefinitely, and enough such requests
// exhaust the server.
func TestMessagingEndpointsDoNotBlock(t *testing.T) {
	s := newTestServer()

	endpoints := []struct {
		method string
		path   string
	}{
		{"GET", "/consume?topic=empty-topic"},
		{"POST", "/publish"},
		{"POST", "/ack"},
	}

	for _, ep := range endpoints {
		done := make(chan int, 1)
		go func(method, path string) {
			req := httptest.NewRequest(method, path, nil)
			w := httptest.NewRecorder()
			s.mux.ServeHTTP(w, req)
			done <- w.Code
		}(ep.method, ep.path)

		select {
		case code := <-done:
			// Any status is acceptable; returning at all is the point.
			if code == 0 {
				t.Errorf("%s %s: no status written", ep.method, ep.path)
			}
		case <-time.After(2 * time.Second):
			t.Fatalf("%s %s did not return within 2s — the handler blocks, "+
				"which leaks a goroutine and a connection per request",
				ep.method, ep.path)
		}
	}
}

// TestConsumeOnEmptyTopicReports404 pins the behaviour the blocking bug hid.
func TestConsumeOnEmptyTopicReports404(t *testing.T) {
	s := newTestServer()

	rec := httptest.NewRecorder()
	s.mux.ServeHTTP(rec, httptest.NewRequest("GET", "/consume?topic=nothing-here", nil))

	if rec.Code != http.StatusNotFound {
		t.Errorf("consume on an empty topic = %d, want 404", rec.Code)
	}
}
