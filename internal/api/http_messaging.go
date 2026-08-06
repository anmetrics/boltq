package api

import (
	"encoding/json"
	"net/http"
	"strconv"
)

// MessagingStats is the read-only view the admin API exposes over the
// messaging subsystem. It is an interface rather than a concrete type so that
// internal/api does not import the streaming packages — which would create an
// import cycle the moment the gateway wants to serve an admin route.
type MessagingStats interface {
	StreamStats() any
	PresenceStats() any
	GatewayStats() any
	SignalStats() any
	PushStats() any
	// ReplicationStats reports whether this node leads or follows, and how far
	// its replicas are behind. Without it the messaging plane has no health
	// signal at all — a node can lose stream leadership while /cluster/status
	// still reports a healthy Raft quorum.
	ReplicationStats() any
	// TopicDetail returns per-partition detail for one topic, or nil when the
	// topic does not exist.
	TopicDetail(name string) any
	// CursorsFor returns the committed positions for a topic partition.
	CursorsFor(topic string, partition int32, group string) any
}

// Handle mounts an additional handler on the admin server's mux.
//
// It exists so the WebSocket gateway can share the admin listener in small
// deployments. Production should give the gateway its own port: end-user
// traffic and operator traffic have different exposure, different rate limits
// and different failure blast radii.
func (s *HTTPServer) Handle(pattern string, h http.Handler) {
	s.mux.Handle(pattern, h)
}

// SetMessagingStats registers the messaging admin endpoints.
func (s *HTTPServer) SetMessagingStats(m MessagingStats) {
	if m == nil {
		return
	}
	s.messaging = m

	s.mux.HandleFunc("/streams", s.cors(s.auth(s.handleStreamStats)))
	s.mux.HandleFunc("/streams/topic", s.cors(s.auth(s.handleStreamTopic)))
	s.mux.HandleFunc("/streams/cursors", s.cors(s.auth(s.handleStreamCursors)))
	s.mux.HandleFunc("/presence", s.cors(s.auth(s.handlePresenceStats)))
	s.mux.HandleFunc("/gateway/stats", s.cors(s.auth(s.handleGatewayStats)))
	s.mux.HandleFunc("/replication", s.cors(s.auth(s.handleReplicationStats)))
	s.mux.HandleFunc("/messaging/overview", s.cors(s.auth(s.handleMessagingOverview)))
}

func (s *HTTPServer) handleStreamStats(w http.ResponseWriter, r *http.Request) {
	writeMessagingJSON(w, s.messaging.StreamStats())
}

func (s *HTTPServer) handleStreamTopic(w http.ResponseWriter, r *http.Request) {
	name := r.URL.Query().Get("name")
	if name == "" {
		http.Error(w, `{"error":"name is required"}`, http.StatusBadRequest)
		return
	}
	detail := s.messaging.TopicDetail(name)
	if detail == nil {
		http.Error(w, `{"error":"topic not found"}`, http.StatusNotFound)
		return
	}
	writeMessagingJSON(w, detail)
}

func (s *HTTPServer) handleStreamCursors(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	topic := q.Get("topic")
	if topic == "" {
		http.Error(w, `{"error":"topic is required"}`, http.StatusBadRequest)
		return
	}
	partition := 0
	if v := q.Get("partition"); v != "" {
		p, err := strconv.Atoi(v)
		if err != nil {
			http.Error(w, `{"error":"partition must be an integer"}`, http.StatusBadRequest)
			return
		}
		partition = p
	}
	writeMessagingJSON(w, s.messaging.CursorsFor(topic, int32(partition), q.Get("group")))
}

func (s *HTTPServer) handlePresenceStats(w http.ResponseWriter, r *http.Request) {
	writeMessagingJSON(w, s.messaging.PresenceStats())
}

func (s *HTTPServer) handleGatewayStats(w http.ResponseWriter, r *http.Request) {
	writeMessagingJSON(w, s.messaging.GatewayStats())
}

func (s *HTTPServer) handleReplicationStats(w http.ResponseWriter, r *http.Request) {
	writeMessagingJSON(w, s.messaging.ReplicationStats())
}

func (s *HTTPServer) handleMessagingOverview(w http.ResponseWriter, r *http.Request) {
	writeMessagingJSON(w, map[string]any{
		"streams":     s.messaging.StreamStats(),
		"presence":    s.messaging.PresenceStats(),
		"gateway":     s.messaging.GatewayStats(),
		"signals":     s.messaging.SignalStats(),
		"push":        s.messaging.PushStats(),
		"replication": s.messaging.ReplicationStats(),
	})
}

func writeMessagingJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	if v == nil {
		w.Write([]byte("null"))
		return
	}
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	if err := enc.Encode(v); err != nil {
		http.Error(w, `{"error":"encode failed"}`, http.StatusInternalServerError)
	}
}
