package cluster

import (
	"log"
	"sync"
	"time"
)

// The controller is the cluster's decision-maker for the stream plane: it
// decides which node leads each partition and publishes that decision through
// Raft.
//
// Exactly one controller is active at a time, and no election is needed to
// arrange that — the Raft leader is the controller. Every decision it makes is
// a Raft command, so a controller that dies mid-decision leaves either a
// committed decision or none at all, and its successor rebuilds its entire
// working set by reading the metadata store it already replicates.
//
// The one piece of state the controller keeps outside Raft is liveness: the
// last time each broker was heard from. Heartbeats are frequent and
// uninteresting, and putting them through consensus would mean a quorum fsync
// every few seconds per broker for information that is worthless the moment it
// is written. Only the *conclusion* — this broker is dead, fence it — is
// replicated. That is the same trade KRaft makes, and the cost of it is that a
// controller failover starts with an empty liveness table, which is why
// newly-observed brokers get a grace period instead of being fenced on sight.

// ControllerConfig tunes failure detection and leadership placement.
type ControllerConfig struct {
	// SessionTimeout is how long a broker may go unheard-from before it is
	// fenced and loses its leaderships. Too low and a GC pause triggers a
	// needless failover; too high and writes to a dead node's partitions stall
	// for that long. Default 15s.
	SessionTimeout time.Duration

	// SweepInterval is how often liveness is evaluated. Default 3s.
	SweepInterval time.Duration

	// StartupGrace is how long after this node becomes controller before it
	// will fence anyone. A fresh controller has heard from nobody yet; without
	// this it would fence the entire cluster on its first sweep. Default is
	// twice SessionTimeout.
	StartupGrace time.Duration

	// PreferredLeaderRebalance drifts leadership back to Replicas[0] once it is
	// healthy again. Without it, leadership accumulates wherever failovers
	// happened to land, and one node ends up leading everything. Default true.
	PreferredLeaderRebalance bool

	// AllowUncleanElection permits electing a replica that is not in the ISR
	// when the ISR is empty. It trades data loss for availability: the elected
	// replica is missing records the dead leader had acknowledged, and those
	// records are gone. Default false, and it should stay false unless the data
	// is genuinely worth less than the downtime.
	AllowUncleanElection bool

	// ApplyTimeout bounds each Raft round trip. Default 5s.
	ApplyTimeout time.Duration

	// Rebalance enables replica movement toward even load. Off by default:
	// moving replicas copies partitions across the network, and a cluster
	// should not start doing that because someone enabled clustering.
	Rebalance bool

	// ReplicationFactor is the target number of copies, used when placing new
	// topics. Default 3.
	ReplicationFactor int

	// MaxConcurrentMoves bounds partitions in flight. Default 4.
	MaxConcurrentMoves int

	// MoveTimeout is how long a destination replica may take to catch up before
	// the move is abandoned. Abandoning leaves the partition over-replicated,
	// which is safe; forcing it through would not be. Default 30m — a large
	// partition on a busy network genuinely takes that long.
	MoveTimeout time.Duration
}

func (c *ControllerConfig) applyDefaults() {
	if c.SessionTimeout <= 0 {
		c.SessionTimeout = 15 * time.Second
	}
	if c.SweepInterval <= 0 {
		c.SweepInterval = 3 * time.Second
	}
	if c.StartupGrace <= 0 {
		c.StartupGrace = 2 * c.SessionTimeout
	}
	if c.ApplyTimeout <= 0 {
		c.ApplyTimeout = 5 * time.Second
	}
	if c.ReplicationFactor <= 0 {
		c.ReplicationFactor = 3
	}
	if c.MaxConcurrentMoves <= 0 {
		c.MaxConcurrentMoves = 4
	}
	if c.MoveTimeout <= 0 {
		c.MoveTimeout = 30 * time.Minute
	}
}

// applier is the subset of a consensus group the controller needs. It is an
// interface so the election logic can be tested without standing up a Raft
// cluster — the decisions are the part worth testing, and they do not depend on
// consensus.
type applier interface {
	Apply(cmd *RaftCommand, timeout time.Duration) (*ApplyResponse, error)
	IsLeader() bool
	NodeID() string
}

// ControlNode is the control-plane consensus group as its users see it.
//
// Both MetadataNode and the legacy combined RaftNode satisfy it, which is what
// lets a deployment migrate from one group to two without every caller changing
// shape at the same moment.
type ControlNode interface {
	applier
	Metadata() *MetadataStore
	LeaderID() string
	Status() ClusterStatus
	Join(nodeID, addr string) error
	JoinNonVoter(nodeID, addr string) error
	Leave(nodeID string) error
}

