package api

import (
	"encoding/json"
	"net/http"
	"time"

	"github.com/boltq/boltq/internal/cluster"
)

// Control-plane endpoints: node registration and liveness.
//
// Both are cluster-internal. They are mounted on the admin listener, which the
// deployment manifests keep off any internet-facing Service and behind a
// NetworkPolicy — a node that can register can also be fenced, and a node that
// can heartbeat can keep a dead one looking alive.

// SetController attaches the controller so heartbeats can reach it. It is nil
// on a node that is not running one, which is every node that is not the Raft
// leader at the moment — but the endpoint stays mounted regardless, because
// leadership moves and the listener does not.
func (s *HTTPServer) SetController(c *cluster.Controller) {
	s.controller = c
}

// handleClusterRegister records a node in the replicated broker registry.
//
// Registration goes through Raft rather than into the controller's memory: it
// is durable state that must survive a controller failover. A node that had
// only told the previous controller about itself would silently disappear the
// moment leadership moved.
func (s *HTTPServer) handleClusterRegister(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	ctl := s.control()
	if ctl == nil {
		writeError(w, http.StatusBadRequest, "clustering is not enabled")
		return
	}

	var info cluster.BrokerInfo
	if err := json.NewDecoder(r.Body).Decode(&info); err != nil {
		writeError(w, http.StatusBadRequest, "invalid json")
		return
	}
	defer r.Body.Close()
	if info.NodeID == "" {
		writeError(w, http.StatusBadRequest, "node_id is required")
		return
	}

	if !ctl.IsLeader() {
		// 421 Misdirected Request, with the leader named. The caller retries
		// against the right node instead of treating a correct redirect as an
		// outage.
		writeJSON(w, http.StatusMisdirectedRequest, map[string]string{
			"error":     "not the controller",
			"leader_id": ctl.LeaderID(),
		})
		return
	}

	resp, err := ctl.Apply(&cluster.RaftCommand{
		Type: cluster.CmdMetaRegisterBroker,
		Meta: &cluster.MetaCommand{Broker: &info, Timestamp: time.Now().UnixNano()},
	}, 5*time.Second)
	if err != nil {
		writeError(w, http.StatusServiceUnavailable, err.Error())
		return
	}
	if resp.Error != nil {
		writeError(w, http.StatusInternalServerError, resp.Error.Error())
		return
	}

	// Count the registration as a heartbeat. Otherwise a node that registers
	// and then waits a full interval before its first beat looks silent for
	// that whole window.
	if s.controller != nil {
		s.controller.Heartbeat(info.NodeID)
	}

	writeJSON(w, http.StatusOK, map[string]string{"status": "registered", "node_id": info.NodeID})
}

