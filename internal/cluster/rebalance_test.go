package cluster

import (
	"testing"
	"time"
)

// rebalanceFixture is a 4-broker cluster with one 2-partition topic placed on
// three of them, leaving n4 empty — the shape that forces a rebalance.
type rebalanceFixture struct {
	*clusterFixture
}

func newRebalanceFixture(t *testing.T, cfg ControllerConfig) *rebalanceFixture {
	t.Helper()

	cfg.Rebalance = true
	cfg.StartupGrace = time.Nanosecond
	if cfg.SessionTimeout == 0 {
		cfg.SessionTimeout = 10 * time.Second
	}

	raft := newFakeRaft("ctrl")
	// The controller advertises no replication listener, so it can never be
	// chosen to host a partition — the dedicated-controller topology the
	// deployment manifests describe.
	raft.Apply(&RaftCommand{
		Type: CmdMetaRegisterBroker,
		Meta: &MetaCommand{Broker: &BrokerInfo{NodeID: "ctrl", AdminAddr: "ctrl:9090"}},
	}, time.Second)
	for _, id := range []string{"n1", "n2", "n3", "n4"} {
		raft.Apply(&RaftCommand{
			Type: CmdMetaRegisterBroker,
			Meta: &MetaCommand{Broker: &BrokerInfo{NodeID: id, StreamAddr: id + ":9200"}},
		}, time.Second)
	}

	// Eight partitions, all on n1..n3; n4 holds nothing.
	//
	// Eight rather than two because the planner tolerates being one replica
	// above average — correctly, since a partition count that does not divide
	// evenly by the broker count can never be perfectly balanced, and chasing
	// it would move replicas forever. Two partitions across four brokers is
	// *within* that tolerance and rightly produces no work, which is a fact
	// about the cluster and not something a rebalance test should fight.
	placements := [][]string{
		{"n1", "n2", "n3"}, {"n2", "n3", "n1"}, {"n3", "n1", "n2"}, {"n1", "n3", "n2"},
		{"n2", "n1", "n3"}, {"n3", "n2", "n1"}, {"n1", "n2", "n3"}, {"n2", "n3", "n1"},
	}
	resp, err := raft.Apply(&RaftCommand{
		Type: CmdMetaCreateTopic,
		Meta: &MetaCommand{
			TopicMeta:  &TopicMeta{Name: "chat", Partitions: int32(len(placements)), ReplicationFactor: 3},
			Placements: placements,
		},
	}, time.Second)
	if err != nil || resp.Error != nil {
		t.Fatalf("create topic: %v %v", err, resp.Error)
	}

	f := &clusterFixture{raft: raft, now: time.Unix(1700000000, 0)}
	f.ctrl = newController(raft, raft.meta(), cfg)
	f.ctrl.clock = func() time.Time { return f.now }
	return &rebalanceFixture{f}
}

// beatAll marks every data node alive.
func (f *rebalanceFixture) beatAll() {
	f.beat("n1", "n2", "n3", "n4")
}

// partitions lists every partition of the fixture topic.
func (f *rebalanceFixture) partitions() []int32 {
	return []int32{0, 1, 2, 3, 4, 5, 6, 7}
}

// catchUp simulates the destination replica finishing its copy: the partition
// leader would report it in sync. Done through the FSM, as the real path does.
func (f *rebalanceFixture) catchUp(t *testing.T, partition int32) {
	t.Helper()
	a, ok := f.raft.meta().Assignment("chat", partition)
	if !ok {
		t.Fatalf("chat/%d missing", partition)
	}
	resp, err := f.raft.Apply(&RaftCommand{
		Type: CmdMetaUpdateISR,
		Meta: &MetaCommand{
			Topic: "chat", Partition: partition,
			LeaderEpoch: a.LeaderEpoch,
			ISR:         append([]string(nil), a.Replicas...),
		},
	}, time.Second)
	if err != nil || resp.Error != nil {
		t.Fatalf("catch up chat/%d: %v %v", partition, err, resp.Error)
	}
}

func (f *rebalanceFixture) replicasOf(t *testing.T, partition int32) []string {
	t.Helper()
	a, ok := f.raft.meta().Assignment("chat", partition)
	if !ok {
		t.Fatalf("chat/%d missing", partition)
	}
	return a.Replicas
}

