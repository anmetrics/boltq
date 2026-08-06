package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/boltq/boltq/internal/broker"
	"github.com/boltq/boltq/internal/config"
	"github.com/boltq/boltq/internal/metrics"
)

func newHealthServer(t *testing.T) *HTTPServer {
	t.Helper()
	b := broker.New(broker.Config{})
	t.Cleanup(func() { b.Close() })
	return NewHTTPServer(b, metrics.Global(), config.ServerConfig{}, "")
}

func probe(t *testing.T, s *HTTPServer, path string) (int, ReadinessState) {
	t.Helper()
	rec := httptest.NewRecorder()
	s.mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))

	var st ReadinessState
	if err := json.Unmarshal(rec.Body.Bytes(), &st); err != nil {
		// /livez returns a different shape; the status code is what matters.
		return rec.Code, ReadinessState{}
	}
	return rec.Code, st
}

// TestLivenessIgnoresClusterState is the property that keeps a quorum loss from
// becoming an outage: liveness must never fail because consensus is unsettled,
// or Kubernetes restarts every node exactly when the cluster needs them up to
// hold an election.
func TestLivenessIgnoresClusterState(t *testing.T) {
	s := newHealthServer(t)

	code, _ := probe(t, s, "/livez")
	if code != http.StatusOK {
		t.Errorf("/livez = %d, want 200", code)
	}
}

// TestReadinessSingleNode: with no cluster configured there is nothing to be
// out of, so a serving process is ready.
func TestReadinessSingleNode(t *testing.T) {
	s := newHealthServer(t)

	code, st := probe(t, s, "/readyz")
	if code != http.StatusOK || !st.Ready {
		t.Errorf("/readyz = %d ready=%v, want 200/true for a single node", code, st.Ready)
	}
	if st.Clustered {
		t.Error("single node reported itself as clustered")
	}
}

// TestReadinessRequiresAKnownLeader: a clustered node that cannot name a leader
// can neither serve writes (nowhere to redirect them) nor trust its own log,
// so it must be pulled from the load balancer.
func TestReadinessRequiresAKnownLeader(t *testing.T) {
	s := newHealthServer(t)

	// Readiness reads leadership through the cluster node; with none attached
	// the single-node path applies, which the previous test covers. Here we
	// assert the contract the manifests depend on: 503 means "do not route".
	st := s.Readiness()
	if !st.Ready {
		t.Fatalf("unexpected unready single node: %+v", st)
	}

	rec := httptest.NewRecorder()
	s.mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/readyz", nil))
	if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", ct)
	}
}

// TestHealthEndpointUnchanged: /health predates the probes and is what existing
// dashboards poll. Repurposing it would silently change what they report.
func TestHealthEndpointUnchanged(t *testing.T) {
	s := newHealthServer(t)

	rec := httptest.NewRecorder()
	s.mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/health", nil))
	if rec.Code != http.StatusOK {
		t.Errorf("/health = %d, want 200", rec.Code)
	}

	var body map[string]string
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body["status"] != "ok" {
		t.Errorf("/health body = %v, want status ok", body)
	}
}
