// Package streamctl turns replicated cluster metadata into local action.
//
// The control plane in internal/cluster decides *what* should be true — which
// node leads each partition, under which epoch. Nothing there touches a log
// file. This package is the other half: on every node, it watches the metadata
// it already replicates and makes the local stream log match, promoting
// partitions this node now leads and fetching the ones it does not.
//
// Keeping the two apart is deliberate. The controller must be able to decide
// leadership for a partition whose data lives on three other machines, and the
// FSM that applies its decisions must never block on disk I/O — an FSM that
// waits on a promotion would stall consensus for the whole cluster.
package streamctl

import (
	"log"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/boltq/boltq/internal/cluster"
	"github.com/boltq/boltq/internal/replication"
	"github.com/boltq/boltq/internal/stream"
)

// Applier submits commands to the replicated log. Narrow by design: the
// reconciler proposes exactly one kind of command, and taking the whole
// RaftNode here would make it untestable without a consensus cluster.
type Applier interface {
	Apply(cmd *cluster.RaftCommand, timeout time.Duration) (*cluster.ApplyResponse, error)
}

// Config tunes the node-side reconciler.
type Config struct {
	// NodeID must match the ID this node registered under.
	NodeID string

	// Secret authenticates replication sessions; it must match the leader's.
	Secret string

	// ISRReportInterval is how often the partitions this node leads report
	// their in-sync set back to the controller. Default 5s.
	ISRReportInterval time.Duration

	// SyncOnApply fsyncs replicated records before acknowledging them. It is
	// the difference between "the replica has it" and "the replica's disk has
	// it", and therefore between surviving a process crash and surviving a
	// machine losing power.
	SyncOnApply bool

	// ApplyTimeout bounds each Raft round trip. Default 5s.
	ApplyTimeout time.Duration
}

func (c *Config) applyDefaults() {
	if c.ISRReportInterval <= 0 {
		c.ISRReportInterval = 5 * time.Second
	}
	if c.ApplyTimeout <= 0 {
		c.ApplyTimeout = 5 * time.Second
	}
}

// Reconciler keeps this node's stream log consistent with cluster metadata.
type Reconciler struct {
	cfg  Config
	slog *stream.Log
	meta *cluster.MetadataStore
	raft Applier

	// leader is this node's replication listener. It is shared by every
	// partition this node leads: one listener, many partitions, because a
	// follower opens one session per leader node and multiplexes partitions
	// over it.
	leader *replication.Leader

	mu sync.Mutex
	// followers is one session per *remote leader node*, keyed by its
	// replication address. A node commonly follows partitions led by several
	// different nodes at once, which is exactly what per-partition leadership
	// buys and what a single follower connection cannot express.
	followers map[string]*followerSession
	// promoted remembers the highest epoch this node has locally opened per
	// partition, so a repeated metadata event does not attempt a promotion the
	// log would reject.
	promoted map[string]uint32

	stop chan struct{}
	done chan struct{}
	once sync.Once
}

type followerSession struct {
	follower    *replication.Follower
	assignments []replication.Assignment
}

// New creates a reconciler. Call Start to begin watching metadata.
func New(slog *stream.Log, meta *cluster.MetadataStore, raft Applier, repLeader *replication.Leader, cfg Config) *Reconciler {
	cfg.applyDefaults()
	return &Reconciler{
		cfg:       cfg,
		slog:      slog,
		meta:      meta,
		raft:      raft,
		leader:    repLeader,
		followers: make(map[string]*followerSession),
		promoted:  make(map[string]uint32),
		stop:      make(chan struct{}),
		done:      make(chan struct{}),
	}
}

// Start begins reconciling. It runs until Close.
//
// Enforcement is switched on here, before the first reconcile, and this is the
// single most important line in the package. Until it runs, every partition
// this node hosts accepts local writes regardless of who leads it — which with
// a gateway on every data node means two nodes can assign the same sequence to
// different records. Epoch fencing cannot repair that afterwards, because each
// write carries an epoch that is valid from its own writer's point of view.
func (r *Reconciler) Start() {
	r.slog.EnforceLeadership(true)
	events, cancel := r.meta.Subscribe(64)
	go r.run(events, cancel)
}

// Close stops reconciling and tears down every replication session.
func (r *Reconciler) Close() {
	r.once.Do(func() {
		close(r.stop)
		<-r.done
	})
}