// TestMoveExpandsBeforeItShrinks is the safety property the whole move protocol
// exists for. Removing the source first would leave the partition one failure
// from data loss for the entire copy window, which on a large partition is
// minutes.
func TestMoveExpandsBeforeItShrinks(t *testing.T) {
	f := newRebalanceFixture(t, ControllerConfig{MaxConcurrentMoves: 1})

	f.beatAll()
	f.advance(time.Second)

	// A move should now be in flight, with the partition over-replicated.
	var moved int32 = -1
	for _, pid := range f.partitions() {
		if len(f.replicasOf(t, pid)) == 4 {
			moved = pid
		}
	}
	if moved < 0 {
		t.Fatalf("no partition was expanded; replicas: %v / %v",
			f.replicasOf(t, 0), f.replicasOf(t, 1))
	}
	if !containsNode(f.replicasOf(t, moved), "n4") {
		t.Errorf("expanded set %v does not include the destination", f.replicasOf(t, moved))
	}

	// It must stay over-replicated until the destination is in sync, however
	// many sweeps pass.
	for i := 0; i < 5; i++ {
		f.beatAll()
		f.advance(time.Second)
	}
	if got := len(f.replicasOf(t, moved)); got != 4 {
		t.Errorf("replica count dropped to %d before the destination caught up", got)
	}
}

// TestMoveCompletesAfterCatchUp: once the destination is in sync, the source is
// released and the partition returns to its replication factor.
func TestMoveCompletesAfterCatchUp(t *testing.T) {
	f := newRebalanceFixture(t, ControllerConfig{MaxConcurrentMoves: 1})

	f.beatAll()
	f.advance(time.Second)

	var moved int32 = -1
	for _, pid := range f.partitions() {
		if len(f.replicasOf(t, pid)) == 4 {
			moved = pid
		}
	}
	if moved < 0 {
		t.Fatal("no move started")
	}

	f.catchUp(t, moved)
	f.beatAll()
	f.advance(time.Second)

	replicas := f.replicasOf(t, moved)
	if len(replicas) != 3 {
		t.Errorf("replicas = %v, want the move to have completed at 3", replicas)
	}
	if !containsNode(replicas, "n4") {
		t.Errorf("replicas = %v, destination missing after completion", replicas)
	}
}

// TestMoveVacatesLeadershipBeforeRemovingTheLeader: dropping the leader from
// the replica set and letting the next sweep notice would leave the partition
// briefly led by a node that has been told to discard its data.
func TestMoveVacatesLeadershipBeforeRemovingTheLeader(t *testing.T) {
	f := newRebalanceFixture(t, ControllerConfig{MaxConcurrentMoves: 4})

	f.beatAll()
	f.advance(time.Second)

	for _, pid := range f.partitions() {
		if len(f.replicasOf(t, pid)) == 4 {
			f.catchUp(t, pid)
		}
	}
	f.beatAll()
	f.advance(time.Second)

	for _, pid := range f.partitions() {
		a, _ := f.raft.meta().Assignment("chat", pid)
		if a.Leader == "" {
			t.Errorf("chat/%d has no leader after a move", pid)
			continue
		}
		if !containsNode(a.Replicas, a.Leader) {
			t.Errorf("chat/%d is led by %s, which is not a replica: %v",
				pid, a.Leader, a.Replicas)
		}
	}
}

// TestMoveAbandonsRatherThanForces: a destination that never catches up must
// not cause the source to be removed. Over-replicated is a safe resting state;
// under-replicated is not.
func TestMoveAbandonsRatherThanForces(t *testing.T) {
	f := newRebalanceFixture(t, ControllerConfig{
		MaxConcurrentMoves: 1,
		MoveTimeout:        30 * time.Second,
	})

	f.beatAll()
	f.advance(time.Second)

	var moved int32 = -1
	for _, pid := range f.partitions() {
		if len(f.replicasOf(t, pid)) == 4 {
			moved = pid
		}
	}
	if moved < 0 {
		t.Fatal("no move started")
	}

	// Never catch up. Push well past the timeout.
	for i := 0; i < 5; i++ {
		f.beatAll()
		f.advance(20 * time.Second)
	}

	replicas := f.replicasOf(t, moved)
	if len(replicas) < 4 {
		t.Errorf("replicas = %v — the source was removed despite the destination never syncing", replicas)
	}

	f.ctrl.mu.Lock()
	inFlight := len(f.ctrl.moves)
	f.ctrl.mu.Unlock()
	if inFlight != 0 {
		t.Errorf("%d moves still tracked after the timeout", inFlight)
	}
}

