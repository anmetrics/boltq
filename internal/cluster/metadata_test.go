package cluster

import (
	"encoding/json"
	"testing"
	"time"
)

func timeoutAfterSeconds(n int) <-chan time.Time {
	return time.After(time.Duration(n) * time.Second)
}

func mustCreateTopic(t *testing.T, m *MetadataStore) {
	t.Helper()
	err := m.applyCreateTopic(
		TopicMeta{Name: "chat", Partitions: 2, ReplicationFactor: 3, MinInSync: 2},
		[][]string{{"n1", "n2", "n3"}, {"n2", "n3", "n1"}},
	)
	if err != nil {
		t.Fatalf("create topic: %v", err)
	}
}

// TestLeaderEpochMustAdvance is the safety property the whole control plane
// rests on. Reusing an epoch would let two leadership terms stamp records with
// the same fencing token, and a follower could no longer tell which term's
// records it holds.
func TestLeaderEpochMustAdvance(t *testing.T) {
	m := NewMetadataStore()
	mustCreateTopic(t, m)

	if err := m.applyAssignLeader("chat", 0, "n1", 5, nil); err != nil {
		t.Fatalf("first assign: %v", err)
	}

	for _, epoch := range []uint32{5, 4, 0} {
		if err := m.applyAssignLeader("chat", 0, "n2", epoch, nil); err == nil {
			t.Errorf("assign at epoch %d was accepted; must be rejected as stale", epoch)
		}
	}

	a, _ := m.Assignment("chat", 0)
	if a.Leader != "n1" || a.LeaderEpoch != 5 {
		t.Errorf("rejected assigns mutated state: leader=%q epoch=%d", a.Leader, a.LeaderEpoch)
	}

	if err := m.applyAssignLeader("chat", 0, "n2", 6, nil); err != nil {
		t.Fatalf("advance to epoch 6: %v", err)
	}
}

// TestISRUpdateIsEpochFenced: a demoted leader can still have an ISR report in
// flight. Applying it would resurrect its view of who is caught up.
func TestISRUpdateIsEpochFenced(t *testing.T) {
	m := NewMetadataStore()
	mustCreateTopic(t, m)
	m.applyAssignLeader("chat", 0, "n1", 1, nil)

	// n1 is deposed.
	m.applyAssignLeader("chat", 0, "n2", 2, []string{"n2", "n3"})

	// A report from n1's term arrives late.
	if err := m.applyUpdateISR("chat", 0, 1, []string{"n1"}); err == nil {
		t.Fatal("ISR update from a stale epoch was accepted")
	}

	a, _ := m.Assignment("chat", 0)
	if a.InISR("n1") {
		t.Errorf("stale ISR report leaked into %v", a.ISR)
	}
}

// TestISRRejectsNonReplicas: an ISR member that holds no placement would be
// counted toward min-in-sync while having no obligation to keep the data.
func TestISRRejectsNonReplicas(t *testing.T) {
	m := NewMetadataStore()
	mustCreateTopic(t, m)
	m.applyAssignLeader("chat", 0, "n1", 1, nil)

	if err := m.applyUpdateISR("chat", 0, 1, []string{"n1", "n99"}); err == nil {
		t.Fatal("ISR containing a non-replica was accepted")
	}
}

// TestISRAlwaysContainsLeader: the leader holds every record by definition, so
// an ISR report that omits it is a bookkeeping slip, not a durability fact.
func TestISRAlwaysContainsLeader(t *testing.T) {
	m := NewMetadataStore()
	mustCreateTopic(t, m)
	m.applyAssignLeader("chat", 0, "n1", 1, nil)

	if err := m.applyUpdateISR("chat", 0, 1, []string{"n2"}); err != nil {
		t.Fatalf("ISR update: %v", err)
	}
	a, _ := m.Assignment("chat", 0)
	if !a.InISR("n1") {
		t.Errorf("leader n1 missing from its own ISR %v", a.ISR)
	}
}

// TestFencingShrinksISR: fencing is a durability statement as much as a
// liveness one — a node we cannot hear from cannot be counted as in-sync.
func TestFencingShrinksISR(t *testing.T) {
	m := NewMetadataStore()
	m.applyRegisterBroker(BrokerInfo{NodeID: "n1"})
	m.applyRegisterBroker(BrokerInfo{NodeID: "n2"})
	m.applyRegisterBroker(BrokerInfo{NodeID: "n3"})
	mustCreateTopic(t, m)

	changed, err := m.applyFenceBroker("n3", true, 1)
	if err != nil || !changed {
		t.Fatalf("fence n3: changed=%v err=%v", changed, err)
	}

	for _, pid := range []int32{0, 1} {
		a, _ := m.Assignment("chat", pid)
		if a.InISR("n3") {
			t.Errorf("chat/%d still lists fenced n3 in ISR %v", pid, a.ISR)
		}
		// Placement survives: fencing is not a decommission.
		found := false
		for _, r := range a.Replicas {
			if r == "n3" {
				found = true
			}
		}
		if !found {
			t.Errorf("chat/%d dropped n3 from Replicas %v; fencing must not rebalance", pid, a.Replicas)
		}
	}

	// Fencing an already-fenced broker is a no-op, so the controller can call
	// it without checking first and without generating Raft churn.
	if changed, _ := m.applyFenceBroker("n3", true, 2); changed {
		t.Error("re-fencing reported a change")
	}
}

