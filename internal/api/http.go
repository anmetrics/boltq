package api

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"strings"
	"time"

	"runtime"

	"github.com/boltq/boltq/internal/broker"
	"github.com/boltq/boltq/internal/cache"
	"github.com/boltq/boltq/internal/cluster"
	"github.com/boltq/boltq/internal/config"
	"github.com/boltq/boltq/internal/metrics"
	"github.com/boltq/boltq/internal/presence"
	"github.com/boltq/boltq/internal/stream"
	"github.com/boltq/boltq/pkg/protocol"
)

// HTTPServer provides the admin REST API (stats, metrics, health, cluster management).
// All messaging operations (publish, consume, ack, nack) go through the TCP protocol.
type HTTPServer struct {
	broker      broker.BrokerIface
	metrics     *metrics.Metrics
	apiKey      string
	tlsConfig   config.TLSConfig
	clusterNode *cluster.RaftNode   // queue-plane group; nil if the queue plane is not clustered
	metaNode    cluster.ControlNode // control-plane group; nil if clustering is disabled
	controller  *cluster.Controller // nil if this node runs no controller
	cache       *cache.Store        // nil if cache is disabled
	defaultTTL  int64               // default TTL for cache entries in ms
	mux         *http.ServeMux
	messaging   MessagingStats
	streamLog   *stream.Log        // nil unless the messaging plane is enabled
	presenceReg *presence.Registry // nil unless presence is enabled
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

	// Orchestrator probes. Kept separate from /health, which predates them and
	// is what existing dashboards poll: changing its semantics would silently
	// repurpose whatever is already watching it.
	s.mux.HandleFunc("/livez", s.cors(s.handleLiveness))
	s.mux.HandleFunc("/readyz", s.cors(s.handleReadiness))

	// Queue management.
	s.mux.HandleFunc("/queues/purge", s.cors(s.auth(s.handlePurgeQueue)))
	s.mux.HandleFunc("/dead-letters/purge", s.cors(s.auth(s.handlePurgeDeadLetters)))

	// Messaging endpoints (for testing/k6/web).
	s.mux.HandleFunc("/publish", s.cors(s.auth(s.handlePublish)))
	s.mux.HandleFunc("/consume", s.cors(s.auth(s.handleConsume)))
	s.mux.HandleFunc("/ack", s.cors(s.auth(s.handleAck)))

	// Exchange management routes.
	s.mux.HandleFunc("/exchange/declare", s.cors(s.auth(s.handleExchangeDeclare)))
	s.mux.HandleFunc("/exchange/delete", s.cors(s.auth(s.handleExchangeDelete)))
	s.mux.HandleFunc("/exchange/bind", s.cors(s.auth(s.handleExchangeBind)))
	s.mux.HandleFunc("/exchange/unbind", s.cors(s.auth(s.handleExchangeUnbind)))
	s.mux.HandleFunc("/exchange/publish", s.cors(s.auth(s.handleExchangePublish)))

	// Cluster management routes.
	s.mux.HandleFunc("/cluster/join", s.cors(s.auth(s.handleClusterJoin)))
	s.mux.HandleFunc("/cluster/leave", s.cors(s.auth(s.handleClusterLeave)))
	s.mux.HandleFunc("/cluster/status", s.cors(s.auth(s.handleClusterStatus)))

	// Control plane: node registration, liveness, and the replicated view of
	// who leads what.
	s.mux.HandleFunc("/cluster/register", s.cors(s.auth(s.handleClusterRegister)))
	s.mux.HandleFunc("/cluster/heartbeat", s.cors(s.auth(s.handleClusterHeartbeat)))
	s.mux.HandleFunc("/cluster/metadata", s.cors(s.auth(s.handleClusterMetadata)))
	s.mux.HandleFunc("/cluster/topics", s.cors(s.auth(s.handleClusterCreateTopic)))
	s.mux.HandleFunc("/cluster/meta", s.cors(s.auth(s.handleClusterMeta)))

	// Peer-to-peer write routing. Cluster-internal only — see http_internal.go.
	s.mux.HandleFunc("/internal/append", s.auth(s.handleInternalAppend))
	s.mux.HandleFunc("/internal/presence", s.auth(s.handleInternalPresence))
	s.mux.HandleFunc("/internal/presence/batch", s.auth(s.handleInternalPresenceBatch))

	// Cache/KV store routes.
	s.registerCacheRoutes()
}

