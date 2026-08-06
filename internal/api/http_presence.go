package api

import (
	"encoding/json"
	"net/http"

	"github.com/boltq/boltq/internal/presence"
)

// The peer-facing presence endpoint. Cluster-internal, on the admin listener,
// behind the API key and the NetworkPolicy — a caller who can reach it can both
// read who is online and assert that a session exists.
//
// It answers only for shards this node owns. It does not forward: a node asked
// about a user it does not own is being asked by a peer with stale metadata,
// and forwarding would turn one stale view into a chain of hops. Answering
// "not mine" lets the caller re-resolve against metadata that has since caught
// up.

// SetPresenceRegistry attaches the local presence shards.
func (s *HTTPServer) SetPresenceRegistry(r *presence.Registry) {
	s.presenceReg = r
}

// handleInternalPresenceBatch answers which of a set of users have no session
// in the shards this node owns.
//
// It exists because fan-out asks about every recipient of a message at once. One
// request per recipient would make a hundred-member group cost a hundred round
// trips; one request per owning node makes it cost one per node.
func (s *HTTPServer) handleInternalPresenceBatch(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	if s.presenceReg == nil {
		writeError(w, http.StatusServiceUnavailable, "presence is not enabled on this node")
		return
	}

	var req struct {
		Users []string `json:"users"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid json")
		return
	}
	defer r.Body.Close()

	// An empty result must serialise as [] rather than null: a caller decoding
	// null into a slice gets nil, which reads identically to "the field was
	// missing" and hides a malformed response.
	offline := make([]string, 0, len(req.Users))
	for _, u := range req.Users {
		if u != "" && !s.presenceReg.Online(u) {
			offline = append(offline, u)
		}
	}
	writeJSON(w, http.StatusOK, map[string][]string{"offline": offline})
}

func (s *HTTPServer) handleInternalPresence(w http.ResponseWriter, r *http.Request) {
	if s.presenceReg == nil {
		writeError(w, http.StatusServiceUnavailable, "presence is not enabled on this node")
		return
	}

	switch r.Method {
	case http.MethodGet:
		userID := r.URL.Query().Get("user")
		if userID == "" {
			writeError(w, http.StatusBadRequest, "user is required")
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"user_id":  userID,
			"sessions": s.presenceReg.Sessions(userID),
		})

	case http.MethodPost:
		var sess presence.Session
		if err := json.NewDecoder(r.Body).Decode(&sess); err != nil {
			writeError(w, http.StatusBadRequest, "invalid json")
			return
		}
		defer r.Body.Close()
		if sess.UserID == "" || sess.DeviceID == "" {
			writeError(w, http.StatusBadRequest, "user_id and device_id are required")
			return
		}
		// NodeID is whatever the reporting node put there — the node holding the
		// socket, not this one. Overwriting it would route every delivery to the
		// shard owner, which holds no socket at all.
		if _, err := s.presenceReg.Bind(sess); err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		writeJSON(w, http.StatusOK, map[string]string{"status": "recorded"})

	case http.MethodDelete:
		userID := r.URL.Query().Get("user")
		deviceID := r.URL.Query().Get("device")
		connID := r.URL.Query().Get("conn")
		if userID == "" || deviceID == "" {
			writeError(w, http.StatusBadRequest, "user and device are required")
			return
		}
		removed := s.presenceReg.Unbind(userID, deviceID, connID)
		writeJSON(w, http.StatusOK, map[string]bool{"removed": removed})

	default:
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
	}
}
