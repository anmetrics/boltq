package cluster

import (
	"log"
	"time"
)

// Moving a replica is the one control-plane operation that touches real data,
// and the only one that can lose it if sequenced wrongly.
//
// The sequence is always expand, catch up, shrink:
//
//	1. add the new broker to the replica set — the partition is now
//	   over-replicated, which costs disk and nothing else
//	2. wait for it to join the ISR under its own steam, by fetching from the
//	   leader like any other follower
//	3. only then remove the old broker
//
// The reverse order — remove then add — is what a naive rebalancer does, and it
// spends the whole copy window one failure away from data loss. Doing it this
// way means a move that stalls forever leaves the cluster over-replicated and
// perfectly safe, which is the right way to fail.
//
// Leadership is handled explicitly at step 3: if the broker being removed is the
// partition leader, leadership moves to a surviving in-sync replica *before* the
// removal, never as a side effect of it.

type movePhase int

const (
	// phaseExpand: the new replica has been added to the set and is copying.
	phaseExpand movePhase = iota
	// phaseShrink: the new replica is in sync; the old one can go.
	phaseShrink
)

type moveState struct {
	move    Move
	phase   movePhase
	started time.Time
}

// planRebalance computes and starts replica moves, respecting the concurrency
// cap. It does nothing unless rebalancing is enabled.
func (c *Controller) planRebalance(now time.Time) {
	if !c.cfg.Rebalance || c.planner == nil {
		return
	}

	c.mu.Lock()
	inFlight := len(c.moves)
	c.mu.Unlock()
	if inFlight >= c.cfg.MaxConcurrentMoves {
		return
	}

	assignments := c.meta.Assignments()
	brokers := c.meta.Brokers()

	// A rebalance computed while any partition is still catching up would count
	// the half-copied replica as settled and plan the next move on a false
	// picture of the load. Wait for the cluster to be quiet.
	for _, a := range assignments {
		if len(a.ISR) < len(a.Replicas) {
			return
		}
	}

	for _, mv := range c.planner.Rebalance(assignments, brokers) {
		c.mu.Lock()
		_, busy := c.moves[PartitionKey(mv.Topic, mv.Partition)]
		room := len(c.moves) < c.cfg.MaxConcurrentMoves
		c.mu.Unlock()
		if busy || !room {
			continue
		}
		c.startMove(mv, now)
	}
}

// startMove adds the destination replica, leaving the source in place.
func (c *Controller) startMove(mv Move, now time.Time) {
	a, ok := c.meta.Assignment(mv.Topic, mv.Partition)
	if !ok {
		return
	}
	if containsNode(a.Replicas, mv.To) {
		return
	}

	expanded := append(append([]string(nil), a.Replicas...), mv.To)
	if !c.reassign(mv.Topic, mv.Partition, expanded) {
		return
	}

	c.mu.Lock()
	c.moves[PartitionKey(mv.Topic, mv.Partition)] = &moveState{
		move: mv, phase: phaseExpand, started: now,
	}
	c.mu.Unlock()
	log.Printf("[controller] move %s: added %s, waiting for it to catch up", mv, mv.To)
}

