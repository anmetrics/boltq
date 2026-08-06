package streamctl

import (
	"github.com/boltq/boltq/internal/cluster"
	"github.com/boltq/boltq/internal/queuelog"
)

// A log-backed queue inherits almost everything from the stream layer:
// partitioning, replication, durability, write routing. The one thing it adds —
// per-record delivery state — is also the one thing the log's replication does
// not cover, because a lease is a claim rather than a position, and claims do
// not merge.
//
// So the queue needs exactly one fact from the control plane: which partitions
// this node may lease from. That is the same question the stream layer already
// asks about writes, answered from the same metadata.

// QueueLeadership answers a queue's leadership question from cluster metadata.
type QueueLeadership struct {
	meta   *cluster.MetadataStore
	nodeID string
}

// NewQueueLeadership creates the adapter.
func NewQueueLeadership(meta *cluster.MetadataStore, nodeID string) *QueueLeadership {
	return &QueueLeadership{meta: meta, nodeID: nodeID}
}

// LeadsPartition implements queuelog.LeadershipSource.
//
// A partition with no assignment is not leased from. During a failover that
// briefly stalls consumers on that partition, which is correct: the alternative
// is two nodes handing out the same records while the cluster works out which
// of them is in charge.
func (q *QueueLeadership) LeadsPartition(topic string, partition int32) bool {
	if q == nil || q.meta == nil {
		return true
	}
	a, ok := q.meta.Assignment(topic, partition)
	if !ok {
		return false
	}
	return a.Leader == q.nodeID
}

// QueueSupervisor keeps a set of queues aligned with partition leadership.
//
// It watches the same metadata event stream the stream reconciler uses, so a
// leadership change reaches the queue's lease gate at the same time it reaches
// the log's write guard. Two watchers with two update paths would let a
// partition be writable and un-leasable — or worse, the reverse.
type QueueSupervisor struct {
	meta   *cluster.MetadataStore
	nodeID string

	queues []*queuelog.Queue
	stop   chan struct{}
	done   chan struct{}
}

// NewQueueSupervisor creates a supervisor over the given queues.
func NewQueueSupervisor(meta *cluster.MetadataStore, nodeID string, queues ...*queuelog.Queue) *QueueSupervisor {
	s := &QueueSupervisor{
		meta:   meta,
		nodeID: nodeID,
		queues: queues,
		stop:   make(chan struct{}),
		done:   make(chan struct{}),
	}
	src := NewQueueLeadership(meta, nodeID)
	for _, q := range queues {
		q.EnforceLeadership(src)
	}
	return s
}

// Start begins reacting to leadership changes.
func (s *QueueSupervisor) Start() {
	events, cancel := s.meta.Subscribe(32)
	go func() {
		defer close(s.done)
		defer cancel()
		for {
			select {
			case <-s.stop:
				return
			case <-events:
				// Events are snapshots, not deltas, so draining and refreshing
				// once is equivalent to handling each in turn — and far cheaper
				// during a rebalance that moves many partitions at once.
				drain(events)
				s.Refresh()
			}
		}
	}()
}

// Refresh re-reads leadership for every supervised queue.
func (s *QueueSupervisor) Refresh() {
	for _, q := range s.queues {
		q.RefreshLeadership()
	}
}

// Close stops the supervisor.
func (s *QueueSupervisor) Close() {
	select {
	case <-s.stop:
		return
	default:
	}
	close(s.stop)
	<-s.done
}

func drain(events <-chan cluster.MetadataEvent) {
	for {
		select {
		case <-events:
		default:
			return
		}
	}
}
