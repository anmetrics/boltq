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

func TestMessagingEndpointsRemoved(t *testing.T) {
	s := newTestServer()
	endpoints := []struct {
		method string
		path   string
	}{
		{"POST", "/publish"},
		{"POST", "/publish/topic"},
		{"GET", "/consume?topic=test"},
		{"POST", "/ack"},
		{"POST", "/nack"},
		{"GET", "/subscribe?topic=test&id=sub1"},
	}
	for _, ep := range endpoints {
		req := httptest.NewRequest(ep.method, ep.path, nil)
		w := httptest.NewRecorder()
		s.mux.ServeHTTP(w, req)
		if w.Code != http.StatusNotFound {
			t.Errorf("%s %s: expected 404, got %d", ep.method, ep.path, w.Code)
		}
	}
}
