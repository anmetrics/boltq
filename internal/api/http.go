package api

import (
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/boltq/boltq/internal/broker"
	"github.com/boltq/boltq/internal/cluster"
	"github.com/boltq/boltq/internal/metrics"
	"github.com/boltq/boltq/pkg/protocol"
)

// HTTPServer provides the REST API for the message broker.
type HTTPServer struct {
	broker      broker.BrokerIface
	metrics     *metrics.Metrics
	apiKey      string
	clusterNode *cluster.RaftNode // nil if clustering is disabled
	mux         *http.ServeMux
	server      *http.Server
}

// NewHTTPServer creates a new HTTP API server.
func NewHTTPServer(b broker.BrokerIface, m *metrics.Metrics, apiKey string) *HTTPServer {
	s := &HTTPServer{
		broker:  b,
		metrics: m,
		apiKey:  apiKey,
		mux:     http.NewServeMux(),
	}
	s.registerRoutes()
	return s
}

func (s *HTTPServer) registerRoutes() {
	s.mux.HandleFunc("/publish", s.auth(s.handlePublish))
	s.mux.HandleFunc("/publish/topic", s.auth(s.handlePublishTopic))
	s.mux.HandleFunc("/consume", s.auth(s.handleConsume))
	s.mux.HandleFunc("/ack", s.auth(s.handleAck))
	s.mux.HandleFunc("/nack", s.auth(s.handleNack))
	s.mux.HandleFunc("/subscribe", s.auth(s.handleSubscribe))
	s.mux.HandleFunc("/stats", s.auth(s.handleStats))
	s.mux.HandleFunc("/metrics", s.handleMetrics)
	s.mux.HandleFunc("/health", s.handleHealth)

	// Cluster management routes (always registered, return 404/error if clustering disabled).
	s.mux.HandleFunc("/cluster/join", s.auth(s.handleClusterJoin))
	s.mux.HandleFunc("/cluster/leave", s.auth(s.handleClusterLeave))
	s.mux.HandleFunc("/cluster/status", s.auth(s.handleClusterStatus))
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
	log.Printf("[http] listening on %s", addr)
	return s.server.ListenAndServe()
}

// Shutdown gracefully stops the HTTP server.
func (s *HTTPServer) Shutdown() error {
	if s.server != nil {
		return s.server.Close()
	}
	return nil
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

// --- Publish (Work Queue) ---

type publishRequest struct {
	Topic   string            `json:"topic"`
	Payload json.RawMessage   `json:"payload"`
	Headers map[string]string `json:"headers"`
}

type publishResponse struct {
	ID    string `json:"id"`
	Topic string `json:"topic"`
}

func (s *HTTPServer) handlePublish(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}

	body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20)) // 1MB limit
	if err != nil {
		writeError(w, http.StatusBadRequest, "read body: "+err.Error())
		return
	}
	defer r.Body.Close()

	var req publishRequest
	if err := json.Unmarshal(body, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid json: "+err.Error())
		return
	}

	if req.Topic == "" {
		writeError(w, http.StatusBadRequest, "topic is required")
		return
	}

	msg := newMessage(req.Topic, req.Payload, req.Headers)
	if err := s.broker.Publish(req.Topic, msg); err != nil {
		if nle, ok := cluster.IsNotLeaderError(err); ok {
			writeNotLeader(w, nle)
			return
		}
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}

	s.metrics.IncPublished()
	writeJSON(w, http.StatusOK, publishResponse{ID: msg.ID, Topic: msg.Topic})
}

// --- Publish (Pub/Sub Topic) ---

func (s *HTTPServer) handlePublishTopic(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}

	body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		writeError(w, http.StatusBadRequest, "read body: "+err.Error())
		return
	}
	defer r.Body.Close()

	var req publishRequest
	if err := json.Unmarshal(body, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid json: "+err.Error())
		return
	}

	if req.Topic == "" {
		writeError(w, http.StatusBadRequest, "topic is required")
		return
	}

	msg := newMessage(req.Topic, req.Payload, req.Headers)
	if err := s.broker.PublishTopic(req.Topic, msg); err != nil {
		if nle, ok := cluster.IsNotLeaderError(err); ok {
			writeNotLeader(w, nle)
			return
		}
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}

	s.metrics.IncPublished()
	writeJSON(w, http.StatusOK, publishResponse{ID: msg.ID, Topic: msg.Topic})
}

// --- Consume ---