// handleClusterHeartbeat records that a node is alive.
//
// Deliberately not a Raft write. A 200-node cluster beating every five seconds
// would push 40 quorum-fsynced writes per second of pure liveness noise through
// consensus, to record something that is stale one tick later. The controller
// keeps this in memory and replicates only its conclusions.
func (s *HTTPServer) handleClusterHeartbeat(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	ctl := s.control()
	if ctl == nil {
		writeError(w, http.StatusBadRequest, "clustering is not enabled")
		return
	}

	var req struct {
		NodeID string `json:"node_id"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid json")
		return
	}
	defer r.Body.Close()
	if req.NodeID == "" {
		writeError(w, http.StatusBadRequest, "node_id is required")
		return
	}

	if !ctl.IsLeader() {
		writeJSON(w, http.StatusMisdirectedRequest, map[string]string{
			"error":     "not the controller",
			"leader_id": ctl.LeaderID(),
		})
		return
	}
	if s.controller == nil {
		writeError(w, http.StatusServiceUnavailable, "controller is not running")
		return
	}

	s.controller.Heartbeat(req.NodeID)
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

// handleClusterMeta commits a metadata command submitted by a data node.
//
// Only ISR reports are accepted. A data node has exactly one fact the
// controller cannot observe for itself — which replicas are caught up with the
// partitions it leads — and that is all this endpoint is for. Accepting
// arbitrary metadata commands here would let any node that reaches the admin
// port reassign leadership, which is the controller's decision alone.
func (s *HTTPServer) handleClusterMeta(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	ctl := s.control()
	if ctl == nil {
		writeError(w, http.StatusBadRequest, "clustering is not enabled")
		return
	}

	var cmd cluster.RaftCommand
	if err := json.NewDecoder(r.Body).Decode(&cmd); err != nil {
		writeError(w, http.StatusBadRequest, "invalid json")
		return
	}
	defer r.Body.Close()

	if cmd.Type != cluster.CmdMetaUpdateISR {
		writeError(w, http.StatusForbidden, "only ISR reports may be submitted by a data node")
		return
	}
	if !ctl.IsLeader() {
		writeJSON(w, http.StatusMisdirectedRequest, map[string]string{
			"error":     "not the controller",
			"leader_id": ctl.LeaderID(),
		})
		return
	}

	resp, err := ctl.Apply(&cmd, 5*time.Second)
	if err != nil {
		writeError(w, http.StatusServiceUnavailable, err.Error())
		return
	}
	if resp.Error != nil {
		// A stale epoch lands here: the reporting node was demoted between
		// measuring and reporting. It is expected, not exceptional.
		writeError(w, http.StatusConflict, resp.Error.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "applied"})
}

// handleClusterCreateTopic places a new topic's replicas and records it.
//
// Placement happens on the controller rather than wherever the request landed,
// because it needs the whole live broker list and the whole current load
// picture. Two nodes placing topics from partial views would pile replicas onto
// the same brokers and each believe it had spread them.
func (s *HTTPServer) handleClusterCreateTopic(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	ctl := s.control()
	if ctl == nil {
		writeError(w, http.StatusBadRequest, "clustering is not enabled")
		return
	}

	var req struct {
		Name              string `json:"name"`
		Partitions        int32  `json:"partitions"`
		ReplicationFactor int    `json:"replication_factor"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid json")
		return
	}
	defer r.Body.Close()
	if req.Name == "" || req.Partitions <= 0 {
		writeError(w, http.StatusBadRequest, "name and a positive partitions count are required")
		return
	}
	if req.ReplicationFactor <= 0 {
		req.ReplicationFactor = 3
	}

	if !ctl.IsLeader() {
		writeJSON(w, http.StatusMisdirectedRequest, map[string]string{
			"error":     "not the controller",
			"leader_id": ctl.LeaderID(),
		})
		return
	}
	if s.controller == nil {
		writeError(w, http.StatusServiceUnavailable, "controller is not running")
		return
	}

	if err := s.controller.CreateTopic(req.Name, req.Partitions, req.ReplicationFactor); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	assignments := make([]*cluster.PartitionAssignment, 0, req.Partitions)
	for pid := int32(0); pid < req.Partitions; pid++ {
		if a, ok := ctl.Metadata().Assignment(req.Name, pid); ok {
			assignments = append(assignments, a)
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"status":     "created",
		"name":       req.Name,
		"partitions": assignments,
	})
}

// handleClusterMetadata exposes the replicated control-plane state.
//
// Readable on any node, not just the controller: it is replicated, and forcing
// every metadata read through the leader would make the leader the bottleneck
// for something every node already has a current copy of.
func (s *HTTPServer) handleClusterMetadata(w http.ResponseWriter, r *http.Request) {
	ctl := s.control()
	if ctl == nil {
		writeError(w, http.StatusBadRequest, "clustering is not enabled")
		return
	}
	meta := ctl.Metadata()
	writeJSON(w, http.StatusOK, map[string]any{
		"version":    meta.Version(),
		"brokers":    meta.Brokers(),
		"topics":     meta.Topics(),
		"partitions": meta.Assignments(),
	})
}