// TestReRegistrationBumpsSession: a restarted process must be distinguishable
// from the one that died, or an in-flight message from the corpse could be
// mistaken for the new instance's.
func TestReRegistrationBumpsSession(t *testing.T) {
	m := NewMetadataStore()
	first := m.applyRegisterBroker(BrokerInfo{NodeID: "n1"})
	m.applyFenceBroker("n1", true, 1)
	second := m.applyRegisterBroker(BrokerInfo{NodeID: "n1"})

	if second.Session <= first.Session {
		t.Errorf("session did not advance: %d -> %d", first.Session, second.Session)
	}
	if second.Fenced {
		t.Error("a broker that just registered is alive; it must not stay fenced")
	}
}

// TestReassignDropsStaleISRMembers: a node told to stop hosting a partition
// must stop counting toward its durability at the same moment.
func TestReassignDropsStaleISRMembers(t *testing.T) {
	m := NewMetadataStore()
	mustCreateTopic(t, m)
	m.applyAssignLeader("chat", 0, "n1", 1, []string{"n1", "n2", "n3"})

	if err := m.applyReassign("chat", 0, []string{"n1", "n2"}); err != nil {
		t.Fatalf("reassign: %v", err)
	}
	a, _ := m.Assignment("chat", 0)
	if a.InISR("n3") {
		t.Errorf("n3 was removed from placement but remains in ISR %v", a.ISR)
	}
}

// TestSnapshotRoundTrip: a controller failover reads its predecessor's state
// out of a snapshot. Losing assignments there would re-elect every partition.
func TestSnapshotRoundTrip(t *testing.T) {
	m := NewMetadataStore()
	m.applyRegisterBroker(BrokerInfo{NodeID: "n1", StreamAddr: "a:1", Rack: "r1"})
	mustCreateTopic(t, m)
	m.applyAssignLeader("chat", 0, "n1", 7, []string{"n1", "n2"})

	// Through JSON, because that is how it actually travels.
	raw, err := json.Marshal(m.Snapshot())
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var st MetadataState
	if err := json.Unmarshal(raw, &st); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	restored := NewMetadataStore()
	restored.Restore(st)

	a, ok := restored.Assignment("chat", 0)
	if !ok {
		t.Fatal("chat/0 lost across snapshot")
	}
	if a.Leader != "n1" || a.LeaderEpoch != 7 {
		t.Errorf("assignment = leader %q epoch %d, want n1/7", a.Leader, a.LeaderEpoch)
	}
	if len(a.ISR) != 2 {
		t.Errorf("ISR = %v, want 2 members", a.ISR)
	}
	if b, ok := restored.Broker("n1"); !ok || b.Rack != "r1" {
		t.Errorf("broker registration lost: %+v", b)
	}
	if restored.Version() != m.Version() {
		t.Errorf("version = %d, want %d", restored.Version(), m.Version())
	}
}

// TestReadsAreCopies: a caller that mutates what it read must not corrupt the
// replicated state. This is easy to get wrong and impossible to debug later.
func TestReadsAreCopies(t *testing.T) {
	m := NewMetadataStore()
	mustCreateTopic(t, m)

	a, _ := m.Assignment("chat", 0)
	a.ISR[0] = "tampered"
	a.Leader = "tampered"

	fresh, _ := m.Assignment("chat", 0)
	if fresh.ISR[0] == "tampered" || fresh.Leader == "tampered" {
		t.Error("Assignment returned a view into FSM-owned state")
	}
}

// TestLedByAndReplicatedByPartition covers the two questions a node asks on
// every metadata change: what do I lead, and what should I be fetching?
func TestLedByAndReplicatedBy(t *testing.T) {
	m := NewMetadataStore()
	mustCreateTopic(t, m)
	m.applyAssignLeader("chat", 0, "n1", 1, nil)
	m.applyAssignLeader("chat", 1, "n2", 1, nil)

	led := m.LedBy("n1")
	if len(led) != 1 || led[0].Partition != 0 {
		t.Errorf("LedBy(n1) = %v, want chat/0 only", led)
	}

	follow := m.ReplicatedBy("n1")
	if len(follow) != 1 || follow[0].Partition != 1 {
		t.Errorf("ReplicatedBy(n1) = %v, want chat/1 only", follow)
	}
}

// TestSubscribeDeliversChanges: local components react to assignment moves
// rather than polling, so the event has to carry the post-change state.
func TestSubscribeDeliversChanges(t *testing.T) {
	m := NewMetadataStore()
	mustCreateTopic(t, m)

	events, cancel := m.Subscribe(8)
	defer cancel()

	m.applyAssignLeader("chat", 0, "n1", 3, nil)

	select {
	case ev := <-events:
		if ev.Assignment == nil || ev.Assignment.Leader != "n1" || ev.Assignment.LeaderEpoch != 3 {
			t.Errorf("event = %+v, want the post-change assignment", ev.Assignment)
		}
	default:
		t.Fatal("no event delivered for a leadership change")
	}
}

// TestSubscribeDropsRatherThanBlocks: FSM apply is the single ordered path for
// every node's state. A slow subscriber stalling it would stall consensus.
func TestSubscribeDropsRatherThanBlocks(t *testing.T) {
	m := NewMetadataStore()
	mustCreateTopic(t, m)

	_, cancel := m.Subscribe(1)
	defer cancel()

	done := make(chan struct{})
	go func() {
		defer close(done)
		for e := uint32(1); e <= 50; e++ {
			m.applyAssignLeader("chat", 0, "n1", e, nil)
		}
	}()

	select {
	case <-done:
	case <-timeoutAfterSeconds(5):
		t.Fatal("applies blocked on a full subscriber channel")
	}
}