type consumeResponse struct {
	ID        string            `json:"id"`
	Topic     string            `json:"topic"`
	Payload   json.RawMessage   `json:"payload"`
	Headers   map[string]string `json:"headers,omitempty"`
	Timestamp int64             `json:"timestamp"`
	Retry     int               `json:"retry"`
}

func (s *HTTPServer) handleConsume(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}

	topic := r.URL.Query().Get("topic")
	if topic == "" {
		writeError(w, http.StatusBadRequest, "topic query param is required")
		return
	}

	// Non-blocking consume for HTTP (don't block the HTTP connection).
	msg := s.broker.TryConsume(topic)
	if msg == nil {
		writeJSON(w, http.StatusNoContent, nil)
		return
	}

	s.metrics.IncConsumed()
	writeJSON(w, http.StatusOK, consumeResponse{
		ID:        msg.ID,
		Topic:     msg.Topic,
		Payload:   msg.Payload,
		Headers:   msg.Headers,
		Timestamp: msg.Timestamp,
		Retry:     msg.Retry,
	})
}

// --- Subscribe (SSE for pub/sub) ---

func (s *HTTPServer) handleSubscribe(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}

	topic := r.URL.Query().Get("topic")
	subscriberID := r.URL.Query().Get("id")
	if topic == "" || subscriberID == "" {
		writeError(w, http.StatusBadRequest, "topic and id query params required")
		return
	}

	flusher, ok := w.(http.Flusher)
	if !ok {
		writeError(w, http.StatusInternalServerError, "streaming not supported")
		return
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")

	ch := s.broker.Subscribe(topic, subscriberID, 256)
	defer s.broker.Unsubscribe(topic, subscriberID)

	ctx := r.Context()
	for {
		select {
		case <-ctx.Done():
			return
		case msg, ok := <-ch:
			if !ok {
				return
			}
			data, _ := json.Marshal(consumeResponse{
				ID:        msg.ID,
				Topic:     msg.Topic,
				Payload:   msg.Payload,
				Headers:   msg.Headers,
				Timestamp: msg.Timestamp,
			})
			fmt.Fprintf(w, "data: %s\n\n", data)
			flusher.Flush()
		}
	}
}

// --- ACK ---

type ackRequest struct {
	ID string `json:"id"`
}

func (s *HTTPServer) handleAck(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}

	var req ackRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid json")
		return
	}
	defer r.Body.Close()

	if req.ID == "" {
		writeError(w, http.StatusBadRequest, "id is required")
		return
	}

	if err := s.broker.Ack(req.ID); err != nil {
		if nle, ok := cluster.IsNotLeaderError(err); ok {
			writeNotLeader(w, nle)
			return
		}
		if strings.Contains(err.Error(), "not found") {
			writeError(w, http.StatusNotFound, err.Error())
		} else {
			writeError(w, http.StatusInternalServerError, err.Error())
		}
		return
	}

	s.metrics.IncAcked()
	writeJSON(w, http.StatusOK, map[string]string{"status": "acked"})
}

// --- NACK ---

func (s *HTTPServer) handleNack(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}

	var req ackRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid json")
		return
	}
	defer r.Body.Close()

	if req.ID == "" {
		writeError(w, http.StatusBadRequest, "id is required")
		return
	}

	if err := s.broker.Nack(req.ID); err != nil {
		if nle, ok := cluster.IsNotLeaderError(err); ok {
			writeNotLeader(w, nle)
			return
		}
		if strings.Contains(err.Error(), "not found") {
			writeError(w, http.StatusNotFound, err.Error())
		} else {
			writeError(w, http.StatusInternalServerError, err.Error())
		}
		return
	}

	s.metrics.IncNacked()
	writeJSON(w, http.StatusOK, map[string]string{"status": "nacked"})
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

// --- Cluster Join ---

type clusterJoinRequest struct {
	NodeID string `json:"node_id"`
	Addr   string `json:"addr"`
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
	if err := s.clusterNode.Join(req.NodeID, req.Addr); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "joined", "node_id": req.NodeID})
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

func writeNotLeader(w http.ResponseWriter, nle *cluster.NotLeaderError) {
	writeJSON(w, http.StatusTemporaryRedirect, map[string]string{
		"error":     "not leader",
		"leader":    nle.Leader,
		"leader_id": nle.LeaderID,
	})
}

func newMessage(topic string, payload json.RawMessage, headers map[string]string) *protocol.Message {
	return protocol.NewMessage(topic, payload, headers)
}
