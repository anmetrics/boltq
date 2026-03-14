package api

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/boltq/boltq/internal/broker"
	"github.com/boltq/boltq/internal/cluster"
	"github.com/boltq/boltq/internal/config"
	"github.com/boltq/boltq/internal/metrics"
	"github.com/boltq/boltq/pkg/protocol"
	"runtime"
)

// HTTPServer provides the admin REST API (stats, metrics, health, cluster management).
// All messaging operations (publish, consume, ack, nack) go through the TCP protocol.
type HTTPServer struct {
	broker      broker.BrokerIface
	metrics     *metrics.Metrics
	apiKey      string
	tlsConfig   config.TLSConfig
	clusterNode *cluster.RaftNode // nil if clustering is disabled
	mux         *http.ServeMux
	server      *http.Server
}

// NewHTTPServer creates a new HTTP admin server.
func NewHTTPServer(b broker.BrokerIface, m *metrics.Metrics, cfg config.ServerConfig, apiKey string) *HTTPServer {
	s := &HTTPServer{
		broker:    b,
		metrics:   m,
		apiKey:    apiKey,
		tlsConfig: cfg.TLS,
		mux:       http.NewServeMux(),
	}
	s.registerRoutes()
	return s
}

func (s *HTTPServer) registerRoutes() {
	// Admin endpoints.
	s.mux.HandleFunc("/overview", s.cors(s.auth(s.handleOverview)))
	s.mux.HandleFunc("/stats", s.cors(s.auth(s.handleStats)))
	s.mux.HandleFunc("/metrics", s.cors(s.handleMetrics))
	s.mux.HandleFunc("/health", s.cors(s.handleHealth))

	// Queue management.
	s.mux.HandleFunc("/queues/purge", s.cors(s.auth(s.handlePurgeQueue)))
	s.mux.HandleFunc("/dead-letters/purge", s.cors(s.auth(s.handlePurgeDeadLetters)))

	// Messaging endpoints (for testing/k6/web).
	s.mux.HandleFunc("/publish", s.cors(s.auth(s.handlePublish)))
	s.mux.HandleFunc("/consume", s.cors(s.auth(s.handleConsume)))
	s.mux.HandleFunc("/ack", s.cors(s.auth(s.handleAck)))

	// Cluster management routes.
	s.mux.HandleFunc("/cluster/join", s.cors(s.auth(s.handleClusterJoin)))
	s.mux.HandleFunc("/cluster/leave", s.cors(s.auth(s.handleClusterLeave)))
	s.mux.HandleFunc("/cluster/status", s.cors(s.auth(s.handleClusterStatus)))
}

// Start starts the HTTP server on the given address.
func (s *HTTPServer) Start(addr string) error {
	s.server = &http.Server{
		Addr:         addr,
		Handler:      s.mux,
		ReadTimeout:  10 * time.Second,
		WriteTimeout: 30 * time.Second,
		IdleTimeout:  60 * time.Second,
	}

	if s.tlsConfig.Enabled {
		log.Printf("[http] admin listening on %s (TLS)", addr)
		return s.server.ListenAndServeTLS(s.tlsConfig.CertFile, s.tlsConfig.KeyFile)
	}

	log.Printf("[http] admin listening on %s", addr)
	return s.server.ListenAndServe()
}

// Shutdown gracefully stops the HTTP server.
func (s *HTTPServer) Shutdown() error {
	if s.server != nil {
		return s.server.Close()
	}
	return nil
}

// cors middleware adds CORS headers for web admin dashboard.
func (s *HTTPServer) cors(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Access-Control-Allow-Methods", "GET, POST, DELETE, OPTIONS")
		w.Header().Set("Access-Control-Allow-Headers", "Content-Type, X-API-Key")
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		next(w, r)
	}
}

// auth middleware checks API key if configured.
func (s *HTTPServer) auth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if s.apiKey != "" {
			key := r.Header.Get("X-API-Key")
			if key == "" {
				key = r.URL.Query().Get("api_key")
			}
			if key != s.apiKey {
				writeError(w, http.StatusUnauthorized, "unauthorized")
				return
			}
		}
		next(w, r)
	}
}

// --- Overview (combined dashboard data) ---

