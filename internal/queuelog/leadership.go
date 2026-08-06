package queuelog

import (
	"errors"
	"fmt"
	"sync/atomic"
)

// Delivery state is the one thing a queue adds to the log, and it is the one
// thing the log's own replication does not cover.
//
// A partition's lease window — who holds which record, for how long, how many
// times it has been delivered — lives in memory on the node serving it. Two
// nodes that both hold a replica and both serve consumers would each keep their
// own window over the same records, and hand the same message to two different
// workers. Neither would learn of the other's ack.
//
// So leasing is a leader-only operation, for exactly the same reason writing is:
// it mutates state that must have one owner. A replica may hold the data and
// still have no right to hand it out.
//
// This is also why a queue cannot simply be "the log plus a cursor". A cursor is
// a position, and positions merge harmlessly. A lease is a claim, and claims do
// not.

// ErrNotQueueLeader means this node holds the partition but may not lease from
// it. Callers should route the consumer to the node that leads it.
var ErrNotQueueLeader = errors.New("queuelog: not the leader of this partition")

// LeadershipSource answers which partitions this node may serve.
//
// It is an interface so the queue does not depend on the control plane
// directly: a single-node deployment passes nil and every partition is
// serviceable, which is correct there because there is nobody to conflict with.
type LeadershipSource interface {
	// LeadsPartition reports whether this node currently leads topic/partition.
	LeadsPartition(topic string, partition int32) bool
}

// leadershipGate tracks, per partition, whether this node may lease.
//
// Held as an atomic per partition rather than consulted on every acquire: the
// answer changes only when the controller moves leadership, while acquire runs
// on every consumer poll. Reading an atomic bool is the difference between a
// lock-free hot path and one that contends with reconciliation.
type leadershipGate struct {
	// enforced is false on a single node, where the gate would reject every
	// legitimate lease.
	enforced bool
	allowed  []atomic.Bool
}

func newLeadershipGate(partitions int, enforced bool) *leadershipGate {
	g := &leadershipGate{enforced: enforced, allowed: make([]atomic.Bool, partitions)}
	if !enforced {
		for i := range g.allowed {
			g.allowed[i].Store(true)
		}
	}
	return g
}

// mayLease reports whether partition may be leased from.
func (g *leadershipGate) mayLease(partition int32) bool {
	if g == nil || !g.enforced {
		return true
	}
	if partition < 0 || int(partition) >= len(g.allowed) {
		return false
	}
	return g.allowed[partition].Load()
}

// set records whether this node leads a partition.
func (g *leadershipGate) set(partition int32, leads bool) bool {
	if g == nil || partition < 0 || int(partition) >= len(g.allowed) {
		return false
	}
	return g.allowed[partition].Swap(leads) != leads
}

// EnforceLeadership makes this queue lease only from partitions the source says
// it leads, and refuse the rest.
//
// Call it once, at construction, when a control plane exists. It is not a
// runtime toggle: turning it on with leases outstanding would leave records held
// by a node that has just lost the right to hold them, and turning it off
// reopens the double-delivery window it closes.
func (q *Queue) EnforceLeadership(src LeadershipSource) {
	q.leadership = src
	q.gate = newLeadershipGate(len(q.parts), src != nil)
	q.RefreshLeadership()
}

// RefreshLeadership re-reads leadership and releases what this node may no
// longer serve.
//
// Releasing matters more than acquiring. A partition this node has stopped
// leading may already be served by its new leader, so every lease still held
// here is a record two workers could be processing. Dropping them immediately
// makes the overlap as short as the reconcile interval instead of as long as the
// ack timeout.
func (q *Queue) RefreshLeadership() {
	if q.gate == nil || q.leadership == nil {
		return
	}
	for i, sp := range q.parts {
		pid := int32(i)
		leads := q.leadership.LeadsPartition(q.name, pid)
		if !q.gate.set(pid, leads) {
			continue
		}
		if !leads {
			// releaseAll returns every in-flight record to the available pool
			// locally; the new leader will hand them out from its own view.
			sp.releaseAll()
		}
	}
}

// LeadsPartition reports whether this node may currently serve a partition.
func (q *Queue) LeadsPartition(partition int32) bool {
	return q.gate.mayLease(partition)
}

// checkLease is the guard acquire consults.
func (q *Queue) checkLease(partition int32) error {
	if q.gate.mayLease(partition) {
		return nil
	}
	return fmt.Errorf("%w: %s/%d", ErrNotQueueLeader, q.name, partition)
}