// SetMetadataNode attaches the control-plane consensus group.
//
// It is separate from SetClusterNode because the two groups are separate: a
// node commonly belongs to the control plane and not to the queue plane, which
// is exactly the arrangement that keeps a hundred data nodes from replicating
// every queue write in the cluster.
func (s *HTTPServer) SetMetadataNode(n cluster.ControlNode) {
	s.metaNode = n
}

// control returns the control-plane group, falling back to the combined node so
// a cluster that has not yet split its groups keeps working.
func (s *HTTPServer) control() cluster.ControlNode {
	if s.metaNode != nil {
		return s.metaNode
	}
	if s.clusterNode != nil {
		return s.clusterNode
	}
	return nil
}

// ServeHTTP lets the admin API be mounted on a listener the caller owns —
// a test server, or a process that terminates TLS elsewhere — instead of only
// through Start.
func (s *HTTPServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.mux.ServeHTTP(w, r)
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

	if s.cache != nil {
		overview["cache"] = map[string]interface{}{
			"enabled": true,
			"stats":   s.cache.GetStats(),
		}
	} else {
		overview["cache"] = map[string]interface{}{
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
		Topic    string          `json:"topic"`
		Payload  json.RawMessage `json:"payload"`
		Delay    int64           `json:"delay"`
		TTL      int64           `json:"ttl"`
		Priority int             `json:"priority"`
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
	msg.Priority = req.Priority

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

	// TryConsume, not Consume: Consume blocks on a condition variable until a
	// message arrives, which in an HTTP handler means the request never returns.
	// A single poll against an empty topic would pin a goroutine and a
	// connection for the lifetime of the process, and enough of them exhaust
	// the server — no authentication required beyond reaching this endpoint.
	//
	// The 404 below was always the intended answer for an empty queue; it was
	// simply unreachable.
	msg := s.broker.TryConsume(topic)
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

// --- Exchange Declare ---

func (s *HTTPServer) handleExchangeDeclare(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	var req struct {
		Name    string `json:"name"`
		Type    string `json:"type"`
		Durable bool   `json:"durable"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid json")
		return
	}
	defer r.Body.Close()
	if req.Name == "" {
		writeError(w, http.StatusBadRequest, "name is required")
		return
	}
	if req.Type == "" {
		req.Type = "direct"
	}
	if err := s.broker.ExchangeDeclare(req.Name, broker.ExchangeType(req.Type), req.Durable); err != nil {
		if nle, ok := cluster.IsNotLeaderError(err); ok {
			writeJSON(w, http.StatusTemporaryRedirect, map[string]string{"error": "not leader", "leader": nle.Leader, "leader_id": nle.LeaderID})
			return
		}
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

// --- Exchange Delete ---

func (s *HTTPServer) handleExchangeDelete(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	var req struct {
		Name string `json:"name"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid json")
		return
	}
	defer r.Body.Close()
	if req.Name == "" {
		writeError(w, http.StatusBadRequest, "name is required")
		return
	}
	if err := s.broker.ExchangeDelete(req.Name); err != nil {
		if nle, ok := cluster.IsNotLeaderError(err); ok {
			writeJSON(w, http.StatusTemporaryRedirect, map[string]string{"error": "not leader", "leader": nle.Leader, "leader_id": nle.LeaderID})
			return
		}
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

// --- Exchange Bind ---

func (s *HTTPServer) handleExchangeBind(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	var req struct {
		Exchange   string            `json:"exchange"`
		Queue      string            `json:"queue"`
		BindingKey string            `json:"binding_key"`
		Headers    map[string]string `json:"headers,omitempty"`
		MatchAll   bool              `json:"match_all,omitempty"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid json")
		return
	}
	defer r.Body.Close()
	if req.Exchange == "" || req.Queue == "" {
		writeError(w, http.StatusBadRequest, "exchange and queue are required")
		return
	}
	if err := s.broker.BindQueue(req.Exchange, req.Queue, req.BindingKey, req.Headers, req.MatchAll); err != nil {
		if nle, ok := cluster.IsNotLeaderError(err); ok {
			writeJSON(w, http.StatusTemporaryRedirect, map[string]string{"error": "not leader", "leader": nle.Leader, "leader_id": nle.LeaderID})
			return
		}
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

// --- Exchange Unbind ---

func (s *HTTPServer) handleExchangeUnbind(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	var req struct {
		Exchange   string `json:"exchange"`
		Queue      string `json:"queue"`
		BindingKey string `json:"binding_key"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid json")
		return
	}
	defer r.Body.Close()
	if req.Exchange == "" || req.Queue == "" {
		writeError(w, http.StatusBadRequest, "exchange and queue are required")
		return
	}
	if err := s.broker.UnbindQueue(req.Exchange, req.Queue, req.BindingKey); err != nil {
		if nle, ok := cluster.IsNotLeaderError(err); ok {
			writeJSON(w, http.StatusTemporaryRedirect, map[string]string{"error": "not leader", "leader": nle.Leader, "leader_id": nle.LeaderID})
			return
		}
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

// --- Exchange Publish ---

func (s *HTTPServer) handleExchangePublish(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	var req struct {
		Exchange   string            `json:"exchange"`
		RoutingKey string            `json:"routing_key"`
		Payload    json.RawMessage   `json:"payload"`
		Headers    map[string]string `json:"headers,omitempty"`
		Priority   int               `json:"priority"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid json")
		return
	}
	defer r.Body.Close()

	msg := protocol.NewMessage("", req.Payload, req.Headers)
	msg.Priority = req.Priority

	if err := s.broker.PublishExchange(req.Exchange, req.RoutingKey, msg); err != nil {
		if nle, ok := cluster.IsNotLeaderError(err); ok {
			writeJSON(w, http.StatusTemporaryRedirect, map[string]string{"error": "not leader", "leader": nle.Leader, "leader_id": nle.LeaderID})
			return
		}
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "published", "id": msg.ID})
}

// --- Cluster Join ---

type clusterJoinRequest struct {
	NodeID string `json:"node_id"`
	// Addr is the queue-group Raft address. Empty means this node runs no queue
	// plane and should join the control plane only — the normal case for a data
	// node in a large cluster.
	Addr string `json:"addr"`
	// MetaAddr is the control-group Raft address. Every node joins that group.
	MetaAddr string `json:"meta_addr,omitempty"`
	NonVoter bool   `json:"non_voter"` // Join as read replica (non-voter)
}

func (s *HTTPServer) handleClusterJoin(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}

	var req clusterJoinRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid json")
		return
	}
	defer r.Body.Close()
	if req.NodeID == "" {
		writeError(w, http.StatusBadRequest, "node_id is required")
		return
	}
	if req.Addr == "" && req.MetaAddr == "" {
		writeError(w, http.StatusBadRequest, "addr or meta_addr is required")
		return
	}

	// Two groups, two memberships. A node joins the control plane always and the
	// queue plane only if it serves queues — which is what stops a data node from
	// receiving every queue write in the cluster.
	joined := make([]string, 0, 2)

	if req.MetaAddr != "" && s.metaNode != nil {
		var err error
		if req.NonVoter {
			err = s.metaNode.JoinNonVoter(req.NodeID, req.MetaAddr)
		} else {
			err = s.metaNode.Join(req.NodeID, req.MetaAddr)
		}
		if err != nil {
			writeError(w, http.StatusInternalServerError, err.Error())
			return
		}
		joined = append(joined, "metadata")
	}

	if req.Addr != "" && s.clusterNode != nil {
		var err error
		if req.NonVoter {
			err = s.clusterNode.JoinNonVoter(req.NodeID, req.Addr)
		} else {
			err = s.clusterNode.Join(req.NodeID, req.Addr)
		}
		if err != nil {
			writeError(w, http.StatusInternalServerError, err.Error())
			return
		}
		joined = append(joined, "queue")
	}

	if len(joined) == 0 {
		writeError(w, http.StatusBadRequest, "no consensus group on this node accepted the join")
		return
	}

	role := "voter"
	if req.NonVoter {
		role = "non-voter"
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"status": "joined", "node_id": req.NodeID, "role": role, "groups": joined,
	})
}

type clusterLeaveRequest struct {
	NodeID string `json:"node_id"`
}

func (s *HTTPServer) handleClusterLeave(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	if s.clusterNode == nil && s.metaNode == nil {
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
	// Leave both groups. A node removed from one but not the other lingers as a
	// dead member forever, and in the control plane a dead voter is quorum the
	// cluster can never reach.
	var left []string
	if s.metaNode != nil {
		if err := s.metaNode.Leave(req.NodeID); err != nil {
			writeError(w, http.StatusInternalServerError, err.Error())
			return
		}
		left = append(left, "metadata")
	}
	if s.clusterNode != nil {
		if err := s.clusterNode.Leave(req.NodeID); err != nil {
			// The node may simply never have been a queue member, which is the
			// normal case for a data node. Not a failure.
			log.Printf("[http] leave queue group for %s: %v", req.NodeID, err)
		} else {
			left = append(left, "queue")
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"status": "removed", "node_id": req.NodeID, "groups": left})
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
