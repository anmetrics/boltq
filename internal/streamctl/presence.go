package streamctl

import (
	"github.com/boltq/boltq/internal/cluster"
	"github.com/boltq/boltq/internal/presence"
)

// Presence shards are partitions of a reserved topic, and that is the whole
// trick.
//
// A presence directory needs to answer "which node owns this user" with
// something every node agrees on, that survives a node dying, and that moves
// when the cluster is rebalanced. Building a second membership mechanism to
// provide that would give the cluster two answers to the same question — and
// the interesting failures are always the ones where the two disagree.
//
// Partition assignments already have every property needed. So presence shard N
// is whoever leads _presence/N, and presence failover is partition failover.

// PresenceResolver maps users to the node owning their presence shard.
type PresenceResolver struct {
	meta   *cluster.MetadataStore
	nodeID string
}

// NewPresenceResolver creates a resolver over cluster metadata.
func NewPresenceResolver(meta *cluster.MetadataStore, nodeID string) *PresenceResolver {
	return &PresenceResolver{meta: meta, nodeID: nodeID}
}

// OwnerOf implements presence.ShardResolver.
//
// An unowned shard — no leader, because its previous owner was fenced and no
// successor is assigned yet — returns empty rather than guessing. The caller
// decides what to do with "unknown", and for presence the safe answer differs
// by call site: assume reachable when deciding whether to push, refuse when
// routing a delivery.
func (r *PresenceResolver) OwnerOf(userID string) (nodeID, addr string, local bool) {
	if r == nil || r.meta == nil {
		return r.nodeID, "", true
	}

	meta, ok := r.meta.Topic(presence.ShardTopic)
	if !ok || meta.Partitions <= 0 {
		// The presence topic has not been created. Every node answers for
		// itself, which is what a single-node deployment wants and what a
		// cluster gets until an operator creates the topic.
		return r.nodeID, "", true
	}

	shard := presence.ShardForUser(userID, meta.Partitions)
	a, ok := r.meta.Assignment(presence.ShardTopic, shard)
	if !ok || a.Leader == "" {
		return "", "", false
	}
	if a.Leader == r.nodeID {
		return r.nodeID, "", true
	}

	b, ok := r.meta.Broker(a.Leader)
	if !ok {
		return a.Leader, "", false
	}
	return a.Leader, b.AdminAddr, false
}