// TestRebalanceWaitsForAQuietCluster: planning a new move while a partition is
// still copying counts a half-built replica as settled, and the next decision
// is made on a false picture of the load.
func TestRebalanceWaitsForAQuietCluster(t *testing.T) {
	f := newRebalanceFixture(t, ControllerConfig{MaxConcurrentMoves: 4})

	f.beatAll()
	f.advance(time.Second)

	before := 0
	for _, pid := range f.partitions() {
		before += len(f.replicasOf(t, pid))
	}

	// Sweep repeatedly without any partition catching up.
	for i := 0; i < 5; i++ {
		f.beatAll()
		f.advance(time.Second)
	}

	after := 0
	for _, pid := range f.partitions() {
		after += len(f.replicasOf(t, pid))
	}
	if after != before {
		t.Errorf("replica total moved from %d to %d while a move was outstanding", before, after)
	}
}

// TestRebalanceIsOptIn: moving replicas copies partitions across the network.
// It must not begin because someone turned on clustering.
func TestRebalanceIsOptIn(t *testing.T) {
	f := newRebalanceFixture(t, ControllerConfig{})
	f.ctrl.cfg.Rebalance = false

	f.beatAll()
	for i := 0; i < 5; i++ {
		f.advance(time.Second)
	}

	for _, pid := range f.partitions() {
		if got := len(f.replicasOf(t, pid)); got != 3 {
			t.Errorf("chat/%d has %d replicas with rebalancing off", pid, got)
		}
	}
}

// TestMovesDropOnLeadershipLoss: in-flight moves are one controller's
// intentions, not cluster state. A successor must form its own rather than
// inherit half-finished plans it cannot see the reasoning behind.
func TestMovesDropOnLeadershipLoss(t *testing.T) {
	f := newRebalanceFixture(t, ControllerConfig{MaxConcurrentMoves: 2})

	f.beatAll()
	f.advance(time.Second)

	f.ctrl.mu.Lock()
	started := len(f.ctrl.moves)
	f.ctrl.mu.Unlock()
	if started == 0 {
		t.Fatal("no move started")
	}

	f.raft.leader = false
	f.advance(time.Second)

	f.ctrl.mu.Lock()
	remaining := len(f.ctrl.moves)
	f.ctrl.mu.Unlock()
	if remaining != 0 {
		t.Errorf("%d moves survived leadership loss", remaining)
	}
}

// TestCreateTopicPlacesReplicas checks the controller-side entry point: a topic
// is placed with the whole live broker list in view, not by whoever received
// the request.
func TestCreateTopicPlacesReplicas(t *testing.T) {
	f := newRebalanceFixture(t, ControllerConfig{})

	if err := f.ctrl.CreateTopic("rooms", 6, 3); err != nil {
		t.Fatalf("create topic: %v", err)
	}

	meta, ok := f.raft.meta().Topic("rooms")
	if !ok {
		t.Fatal("topic was not recorded")
	}
	if meta.Partitions != 6 {
		t.Errorf("partitions = %d, want 6", meta.Partitions)
	}

	leaders := map[string]int{}
	for pid := int32(0); pid < 6; pid++ {
		a, ok := f.raft.meta().Assignment("rooms", pid)
		if !ok {
			t.Fatalf("rooms/%d has no assignment", pid)
		}
		if len(a.Replicas) != 3 {
			t.Errorf("rooms/%d has %d replicas, want 3", pid, len(a.Replicas))
		}
		leaders[a.Replicas[0]]++
	}
	if len(leaders) < 3 {
		t.Errorf("preferred leadership landed on only %d brokers: %v", len(leaders), leaders)
	}
}