func (s *HTTPServer) handleOverview(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}

	stats := s.broker.Stats()
	snap := s.metrics.Snapshot()

	var mem runtime.MemStats
	runtime.ReadMemStats(&mem)

	overview := map[string]interface{}{
		"health": "ok",
		"stats":  stats,
		"metrics": map[string]int64{
			"messages_published": snap.MessagesPublished,
			"messages_consumed":  snap.MessagesConsumed,
			"messages_acked":     snap.MessagesAcked,
			"messages_nacked":    snap.MessagesNacked,
			"retry_count":        snap.RetryCount,
			"dead_letter_count":  snap.DeadLetterCount,
			"raft_apply_count":   snap.RaftApplyCount,
			"snapshot_count":     snap.SnapshotCount,
			"leader_changes":     snap.LeaderChanges,
		},
		"storage": map[string]interface{}{
			"mode":                 s.broker.StorageMode(),
			"size":                 s.broker.StorageSize(),
			"compaction_threshold": s.broker.CompactionThreshold(),
		},
		"system": map[string]interface{}{
			"goroutines": runtime.NumGoroutine(),
			"memory":     mem.Alloc, // bytes allocated and not yet freed
		},
		"uptime_ms": time.Since(s.startTime()).Milliseconds(),
	}

	if s.clusterNode != nil {
		overview["cluster"] = map[string]interface{}{
			"enabled": true,
			"cluster": s.clusterNode.Status(),
		}
	} else {
		overview["cluster"] = map[string]interface{}{
			"enabled": false,
		}
	}

	writeJSON(w, http.StatusOK, overview)
}

var serverStartTime = time.Now()

func (s *HTTPServer) startTime() time.Time {
	return serverStartTime
}

// --- Stats ---

func (s *HTTPServer) handleStats(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	writeJSON(w, http.StatusOK, s.broker.Stats())
}

// --- Metrics ---

func (s *HTTPServer) handleMetrics(w http.ResponseWriter, r *http.Request) {
	accept := r.Header.Get("Accept")
	if strings.Contains(accept, "application/json") {
		data, _ := s.metrics.JSON()
		w.Header().Set("Content-Type", "application/json")
		w.Write(data)
		return
	}
	// Default: Prometheus text format.
	w.Header().Set("Content-Type", "text/plain; version=0.0.4")
	fmt.Fprint(w, s.metrics.Prometheus())
}

// --- Health ---

func (s *HTTPServer) handleHealth(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

// --- Purge Queue ---

func (s *HTTPServer) handlePurgeQueue(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	var req struct {
		Queue string `json:"queue"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid json")
		return
	}
	defer r.Body.Close()
	if req.Queue == "" {
		writeError(w, http.StatusBadRequest, "queue is required")
		return
	}
	count, err := s.broker.PurgeQueue(req.Queue)
	if err != nil {
		if nle, ok := cluster.IsNotLeaderError(err); ok {
			writeJSON(w, http.StatusTemporaryRedirect, map[string]string{
				"error": "not leader", "leader": nle.Leader, "leader_id": nle.LeaderID,
			})
			return
		}
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"status": "purged", "queue": req.Queue, "purged_count": count,
	})
}

// --- Purge Dead Letters ---

func (s *HTTPServer) handlePurgeDeadLetters(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	var req struct {
		Queue string `json:"queue"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid json")
		return
	}
	defer r.Body.Close()
	if req.Queue == "" {
		writeError(w, http.StatusBadRequest, "queue is required")
		return
	}
	count, err := s.broker.PurgeDeadLetters(req.Queue)
	if err != nil {
		if nle, ok := cluster.IsNotLeaderError(err); ok {
			writeJSON(w, http.StatusTemporaryRedirect, map[string]string{
				"error": "not leader", "leader": nle.Leader, "leader_id": nle.LeaderID,
			})
			return
		}
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"status": "purged", "queue": req.Queue, "purged_count": count,
	})
}

// --- Messaging: Publish ---

