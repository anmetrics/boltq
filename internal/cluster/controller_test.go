package cluster

import (
	"sync"
	"testing"
	"time"
)

// fakeRaft applies commands straight to a metadata store, skipping consensus.
// The controller's decisions are the interesting part; Raft's job of agreeing
// on them is already tested by Raft.
type fakeRaft struct {
	mu     sync.Mutex
	fsm    *BrokerFSM
	leader bool
	id     string
	// applied records every command, so a test can assert on what the
	// controller decided, not merely on where it ended up.
	applied []*RaftCommand
}

func newFakeRaft(id string) *fakeRaft {
	return &fakeRaft{fsm: &BrokerFSM{metadata: NewMetadataStore()}, leader: true, id: id}
}

func (f *fakeRaft) Apply(cmd *RaftCommand, _ time.Duration) (*ApplyResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.applied = append(f.applied, cmd)
	return f.fsm.applyMeta(cmd), nil
}

func (f *fakeRaft) IsLeader() bool { return f.leader }
func (f *fakeRaft) NodeID() string { return f.id }

func (f *fakeRaft) meta() *MetadataStore { return f.fsm.metadata }

func (f *fakeRaft) commandTypes() []CommandType {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]CommandType, 0, len(f.applied))
	for _, c := range f.applied {
		out = append(out, c.Type)
	}
	return out
}

// clusterFixture builds a 3-broker cluster with one 2-partition topic,
// replication factor 3, and a controller whose clock the test drives.
type clusterFixture struct {
	raft *fakeRaft
	ctrl *Controller
	now  time.Time
}

func newFixture(t *testing.T, cfg ControllerConfig) *clusterFixture {
	t.Helper()

	// The controller is a node of its own, holding no partitions — the topology
	// the deployment manifests describe, and the one that lets a test kill any
	// data node without killing the decision-maker.
	raft := newFakeRaft("ctrl")
	nodes := []string{"ctrl", "n1", "n2", "n3"}
	for _, id := range nodes {
		resp, err := raft.Apply(&RaftCommand{
			Type: CmdMetaRegisterBroker,
			Meta: &MetaCommand{Broker: &BrokerInfo{NodeID: id, StreamAddr: id + ":9200"}},
		}, time.Second)
		if err != nil || resp.Error != nil {
			t.Fatalf("register %s: %v %v", id, err, resp.Error)
		}
	}

	// Partition 0 prefers n1, partition 1 prefers n2, so a test can tell a
	// preferred-leader decision apart from "always picks the first node".
	resp, err := raft.Apply(&RaftCommand{
		Type: CmdMetaCreateTopic,
		Meta: &MetaCommand{
			TopicMeta:  &TopicMeta{Name: "chat", Partitions: 2, ReplicationFactor: 3, MinInSync: 2},
			Placements: [][]string{{"n1", "n2", "n3"}, {"n2", "n3", "n1"}},
		},
	}, time.Second)
	if err != nil || resp.Error != nil {
		t.Fatalf("create topic: %v %v", err, resp.Error)
	}

	f := &clusterFixture{raft: raft, now: time.Unix(1700000000, 0)}
	f.ctrl = newController(raft, raft.meta(), cfg)
	f.ctrl.clock = func() time.Time { return f.now }
	return f
}

// beat marks the given nodes alive at the current fixture time.
func (f *clusterFixture) beat(nodes ...string) {
	for _, id := range nodes {
		f.ctrl.Heartbeat(id)
	}
}

// advance moves the clock and runs a reconcile pass.
func (f *clusterFixture) advance(d time.Duration) {
	f.now = f.now.Add(d)
	f.ctrl.Reconcile()
}

func (f *clusterFixture) leaderOf(t *testing.T, partition int32) string {
	t.Helper()
	a, ok := f.raft.meta().Assignment("chat", partition)
	if !ok {
		t.Fatalf("chat/%d has no assignment", partition)
	}
	return a.Leader
}