// Controller reconciles broker liveness into partition leadership.
type Controller struct {
	cfg     ControllerConfig
	raft    applier
	meta    *MetadataStore
	planner *Planner
	clock   func() time.Time

	mu sync.Mutex
	// lastSeen is the in-memory liveness table described above.
	lastSeen map[string]time.Time
	// leaderSince marks when this node became controller, for StartupGrace.
	leaderSince time.Time
	wasLeader   bool
	// moves are the replica relocations currently in flight, keyed by
	// partition. Held in memory, not in Raft: a controller failover abandons
	// them, and abandoning a move is always safe because it leaves the
	// partition over-replicated. The successor recomputes what it wants.
	moves map[string]*moveState

	stop chan struct{}
	done chan struct{}
}

// NewController creates a controller. It does nothing until Start is called,
// and does nothing on a node that is not the Raft leader.
func NewController(node ControlNode, cfg ControllerConfig) *Controller {
	return newController(node, node.Metadata(), cfg)
}

func newController(a applier, meta *MetadataStore, cfg ControllerConfig) *Controller {
	cfg.applyDefaults()
	return &Controller{
		cfg:  cfg,
		raft: a,
		meta: meta,
		planner: NewPlanner(PlannerConfig{
			ReplicationFactor:  cfg.ReplicationFactor,
			MaxConcurrentMoves: cfg.MaxConcurrentMoves,
		}),
		clock:    time.Now,
		lastSeen: make(map[string]time.Time),
		moves:    make(map[string]*moveState),
		stop:     make(chan struct{}),
		done:     make(chan struct{}),
	}
}

// Heartbeat records that a broker is alive.
//
// It is called on whichever node receives the heartbeat; on a non-controller
// that is harmless bookkeeping, and it means a controller failover to that node
// starts with a warm table instead of a blank one.
func (c *Controller) Heartbeat(nodeID string) {
	if nodeID == "" {
		return
	}
	c.mu.Lock()
	c.lastSeen[nodeID] = c.clock()
	c.mu.Unlock()
}

// Start begins the reconcile loop.
func (c *Controller) Start() {
	go c.run()
}

// Close stops the loop and waits for it to exit.
func (c *Controller) Close() {
	select {
	case <-c.stop:
		return
	default:
	}
	close(c.stop)
	<-c.done
}

func (c *Controller) run() {
	defer close(c.done)
	ticker := time.NewTicker(c.cfg.SweepInterval)
	defer ticker.Stop()

	for {
		select {
		case <-c.stop:
			return
		case <-ticker.C:
			c.Reconcile()
		}
	}
}

// Reconcile runs one pass: fence brokers that went quiet, then repair every
// partition whose leader is no longer viable. Exported for tests and for
// callers that want to force a pass after a known event.
func (c *Controller) Reconcile() {
	if !c.raft.IsLeader() {
		c.mu.Lock()
		c.wasLeader = false
		// Drop in-flight moves on losing leadership. They are this
		// controller's intentions, not cluster state, and the node that takes
		// over will form its own from the metadata everyone already shares.
		c.moves = make(map[string]*moveState)
		c.mu.Unlock()
		return
	}

	now := c.clock()

	// The controller vouches for itself. If this loop is running, this process
	// is alive — there is no failure mode where the node is dead but still
	// sweeping. Without this the controller never observes a heartbeat from
	// itself, fences itself after one session timeout, and drops every
	// partition it leads for no reason at all.
	c.Heartbeat(c.raft.NodeID())

	c.mu.Lock()
	if !c.wasLeader {
		c.wasLeader = true
		c.leaderSince = now
		log.Printf("[controller] %s is now the controller", c.raft.NodeID())
	}
	inGrace := now.Sub(c.leaderSince) < c.cfg.StartupGrace
	c.mu.Unlock()

	c.sweepLiveness(now, inGrace)
	c.repairLeadership()

	// Rebalancing comes last, and only once leadership is settled. Planning a
	// move against a partition whose leader is still being repaired would base
	// an expensive decision on a picture that is about to change.
	c.advanceMoves(now)
	c.planRebalance(now)
}