// advanceMoves drives every in-flight move one step.
func (c *Controller) advanceMoves(now time.Time) {
	c.mu.Lock()
	pending := make([]*moveState, 0, len(c.moves))
	for _, st := range c.moves {
		pending = append(pending, st)
	}
	c.mu.Unlock()

	for _, st := range pending {
		key := PartitionKey(st.move.Topic, st.move.Partition)

		a, ok := c.meta.Assignment(st.move.Topic, st.move.Partition)
		if !ok {
			c.finishMove(key)
			continue
		}

		switch st.phase {
		case phaseExpand:
			if !a.InISR(st.move.To) {
				if now.Sub(st.started) > c.cfg.MoveTimeout {
					// Abandon rather than force it. The destination is still a
					// replica and still fetching; leaving it in place costs
					// disk, while removing the source to "finish" the move
					// would drop a copy that is demonstrably healthier.
					log.Printf("[controller] move %s: %s has not caught up in %s — abandoning, partition stays over-replicated",
						st.move, st.move.To, c.cfg.MoveTimeout)
					c.finishMove(key)
				}
				continue
			}
			c.mu.Lock()
			st.phase = phaseShrink
			c.mu.Unlock()
			log.Printf("[controller] move %s: %s is in sync, removing %s", st.move, st.move.To, st.move.From)
			fallthrough

		case phaseShrink:
			// Leadership first. Removing the leader from the replica set and
			// letting the next sweep notice would leave the partition briefly
			// led by a node that has been told to drop its data.
			if a.Leader == st.move.From {
				next := ""
				for _, id := range a.ISR {
					if id != st.move.From {
						next = id
						break
					}
				}
				if next == "" {
					// Nothing else is in sync. Waiting is correct: the move has
					// nowhere safe to hand leadership.
					continue
				}
				if !c.assignLeader(a, next, "vacating a departing replica") {
					continue
				}
				a, _ = c.meta.Assignment(st.move.Topic, st.move.Partition)
			}

			shrunk := make([]string, 0, len(a.Replicas)-1)
			for _, id := range a.Replicas {
				if id != st.move.From {
					shrunk = append(shrunk, id)
				}
			}
			if c.reassign(st.move.Topic, st.move.Partition, shrunk) {
				log.Printf("[controller] move %s: complete", st.move)
			}
			c.finishMove(key)
		}
	}
}

func (c *Controller) finishMove(key string) {
	c.mu.Lock()
	delete(c.moves, key)
	c.mu.Unlock()
}

// reassign applies a replica-set change, reporting whether it took effect.
func (c *Controller) reassign(topic string, partition int32, replicas []string) bool {
	resp, err := c.raft.Apply(&RaftCommand{
		Type: CmdMetaReassign,
		Meta: &MetaCommand{Topic: topic, Partition: partition, Replicas: replicas},
	}, c.cfg.ApplyTimeout)
	if err != nil {
		log.Printf("[controller] reassign %s/%d: %v", topic, partition, err)
		return false
	}
	if resp.Error != nil {
		log.Printf("[controller] reassign %s/%d rejected: %v", topic, partition, resp.Error)
		return false
	}
	return true
}

// assignLeader installs a new leader at the next epoch.
func (c *Controller) assignLeader(a *PartitionAssignment, leader, reason string) bool {
	resp, err := c.raft.Apply(&RaftCommand{
		Type: CmdMetaAssignLeader,
		Meta: &MetaCommand{
			Topic:       a.Topic,
			Partition:   a.Partition,
			Leader:      leader,
			LeaderEpoch: a.LeaderEpoch + 1,
		},
	}, c.cfg.ApplyTimeout)
	if err != nil || resp.Error != nil {
		return false
	}
	log.Printf("[controller] %s/%d leader %q -> %q at epoch %d (%s)",
		a.Topic, a.Partition, a.Leader, leader, a.LeaderEpoch+1, reason)
	return true
}

// CreateTopic places a new topic's replicas and records it.
//
// Placement happens here, on the controller, rather than wherever the request
// arrived: it needs the whole live broker list and the whole existing load
// picture, and two nodes placing topics concurrently from partial views would
// pile replicas onto the same brokers.
func (c *Controller) CreateTopic(name string, partitions int32, replicationFactor int) error {
	if c.planner == nil {
		return ErrNoEligibleLeader
	}
	p := NewPlanner(PlannerConfig{
		ReplicationFactor:  replicationFactor,
		MaxConcurrentMoves: c.cfg.MaxConcurrentMoves,
	})

	placements, err := p.Place(partitions, c.meta.Brokers())
	if err != nil {
		return err
	}

	resp, err := c.raft.Apply(&RaftCommand{
		Type: CmdMetaCreateTopic,
		Meta: &MetaCommand{
			TopicMeta: &TopicMeta{
				Name:              name,
				Partitions:        partitions,
				ReplicationFactor: int32(replicationFactor),
			},
			Placements: placements,
		},
	}, c.cfg.ApplyTimeout)
	if err != nil {
		return err
	}
	if resp.Error != nil {
		return resp.Error
	}
	log.Printf("[controller] created topic %s (%d partitions, rf=%d)", name, partitions, replicationFactor)
	return nil
}