func (f *clusterFixture) epochOf(t *testing.T, partition int32) uint32 {
	t.Helper()
	a, ok := f.raft.meta().Assignment("chat", partition)
	if !ok {
		t.Fatalf("chat/%d has no assignment", partition)
	}
	return a.LeaderEpoch
}

// TestControllerElectsPreferredLeaders checks the steady state: with everyone
// healthy, each partition lands on its preferred replica.
func TestControllerElectsPreferredLeaders(t *testing.T) {
	f := newFixture(t, ControllerConfig{PreferredLeaderRebalance: true})

	f.beat("n1", "n2", "n3")
	f.advance(time.Second)

	if got := f.leaderOf(t, 0); got != "n1" {
		t.Errorf("chat/0 leader = %q, want n1 (preferred)", got)
	}
	if got := f.leaderOf(t, 1); got != "n2" {
		t.Errorf("chat/1 leader = %q, want n2 (preferred)", got)
	}
	if got := f.epochOf(t, 0); got != 1 {
		t.Errorf("chat/0 epoch = %d, want 1 on first election", got)
	}
}

// TestControllerIsIdempotent is the guard against a busy loop: once leadership
// has settled, further sweeps must produce no Raft traffic at all. A controller
// that re-elects on every tick would bump the epoch every few seconds and force
// a truncation check on every follower.
func TestControllerIsIdempotent(t *testing.T) {
	f := newFixture(t, ControllerConfig{PreferredLeaderRebalance: true})

	f.beat("n1", "n2", "n3")
	f.advance(time.Second)

	before := len(f.raft.commandTypes())
	epoch := f.epochOf(t, 0)

	for i := 0; i < 5; i++ {
		f.beat("n1", "n2", "n3")
		f.advance(time.Second)
	}

	if after := len(f.raft.commandTypes()); after != before {
		t.Errorf("steady state applied %d extra commands, want 0: %v",
			after-before, f.raft.commandTypes()[before:])
	}
	if got := f.epochOf(t, 0); got != epoch {
		t.Errorf("epoch drifted from %d to %d with no failure", epoch, got)
	}
}

// TestControllerFailsOverOnSessionExpiry is the core HA behaviour: a leader
// that stops heartbeating is fenced and its partitions move to a surviving
// in-sync replica, under a strictly newer epoch.
func TestControllerFailsOverOnSessionExpiry(t *testing.T) {
	f := newFixture(t, ControllerConfig{
		SessionTimeout:           10 * time.Second,
		StartupGrace:             time.Nanosecond,
		PreferredLeaderRebalance: true,
	})

	f.beat("n1", "n2", "n3")
	f.advance(time.Second)
	if f.leaderOf(t, 0) != "n1" {
		t.Fatalf("setup: chat/0 should start on n1")
	}
	epochBefore := f.epochOf(t, 0)

	// n1 goes silent; the others keep beating.
	for i := 0; i < 4; i++ {
		f.beat("n2", "n3")
		f.advance(4 * time.Second)
	}

	b, ok := f.raft.meta().Broker("n1")
	if !ok || !b.Fenced {
		t.Fatalf("n1 should be fenced after session expiry, got %+v", b)
	}

	got := f.leaderOf(t, 0)
	if got == "n1" || got == "" {
		t.Fatalf("chat/0 leader = %q, want a surviving replica", got)
	}
	if e := f.epochOf(t, 0); e <= epochBefore {
		t.Errorf("chat/0 epoch = %d, must exceed %d after failover", e, epochBefore)
	}

	// The fenced node must also be out of the ISR — otherwise it would keep
	// counting toward min-in-sync while acknowledging nothing.
	a, _ := f.raft.meta().Assignment("chat", 0)
	if a.InISR("n1") {
		t.Errorf("fenced n1 is still in ISR %v", a.ISR)
	}
}