// sweepLiveness fences brokers whose sessions expired and unfences those that
// came back.
func (c *Controller) sweepLiveness(now time.Time, inGrace bool) {
	for _, b := range c.meta.Brokers() {
		c.mu.Lock()
		seen, known := c.lastSeen[b.NodeID]
		c.mu.Unlock()

		alive := known && now.Sub(seen) < c.cfg.SessionTimeout

		switch {
		case alive && b.Fenced:
			c.fence(b.NodeID, false, now)
		case !alive && !b.Fenced:
			// During the startup grace period an unknown broker is presumed
			// alive: we have simply not been listening long enough to know
			// otherwise, and fencing on ignorance would fail over the whole
			// cluster every time the controller moves.
			if inGrace && !known {
				continue
			}
			c.fence(b.NodeID, true, now)
		}
	}
}

func (c *Controller) fence(nodeID string, fenced bool, now time.Time) {
	resp, err := c.raft.Apply(&RaftCommand{
		Type: CmdMetaFenceBroker,
		Meta: &MetaCommand{NodeID: nodeID, Fenced: fenced, Timestamp: now.UnixNano()},
	}, c.cfg.ApplyTimeout)
	if err != nil {
		log.Printf("[controller] fence %s=%v: %v", nodeID, fenced, err)
		return
	}
	if resp.Error != nil {
		log.Printf("[controller] fence %s=%v rejected: %v", nodeID, fenced, resp.Error)
		return
	}
	if resp.Changed {
		if fenced {
			log.Printf("[controller] fenced %s — session expired", nodeID)
		} else {
			log.Printf("[controller] unfenced %s — heartbeats resumed", nodeID)
		}
	}
}

// repairLeadership elects a new leader for every partition that needs one.
func (c *Controller) repairLeadership() {
	live := make(map[string]bool)
	for _, b := range c.meta.LiveBrokers() {
		live[b.NodeID] = true
	}

	for _, a := range c.meta.Assignments() {
		next, reason := c.electLeader(a, live)
		if next == a.Leader {
			continue
		}
		if next == "" {
			// Recording the absence matters: with no leader, writes fail fast
			// with "partition offline" instead of hanging on a node that is
			// never going to accept them.
			log.Printf("[controller] %s/%d has no eligible leader (%s) — marking offline",
				a.Topic, a.Partition, reason)
		}

		resp, err := c.raft.Apply(&RaftCommand{
			Type: CmdMetaAssignLeader,
			Meta: &MetaCommand{
				Topic:       a.Topic,
				Partition:   a.Partition,
				Leader:      next,
				LeaderEpoch: a.LeaderEpoch + 1,
			},
		}, c.cfg.ApplyTimeout)
		if err != nil {
			log.Printf("[controller] assign %s/%d: %v", a.Topic, a.Partition, err)
			continue
		}
		if resp.Error != nil {
			// Usually a stale epoch, meaning another controller already made
			// this decision. Harmless; the next sweep sees the new state.
			log.Printf("[controller] assign %s/%d rejected: %v", a.Topic, a.Partition, resp.Error)
			continue
		}
		if next != "" {
			log.Printf("[controller] %s/%d leader %q -> %q at epoch %d (%s)",
				a.Topic, a.Partition, a.Leader, next, a.LeaderEpoch+1, reason)
		}
	}
}

// electLeader picks the node that should lead a partition, and why.
//
// Order of preference:
//
//  1. Keep the current leader if it is live and in-sync. Leadership changes
//     cost a truncation round on every follower; not moving is the cheapest
//     correct answer.
//  2. The preferred replica (Replicas[0]) when it is live and in-sync and
//     rebalancing is on — this is what pulls the cluster back toward even
//     placement after failovers.
//  3. The first live, in-sync replica in placement order.
//  4. Nothing — the partition goes offline — unless unclean election is
//     explicitly allowed, in which case any live replica wins and the records
//     it is missing are lost.
func (c *Controller) electLeader(a *PartitionAssignment, live map[string]bool) (string, string) {
	eligible := func(id string) bool { return live[id] && a.InISR(id) }

	if a.Leader != "" && eligible(a.Leader) {
		if c.cfg.PreferredLeaderRebalance && len(a.Replicas) > 0 {
			pref := a.Replicas[0]
			if pref != a.Leader && eligible(pref) {
				return pref, "preferred leader healthy"
			}
		}
		return a.Leader, "unchanged"
	}

	if c.cfg.PreferredLeaderRebalance && len(a.Replicas) > 0 && eligible(a.Replicas[0]) {
		return a.Replicas[0], "preferred leader"
	}
	for _, id := range a.Replicas {
		if eligible(id) {
			return id, "first in-sync replica"
		}
	}

	if c.cfg.AllowUncleanElection {
		for _, id := range a.Replicas {
			if live[id] {
				return id, "UNCLEAN election — acknowledged records may be lost"
			}
		}
	}
	return "", "no in-sync replica is live"
}
