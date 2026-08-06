package api

import (
	"net/http"
	"time"
)

// Liveness and readiness answer two different questions, and conflating them is
// actively dangerous in a clustered deployment.
//
// Liveness asks "is this process wedged?" — if it fails, the orchestrator kills
// the container. Readiness asks "should traffic go here right now?" — if it
// fails, the pod is pulled from the load balancer and left alone.
//
// A node that has lost quorum is not wedged. It is doing exactly the right
// thing: refusing writes and waiting for an election. If liveness reported that
// as failure, Kubernetes would restart every node in the cluster at the precise
// moment the cluster needed them stable to elect a leader — turning a
// recoverable partition into an outage. So liveness stays deliberately dumb,
// and everything situational goes into readiness.

// ReadinessState is the machine-readable body of a readiness response.
type ReadinessState struct {
	Ready bool `json:"ready"`
	// Reason is empty when ready, and human-readable when not. It is what an
	// operator sees in `kubectl describe pod`, so it has to name the actual
	// blocker.
	Reason string `json:"reason,omitempty"`

	Clustered bool   `json:"clustered"`
	NodeID    string `json:"node_id,omitempty"`
	State     string `json:"state,omitempty"`
	LeaderID  string `json:"leader_id,omitempty"`

	UptimeSeconds int64 `json:"uptime_seconds"`
}

// startedAt is when this process began serving. Readiness uses it for a grace
// period: a node that has been up for two seconds has not failed to join the
// cluster, it simply has not finished trying.
var startedAt = time.Now()

// readinessGrace is how long after start a node may be un-joined without being
// reported unready-with-a-reason. It only affects the message, never the
// verdict — a node that is not ready is not ready.
const readinessGrace = 30 * time.Second

// Readiness computes whether this node should receive traffic.
//
// The rule for a clustered node is that it must know who the leader is. Not
// "must be the leader" — a follower serves reads and redirects writes, which is
// useful. But a node that cannot name a leader can do neither: every write it
// receives has nowhere to go, and it would answer reads from a log it has no
// reason to believe is current.
func (s *HTTPServer) Readiness() ReadinessState {
	st := ReadinessState{
		UptimeSeconds: int64(time.Since(startedAt).Seconds()),
	}

	// Readiness follows the *control* plane, not the queue plane. A data node
	// belongs to no queue group at all, and asking that group whether this node
	// is ready would answer "there is no cluster" and mark every data node ready
	// before it had joined anything.
	ctl := s.control()
	if ctl == nil {
		// Single-node mode: if the process is serving, it is ready. There is no
		// cluster to be out of.
		st.Ready = true
		return st
	}

	status := ctl.Status()
	st.Clustered = true
	st.NodeID = status.NodeID
	st.State = status.State
	st.LeaderID = status.LeaderID

	if status.LeaderID == "" {
		st.Reason = "no leader elected yet"
		if time.Since(startedAt) < readinessGrace {
			st.Reason = "joining cluster"
		}
		return st
	}

	st.Ready = true
	return st
}

// handleLiveness reports that the process is running. It must not consult
// cluster state — see the note at the top of this file.
func (s *HTTPServer) handleLiveness(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"status": "ok"})
}

// handleReadiness reports whether this node should receive traffic, using 503
// so a load balancer and a probe both read it correctly without parsing the
// body.
func (s *HTTPServer) handleReadiness(w http.ResponseWriter, r *http.Request) {
	st := s.Readiness()
	code := http.StatusOK
	if !st.Ready {
		code = http.StatusServiceUnavailable
	}
	writeJSON(w, code, st)
}
