package api

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/boltq/boltq/internal/broker"
	"github.com/boltq/boltq/internal/metrics"
)

func newTestServer() *HTTPServer {
	b := broker.New(broker.Config{
		MaxRetry:   3,
		AckTimeout: 30 * time.Second,
		QueueCap:   1024,
	})
	return NewHTTPServer(b, metrics.Global(), "")
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

func TestPublishAndConsume(t *testing.T) {
	s := newTestServer()

	// Publish.
	body := `{"topic":"test_queue","payload":"hello world"}`
	req := httptest.NewRequest("POST", "/publish", bytes.NewBufferString(body))
	w := httptest.NewRecorder()
	s.mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("publish: expected 200, got %d: %s", w.Code, w.Body.String())
	}

	var pubResp publishResponse
	json.NewDecoder(w.Body).Decode(&pubResp)
	if pubResp.ID == "" {
		t.Fatal("publish: expected message ID")
	}

	// Consume.
	req = httptest.NewRequest("GET", "/consume?topic=test_queue", nil)
	w = httptest.NewRecorder()
	s.mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("consume: expected 200, got %d", w.Code)
	}

	var consumeResp consumeResponse
	json.NewDecoder(w.Body).Decode(&consumeResp)
	if consumeResp.ID != pubResp.ID {
		t.Fatalf("expected id %s, got %s", pubResp.ID, consumeResp.ID)
	}
}

func TestAckEndpoint(t *testing.T) {
	s := newTestServer()

	// Publish and consume first.
	body := `{"topic":"test","payload":"data"}`
	req := httptest.NewRequest("POST", "/publish", bytes.NewBufferString(body))
	w := httptest.NewRecorder()
	s.mux.ServeHTTP(w, req)

	var pubResp publishResponse
	json.NewDecoder(w.Body).Decode(&pubResp)

	req = httptest.NewRequest("GET", "/consume?topic=test", nil)
	w = httptest.NewRecorder()
	s.mux.ServeHTTP(w, req)

	// ACK.
	ackBody, _ := json.Marshal(map[string]string{"id": pubResp.ID})
	req = httptest.NewRequest("POST", "/ack", bytes.NewReader(ackBody))
	w = httptest.NewRecorder()
	s.mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("ack: expected 200, got %d: %s", w.Code, w.Body.String())
	}
}

func TestConsumeEmpty(t *testing.T) {
	s := newTestServer()

	req := httptest.NewRequest("GET", "/consume?topic=empty", nil)
	w := httptest.NewRecorder()
	s.mux.ServeHTTP(w, req)

	if w.Code != http.StatusNoContent {
		t.Fatalf("expected 204, got %d", w.Code)
	}
}

func TestAPIKeyAuth(t *testing.T) {
	b := broker.New(broker.Config{MaxRetry: 3, AckTimeout: 30 * time.Second, QueueCap: 1024})
	s := NewHTTPServer(b, metrics.Global(), "secret-key")

	// Without key.
	req := httptest.NewRequest("GET", "/stats", nil)
	w := httptest.NewRecorder()
	s.mux.ServeHTTP(w, req)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d", w.Code)
	}

	// With key.
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
	if w.Header().Get("Content-Type") != "text/plain; version=0.0.4" {
		t.Fatalf("expected prometheus content type, got %s", w.Header().Get("Content-Type"))
	}
}

// --- Benchmarks ---

func BenchmarkHTTPPublish(b *testing.B) {
	s := newTestServer()
	body := []byte(`{"topic":"bench","payload":"benchmark data"}`)

	b.ResetTimer()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		req := httptest.NewRequest("POST", "/publish", bytes.NewReader(body))
		w := httptest.NewRecorder()
		s.mux.ServeHTTP(w, req)
	}
}

func BenchmarkHTTPConsume(b *testing.B) {
	s := newTestServer()

	// Pre-fill.
	body := []byte(`{"topic":"bench","payload":"data"}`)
	for i := 0; i < 1024; i++ {
		req := httptest.NewRequest("POST", "/publish", bytes.NewReader(body))
		w := httptest.NewRecorder()
		s.mux.ServeHTTP(w, req)
	}

	b.ResetTimer()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		req := httptest.NewRequest("GET", "/consume?topic=bench", nil)
		w := httptest.NewRecorder()
		s.mux.ServeHTTP(w, req)

		if w.Code == http.StatusNoContent {
			// Refill.
			for j := 0; j < 512; j++ {
				pr := httptest.NewRequest("POST", "/publish", bytes.NewReader(body))
				pw := httptest.NewRecorder()
				s.mux.ServeHTTP(pw, pr)
			}
		}
	}
}