func (r *Reconciler) run(events <-chan cluster.MetadataEvent, cancelSub func()) {
	defer close(r.done)
	defer cancelSub()

	isrTicker := time.NewTicker(r.cfg.ISRReportInterval)
	defer isrTicker.Stop()

	// A periodic tick backs up the event stream. Events are lossy under load by
	// design, and a resync that only ever ran on an event could sit
	// indefinitely on a dropped one.
	resync := time.NewTicker(10 * time.Second)
	defer resync.Stop()

	r.Reconcile()

	for {
		select {
		case <-r.stop:
			r.shutdownFollowers()
			return
		case <-events:
			// Drain any events that piled up: they are snapshots, not deltas,
			// so only the last one matters and Reconcile reads current state
			// anyway.
			r.drain(events)
			r.Reconcile()
		case <-resync.C:
			r.Reconcile()
		case <-isrTicker.C:
			r.reportISR()
		}
	}
}

func (r *Reconciler) drain(events <-chan cluster.MetadataEvent) {
	for {
		select {
		case <-events:
		default:
			return
		}
	}
}

// Reconcile makes one pass over the metadata and applies it locally.
//
// Resignation runs before promotion, deliberately. Both orders converge to the
// same state, but only this one is safe in the window between them: giving up a
// partition first can never overlap with another node's term, while taking one
// on first can.
func (r *Reconciler) Reconcile() {
	r.applyResignations()
	r.applyLeadership()
	r.applyFollowership()
}

// applyResignations stops writing to partitions this node no longer leads.
func (r *Reconciler) applyResignations() {
	led := make(map[string]bool)
	for _, a := range r.meta.LedBy(r.cfg.NodeID) {
		led[cluster.PartitionKey(a.Topic, a.Partition)] = true
	}

	r.mu.Lock()
	var giveUp []cluster.PartitionAssignment
	for key, epoch := range r.promoted {
		if led[key] {
			continue
		}
		topic, partition, ok := parsePartitionKey(key)
		if !ok {
			continue
		}
		giveUp = append(giveUp, cluster.PartitionAssignment{
			Topic: topic, Partition: partition, LeaderEpoch: epoch,
		})
		delete(r.promoted, key)
	}
	r.mu.Unlock()

	for _, a := range giveUp {
		if err := r.slog.ResignFor(a.Topic, a.Partition); err != nil {
			log.Printf("[streamctl] resign %s/%d: %v", a.Topic, a.Partition, err)
			continue
		}
		log.Printf("[streamctl] resigned %s/%d (was epoch %d)", a.Topic, a.Partition, a.LeaderEpoch)
	}
}

// parsePartitionKey splits the "topic/partition" key form.
func parsePartitionKey(key string) (string, int32, bool) {
	i := strings.LastIndex(key, "/")
	if i <= 0 {
		return "", 0, false
	}
	n, err := strconv.ParseInt(key[i+1:], 10, 32)
	if err != nil {
		return "", 0, false
	}
	return key[:i], int32(n), true
}

// applyLeadership opens a local leadership term for each partition the cluster
// says this node leads.
//
// The epoch comes from the assignment, never from a local counter. That is what
// makes the promotion safe: two nodes cannot both believe they lead the same
// partition under the same epoch, because the epoch was handed out by
// consensus.
func (r *Reconciler) applyLeadership() {
	for _, a := range r.meta.LedBy(r.cfg.NodeID) {
		key := cluster.PartitionKey(a.Topic, a.Partition)

		r.mu.Lock()
		already := r.promoted[key] >= a.LeaderEpoch
		r.mu.Unlock()
		if already {
			continue
		}

		// A node can be assigned leadership of a partition whose topic it has
		// never hosted — that is what happens when a replica is placed here for
		// the first time.
		if _, err := r.slog.GetOrCreateTopic(a.Topic); err != nil {
			log.Printf("[streamctl] create topic %s: %v", a.Topic, err)
			continue
		}

		if err := r.slog.BecomeLeaderFor(a.Topic, a.Partition, a.LeaderEpoch); err != nil {
			log.Printf("[streamctl] promote %s/%d to epoch %d: %v",
				a.Topic, a.Partition, a.LeaderEpoch, err)
			continue
		}

		r.mu.Lock()
		r.promoted[key] = a.LeaderEpoch
		r.mu.Unlock()
		log.Printf("[streamctl] leading %s/%d at epoch %d", a.Topic, a.Partition, a.LeaderEpoch)
	}
}