// TestControllerRestoresLeadershipOnRecovery covers the other half: a node that
// comes back is unfenced, and preferred-leader rebalance pulls its partitions
// home once it is in-sync again.
func TestControllerRestoresLeadershipOnRecovery(t *testing.T) {
	f := newFixture(t, ControllerConfig{
		SessionTimeout:           10 * time.Second,
		StartupGrace:             time.Nanosecond,
		PreferredLeaderRebalance: true,
	})

	f.beat("n1", "n2", "n3")
	f.advance(time.Second)

	for i := 0; i < 4; i++ {
		f.beat("n2", "n3")
		f.advance(4 * time.Second)
	}
	newLeader := f.leaderOf(t, 0)

	// n1 returns and heartbeats again.
	f.beat("n1", "n2", "n3")
	f.advance(time.Second)

	if b, _ := f.raft.meta().Broker("n1"); b.Fenced {
		t.Fatalf("n1 should be unfenced after heartbeats resumed")
	}

	// Leadership stays put until n1 rejoins the ISR: an unfenced replica is
	// alive, not caught up, and electing it here would drop records.
	if got := f.leaderOf(t, 0); got != newLeader {
		t.Errorf("chat/0 leader = %q, want %q — a live but out-of-sync replica must not be elected",
			got, newLeader)
	}

	// The current leader reports n1 back in sync.
	a, _ := f.raft.meta().Assignment("chat", 0)
	resp, err := f.raft.Apply(&RaftCommand{
		Type: CmdMetaUpdateISR,
		Meta: &MetaCommand{
			Topic: "chat", Partition: 0,
			LeaderEpoch: a.LeaderEpoch,
			ISR:         []string{newLeader, "n1"},
		},
	}, time.Second)
	if err != nil || resp.Error != nil {
		t.Fatalf("ISR update: %v %v", err, resp.Error)
	}

	f.beat("n1", "n2", "n3")
	f.advance(time.Second)

	if got := f.leaderOf(t, 0); got != "n1" {
		t.Errorf("chat/0 leader = %q, want n1 back as preferred leader", got)
	}
}

// TestControllerMarksPartitionOfflineRatherThanLoseData: when no in-sync
// replica survives, the safe answer is no leader. Electing a lagging replica
// would silently discard records that were acknowledged to publishers.
func TestControllerMarksPartitionOfflineRatherThanLoseData(t *testing.T) {
	f := newFixture(t, ControllerConfig{
		SessionTimeout: 10 * time.Second,
		StartupGrace:   time.Nanosecond,
	})

	f.beat("n1", "n2", "n3")
	f.advance(time.Second)

	// Shrink chat/0's ISR to n1 alone, then kill n1.
	a, _ := f.raft.meta().Assignment("chat", 0)
	resp, err := f.raft.Apply(&RaftCommand{
		Type: CmdMetaUpdateISR,
		Meta: &MetaCommand{Topic: "chat", Partition: 0, LeaderEpoch: a.LeaderEpoch, ISR: []string{a.Leader}},
	}, time.Second)
	if err != nil || resp.Error != nil {
		t.Fatalf("ISR shrink: %v %v", err, resp.Error)
	}
	dead := a.Leader

	for i := 0; i < 4; i++ {
		for _, id := range []string{"n1", "n2", "n3"} {
			if id != dead {
				f.beat(id)
			}
		}
		f.advance(4 * time.Second)
	}

	if got := f.leaderOf(t, 0); got != "" {
		t.Errorf("chat/0 leader = %q, want offline — no in-sync replica survived", got)
	}

	// The other partition, whose ISR was untouched, must stay online. A single
	// partition losing its ISR is not a reason to stall the whole topic.
	if got := f.leaderOf(t, 1); got == "" {
		t.Errorf("chat/1 went offline too; only chat/0 lost its ISR")
	}
}

