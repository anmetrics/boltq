package api

import (
	"encoding/json"
	"errors"
	"net/http"

	"github.com/boltq/boltq/internal/stream"
	"github.com/boltq/boltq/internal/streamctl"
)

// The internal append endpoint receives writes routed from a node that holds a
// partition but does not lead it.
//
// It is on the admin listener, which the manifests keep off any internet-facing
// Service and behind a NetworkPolicy. Anyone who can reach it can write records
// into any partition this node leads, bypassing every authorisation check the
// gateway performs — the peer that forwarded the write already did those, and
// repeating them here would need the caller's identity, which is not what this
// endpoint carries. That trade is the reason it must never be exposed.

// SetStreamLog attaches the local stream log so forwarded writes can be
// applied. Nil disables the endpoint.
func (s *HTTPServer) SetStreamLog(l *stream.Log) {
	s.streamLog = l
}

// handleInternalAppend applies a write forwarded by a peer.
func (s *HTTPServer) handleInternalAppend(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	if s.streamLog == nil {
		writeError(w, http.StatusServiceUnavailable, "streaming is not enabled on this node")
		return
	}

	var req streamctl.ForwardRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid json")
		return
	}
	defer r.Body.Close()
	if req.Topic == "" {
		writeError(w, http.StatusBadRequest, "topic is required")
		return
	}

	rec := &stream.Record{
		Key:     req.Key,
		Payload: req.Payload,
		Headers: req.Headers,
		Flags:   req.Flags,
	}

	// AppendContext, not Append: if this node is configured for quorum
	// acknowledgement, the forwarding peer is waiting on this call and must not
	// be told the write succeeded before the replicas hold it.
	res, err := s.streamLog.AppendContext(r.Context(), req.Topic, rec)
	if err != nil {
		if errors.Is(err, stream.ErrNotPartitionLeader) {
			// Leadership moved again between the peer reading metadata and this
			// request arriving. 421 tells the caller to re-resolve rather than
			// retry blindly, and stops the two nodes bouncing the write between
			// them.
			writeJSON(w, http.StatusMisdirectedRequest, map[string]string{
				"error": "not the partition leader",
				"topic": req.Topic,
			})
			return
		}
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}

	writeJSON(w, http.StatusOK, streamctl.ForwardResponse{
		Topic:     res.Topic,
		Partition: res.Partition,
		Seq:       res.Seq,
		Timestamp: res.Timestamp,
	})
}