func (s *HTTPServer) handlePublish(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	var req struct {
		Topic   string          `json:"topic"`
		Payload json.RawMessage `json:"payload"`
		Delay   int64           `json:"delay"`
		TTL     int64           `json:"ttl"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid json")
		return
	}
	if req.Topic == "" {
		writeError(w, http.StatusBadRequest, "topic is required")
		return
	}

	msg := protocol.NewMessage(req.Topic, req.Payload, nil)
	msg.Delay = req.Delay
	msg.TTL = req.TTL

	if err := s.broker.Publish(req.Topic, msg); err != nil {
		if nle, ok := cluster.IsNotLeaderError(err); ok {
			writeJSON(w, http.StatusTemporaryRedirect, map[string]string{
				"error": "not leader", "leader": nle.Leader, "leader_id": nle.LeaderID,
			})
			return
		}
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}

	writeJSON(w, http.StatusOK, map[string]string{"status": "published", "id": msg.ID})
}

// --- Messaging: Consume ---

func (s *HTTPServer) handleConsume(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	topic := r.URL.Query().Get("topic")
	if topic == "" {
		writeError(w, http.StatusBadRequest, "topic is required")
		return
	}

	msg := s.broker.Consume(topic)
	if msg == nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "no messages available"})
		return
	}

	writeJSON(w, http.StatusOK, msg)
}

// --- Messaging: Ack ---

func (s *HTTPServer) handleAck(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	var req struct {
		ID string `json:"id"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid json")
		return
	}
	if req.ID == "" {
		writeError(w, http.StatusBadRequest, "id is required")
		return
	}

	if err := s.broker.Ack(req.ID); err != nil {
		if nle, ok := cluster.IsNotLeaderError(err); ok {
			writeJSON(w, http.StatusTemporaryRedirect, map[string]string{
				"error": "not leader", "leader": nle.Leader, "leader_id": nle.LeaderID,
			})
			return
		}
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}

	writeJSON(w, http.StatusOK, map[string]string{"status": "acked"})
}

// --- Cluster Join ---

type clusterJoinRequest struct {
	NodeID   string `json:"node_id"`
	Addr     string `json:"addr"`
	NonVoter bool   `json:"non_voter"` // Join as read replica (non-voter)
}

func (s *HTTPServer) handleClusterJoin(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	if s.clusterNode == nil {
		writeError(w, http.StatusBadRequest, "clustering is not enabled")
		return
	}
	var req clusterJoinRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid json")
		return
	}
	defer r.Body.Close()
	if req.NodeID == "" || req.Addr == "" {
		writeError(w, http.StatusBadRequest, "node_id and addr are required")
		return
	}
	var err error
	role := "voter"
	if req.NonVoter {
		err = s.clusterNode.JoinNonVoter(req.NodeID, req.Addr)
		role = "non-voter"
	} else {
		err = s.clusterNode.Join(req.NodeID, req.Addr)
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "joined", "node_id": req.NodeID, "role": role})
}

// --- Cluster Leave ---

type clusterLeaveRequest struct {
	NodeID string `json:"node_id"`
}

func (s *HTTPServer) handleClusterLeave(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	if s.clusterNode == nil {
		writeError(w, http.StatusBadRequest, "clustering is not enabled")
		return
	}
	var req clusterLeaveRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid json")
		return
	}
	defer r.Body.Close()
	if req.NodeID == "" {
		writeError(w, http.StatusBadRequest, "node_id is required")
		return
	}
	if err := s.clusterNode.Leave(req.NodeID); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "removed", "node_id": req.NodeID})
}

// --- Cluster Status ---

func (s *HTTPServer) handleClusterStatus(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	if s.clusterNode == nil {
		writeJSON(w, http.StatusOK, map[string]interface{}{
			"enabled": false,
		})
		return
	}
	status := s.clusterNode.Status()
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"enabled": true,
		"cluster": status,
	})
}

// SetClusterNode sets the Raft node for cluster management endpoints.
func (s *HTTPServer) SetClusterNode(node *cluster.RaftNode) {
	s.clusterNode = node
}

// --- Helpers ---

func writeJSON(w http.ResponseWriter, code int, v interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	if v != nil {
		json.NewEncoder(w).Encode(v)
	}
}

func writeError(w http.ResponseWriter, code int, msg string) {
	writeJSON(w, code, map[string]string{"error": msg})
}