// applyFollowership opens, closes and rebuilds replication sessions so this
// node fetches exactly the partitions it hosts but does not lead.
func (r *Reconciler) applyFollowership() {
	// Group the partitions to fetch by the address of the node leading them.
	want := make(map[string][]replication.Assignment)
	for _, a := range r.meta.ReplicatedBy(r.cfg.NodeID) {
		if a.Leader == "" {
			// Offline partition: there is nobody to fetch from, and picking
			// some other replica would be inventing a leader.
			continue
		}
		lb, ok := r.meta.Broker(a.Leader)
		if !ok || lb.StreamAddr == "" || lb.Fenced {
			continue
		}
		want[lb.StreamAddr] = append(want[lb.StreamAddr], replication.Assignment{
			Topic:          a.Topic,
			Partition:      a.Partition,
			PartitionCount: r.partitionCount(a.Topic),
		})
	}

	r.mu.Lock()
	defer r.mu.Unlock()

	// Drop sessions to leaders we no longer follow.
	for addr, sess := range r.followers {
		if _, keep := want[addr]; !keep {
			log.Printf("[streamctl] stopping replication from %s", addr)
			sess.follower.Close()
			delete(r.followers, addr)
		}
	}

	for addr, assignments := range want {
		if sess, ok := r.followers[addr]; ok && sameAssignments(sess.assignments, assignments) {
			continue
		}
		// The follower's assignment set is fixed at construction, so a change
		// means a rebuild. Replication sessions are cheap to restart and
		// assignment changes are rare — this is the simple correct option, not
		// a hot path.
		if sess, ok := r.followers[addr]; ok {
			sess.follower.Close()
			delete(r.followers, addr)
		}

		f, err := replication.NewFollower(r.slog, replication.FollowerConfig{
			LeaderAddr:  addr,
			NodeID:      r.cfg.NodeID,
			Secret:      r.cfg.Secret,
			Assignments: assignments,
			SyncOnApply: r.cfg.SyncOnApply,
		})
		if err != nil {
			log.Printf("[streamctl] follower for %s: %v", addr, err)
			continue
		}
		f.Start()
		r.followers[addr] = &followerSession{follower: f, assignments: assignments}
		log.Printf("[streamctl] replicating %d partition(s) from %s", len(assignments), addr)
	}
}

func (r *Reconciler) partitionCount(topic string) int32 {
	if t, ok := r.meta.Topic(topic); ok {
		return t.Partitions
	}
	if t, err := r.slog.Topic(topic); err == nil {
		return t.PartitionCount()
	}
	return 1
}

func (r *Reconciler) shutdownFollowers() {
	r.mu.Lock()
	defer r.mu.Unlock()
	for addr, sess := range r.followers {
		sess.follower.Close()
		delete(r.followers, addr)
	}
}

// reportISR publishes, for each partition this node leads, which replicas are
// caught up.
//
// Only the leader can answer this — it is the only node that knows both what it
// has written and what each follower has acknowledged. The report is stamped
// with the leader epoch so a deposed leader's late report is rejected rather
// than believed.
func (r *Reconciler) reportISR() {
	if r.leader == nil || r.raft == nil {
		return
	}

	for _, a := range r.meta.LedBy(r.cfg.NodeID) {
		// InSync is computed by the replication leader against its own lag
		// threshold. Recomputing it here from raw lag would give the cluster two
		// definitions of "in sync" that drift apart under load.
		isr := []string{r.cfg.NodeID}
		for _, rep := range r.leader.Replication(a.Topic, a.Partition).Replicas {
			if rep.InSync {
				isr = append(isr, rep.NodeID)
			}
		}
		if sameStringSet(isr, a.ISR) {
			continue
		}

		resp, err := r.raft.Apply(&cluster.RaftCommand{
			Type: cluster.CmdMetaUpdateISR,
			Meta: &cluster.MetaCommand{
				Topic:       a.Topic,
				Partition:   a.Partition,
				LeaderEpoch: a.LeaderEpoch,
				ISR:         isr,
			},
		}, r.cfg.ApplyTimeout)
		if err != nil {
			// Commonly "not leader": ISR updates go through the Raft leader,
			// and this node is only the *partition* leader. Reporting it at
			// every tick would be noise; the next tick retries.
			continue
		}
		if resp.Error == nil {
			log.Printf("[streamctl] %s/%d ISR %v -> %v", a.Topic, a.Partition, a.ISR, isr)
		}
	}
}

// partitionRef keys a partition for set comparison.
type partitionRef struct {
	topic     string
	partition int32
}

func sameAssignments(a, b []replication.Assignment) bool {
	if len(a) != len(b) {
		return false
	}
	seen := make(map[partitionRef]bool, len(a))
	for _, x := range a {
		seen[partitionRef{x.Topic, x.Partition}] = true
	}
	for _, y := range b {
		if !seen[partitionRef{y.Topic, y.Partition}] {
			return false
		}
	}
	return true
}

func sameStringSet(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	seen := make(map[string]bool, len(a))
	for _, x := range a {
		seen[x] = true
	}
	for _, y := range b {
		if !seen[y] {
			return false
		}
	}
	return true
}
