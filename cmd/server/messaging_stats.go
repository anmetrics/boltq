package main

import (
	"github.com/boltq/boltq/internal/stream"
)

// messagingStatsAdapter exposes the messaging stack to the admin API without
// making internal/api depend on the streaming packages.
type messagingStatsAdapter struct {
	st *messagingStack
}

// StreamStats returns per-topic log statistics.
func (a *messagingStatsAdapter) StreamStats() any {
	if a.st == nil || a.st.Log == nil {
		return nil
	}
	return map[string]any{
		"topics": a.st.Log.Stats(),
		"cursors": map[string]any{
			"tracked": a.st.Cursors.Count(),
		},
	}
}

// PresenceStats returns connection counts by node, region and state.
func (a *messagingStatsAdapter) PresenceStats() any {
	if a.st == nil || a.st.Presence == nil {
		return nil
	}
	return a.st.Presence.Stats()
}

// GatewayStats returns WebSocket edge counters.
func (a *messagingStatsAdapter) GatewayStats() any {
	if a.st == nil || a.st.Gateway == nil {
		return nil
	}
	return a.st.Gateway.Stats()
}

// SignalStats returns ephemeral signal counters.
func (a *messagingStatsAdapter) SignalStats() any {
	if a.st == nil || a.st.Signals == nil {
		return nil
	}
	return a.st.Signals.Stats()
}

// PushStats returns push dispatcher counters.
func (a *messagingStatsAdapter) PushStats() any {
	if a.st == nil || a.st.Dispatch == nil {
		return nil
	}
	return a.st.Dispatch.Stats()
}

// ReplicationStats reports this node's replication role and health.
//
// This is the messaging plane's own health signal, and it is deliberately
// separate from /cluster/status: the Raft cluster and stream replication are
// configured independently, so a healthy Raft quorum says nothing about whether
// chat is being replicated — or whether this node still leads anything.
func (a *messagingStatsAdapter) ReplicationStats() any {
	if a.st == nil {
		return nil
	}

	switch {
	case a.st.RepLeader != nil:
		s := a.st.RepLeader.Stats()
		out := map[string]any{
			"role":                "leader",
			"followers_connected": s.FollowersConnected,
			"followers":           a.st.RepLeader.Followers(),
			"min_in_sync":         s.MinInSync,
			"records_shipped":     s.RecordsShipped,
			"bytes_shipped":       s.BytesShipped,
			"acks_received":       s.AcksReceived,
			"ack_timeouts":        s.AckTimeouts,
			"auth_failures":       s.AuthFailures,
		}
		// The single most important derived signal: whether writes can still
		// reach the configured durability. Below this, every send fails.
		needed := s.MinInSync - 1
		out["quorum_available"] = s.FollowersConnected >= needed
		out["followers_needed"] = needed

		// Per-partition replica lag, for the topics that have any.
		var partitions []stream.PartitionReplication
		if a.st.Log != nil {
			for _, ts := range a.st.Log.Stats() {
				for _, ps := range ts.Partitions {
					r := a.st.RepLeader.Replication(ts.Name, ps.ID)
					if len(r.Replicas) > 0 {
						partitions = append(partitions, r)
					}
				}
			}
		}
		out["partitions"] = partitions
		return out

	case a.st.RepFollow != nil:
		s := a.st.RepFollow.Stats()
		return map[string]any{
			"role":            "follower",
			"connected":       s.Connected,
			"leader_node_id":  s.LeaderNodeID,
			"records_applied": s.RecordsApplied,
			"batches_applied": s.BatchesApplied,
			"gaps":            s.Gaps,
			"reconnects":      s.Reconnects,
			"errors":          s.Errors,
			"last_applied_at": s.LastAppliedAt,
			"partition_heads": s.PartitionHeads,
			"assignments":     len(a.st.RepFollow.Assignments()),
		}

	default:
		// Replication is off. Say so explicitly rather than returning null,
		// which a dashboard would render identically to "not reporting".
		return map[string]any{
			"role":    "standalone",
			"warning": "stream log is not replicated; losing this node's disk loses its messages",
		}
	}
}

// TopicDetail returns per-partition detail for one topic.
func (a *messagingStatsAdapter) TopicDetail(name string) any {
	if a.st == nil || a.st.Log == nil {
		return nil
	}
	t, err := a.st.Log.Topic(name)
	if err != nil {
		return nil
	}

	parts := make([]stream.PartitionStats, 0, t.PartitionCount())
	var total int64
	for _, p := range t.Partitions() {
		first, next, bytes := p.FirstSeq(), p.NextSeq(), p.Bytes()
		parts = append(parts, stream.PartitionStats{
			ID: p.ID, FirstSeq: first, NextSeq: next,
			Records: next - first, Bytes: bytes,
		})
		total += bytes
	}
	return stream.TopicStats{Name: name, Partitions: parts, TotalBytes: total}
}

// CursorsFor returns the committed positions for a topic partition.
//
// When no group is named it reports the push dispatcher's position, which is
// the one an operator usually wants: it answers "are notifications keeping up
// with the log?" — the question behind most delivery complaints.
func (a *messagingStatsAdapter) CursorsFor(topic string, partition int32, group string) any {
	if a.st == nil || a.st.Cursors == nil {
		return nil
	}
	if group == "" {
		group = "push-dispatcher"
	}

	members := a.st.Cursors.GroupMembers(topic, partition, group)
	out := map[string]any{
		"topic":     topic,
		"partition": partition,
		"group":     group,
		"members":   members,
	}
	if slowest, ok := a.st.Cursors.SlowestInGroup(topic, partition, group); ok {
		out["watermark"] = slowest
	}
	if t, err := a.st.Log.Topic(topic); err == nil {
		if p, err := t.Partition(partition); err == nil {
			out["next_seq"] = p.NextSeq()
			out["first_seq"] = p.FirstSeq()
			if slowest, ok := a.st.Cursors.SlowestInGroup(topic, partition, group); ok {
				out["lag"] = p.NextSeq() - slowest
			}
		}
	}
	return out
}