// TestControllerUncleanElectionIsOptIn checks the availability-over-durability
// escape hatch does what it says, and only when asked.
func TestControllerUncleanElectionIsOptIn(t *testing.T) {
	f := newFixture(t, ControllerConfig{
		SessionTimeout:       10 * time.Second,
		StartupGrace:         time.Nanosecond,
		AllowUncleanElection: true,
	})

	f.beat("n1", "n2", "n3")
	f.advance(time.Second)

	a, _ := f.raft.meta().Assignment("chat", 0)
	f.raft.Apply(&RaftCommand{
		Type: CmdMetaUpdateISR,
		Meta: &MetaCommand{Topic: "chat", Partition: 0, LeaderEpoch: a.LeaderEpoch, ISR: []string{a.Leader}},
	}, time.Second)
	dead := a.Leader

	for i := 0; i < 4; i++ {
		for _, id := range []string{"n1", "n2", "n3"} {
			if id != dead {
				f.beat(id)
			}
		}
		f.advance(4 * time.Second)
	}

	got := f.leaderOf(t, 0)
	if got == "" || got == dead {
		t.Errorf("chat/0 leader = %q, want an unclean election to a live replica", got)
	}
}

// TestControllerStartupGraceAvoidsMassFencing: a controller that has just taken
// over has heard from nobody. Fencing on that ignorance would fail over every
// partition in the cluster at once.
func TestControllerStartupGraceAvoidsMassFencing(t *testing.T) {
	f := newFixture(t, ControllerConfig{
		SessionTimeout: 10 * time.Second,
		StartupGrace:   30 * time.Second,
	})

	// No heartbeats at all, well past the session timeout.
	f.advance(15 * time.Second)

	for _, id := range []string{"n1", "n2", "n3"} {
		if b, _ := f.raft.meta().Broker(id); b.Fenced {
			t.Errorf("%s fenced during startup grace", id)
		}
	}

	// Past the grace period, silence does mean dead.
	f.advance(30 * time.Second)
	for _, id := range []string{"n1", "n2", "n3"} {
		if b, _ := f.raft.meta().Broker(id); !b.Fenced {
			t.Errorf("%s should be fenced after the grace period expired", id)
		}
	}
}

// TestControllerNeverFencesItself is a regression test for a bug that made the
// control plane self-destruct: the controller's own liveness came from
// heartbeats it never sent itself, so after one session timeout it fenced
// itself and dropped every partition it led.
//
// If the sweep loop is running, the process is alive. There is no failure mode
// in between.
func TestControllerNeverFencesItself(t *testing.T) {
	f := newFixture(t, ControllerConfig{
		SessionTimeout:           10 * time.Second,
		StartupGrace:             time.Nanosecond,
		PreferredLeaderRebalance: true,
	})

	// Only the data nodes ever heartbeat. The controller sends none for itself.
	for i := 0; i < 6; i++ {
		f.beat("n1", "n2", "n3")
		f.advance(4 * time.Second)
	}

	b, ok := f.raft.meta().Broker("ctrl")
	if !ok {
		t.Fatal("controller lost its own registration")
	}
	if b.Fenced {
		t.Fatal("the controller fenced itself")
	}

	// The data nodes must be unaffected — a self-fencing controller took their
	// leaderships down with it.
	if got := f.leaderOf(t, 0); got == "" {
		t.Error("chat/0 went offline while every replica was healthy")
	}
}

// TestControllerIsInertOnFollowers: only the Raft leader may make decisions.
// Two controllers acting at once would assign the same partition to different
// nodes under different epochs.
func TestControllerIsInertOnFollowers(t *testing.T) {
	f := newFixture(t, ControllerConfig{SessionTimeout: 10 * time.Second, StartupGrace: time.Nanosecond})
	f.raft.leader = false

	before := len(f.raft.commandTypes())
	f.advance(60 * time.Second)

	if after := len(f.raft.commandTypes()); after != before {
		t.Errorf("non-leader applied %d commands, want 0", after-before)
	}
}
