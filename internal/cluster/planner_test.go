package cluster

import (
	"fmt"
	"testing"
)

func brokersIn(racks map[string][]string) []BrokerInfo {
	var out []BrokerInfo
	for rack, ids := range racks {
		for _, id := range ids {
			out = append(out, BrokerInfo{NodeID: id, Rack: rack, StreamAddr: id + ":9200"})
		}
	}
	return liveSorted(out)
}

func flatBrokers(n int) []BrokerInfo {
	out := make([]BrokerInfo, 0, n)
	for i := 1; i <= n; i++ {
		id := fmt.Sprintf("n%d", i)
		out = append(out, BrokerInfo{NodeID: id, StreamAddr: id + ":9200"})
	}
	return out
}

// TestPlaceSpreadsAcrossRacks is the property that makes a replica a replica.
// Three copies in one rack survive a disk failure and nothing else — a single
// power event takes all of them.
func TestPlaceSpreadsAcrossRacks(t *testing.T) {
	p := NewPlanner(PlannerConfig{ReplicationFactor: 3})
	brokers := brokersIn(map[string][]string{
		"a": {"n1", "n2"},
		"b": {"n3", "n4"},
		"c": {"n5", "n6"},
	})

	placements, err := p.Place(12, brokers)
	if err != nil {
		t.Fatalf("place: %v", err)
	}

	rack := map[string]string{}
	for _, b := range brokers {
		rack[b.NodeID] = b.Rack
	}

	for pid, replicas := range placements {
		seen := map[string]bool{}
		for _, r := range replicas {
			if seen[rack[r]] {
				t.Errorf("partition %d has two replicas in rack %s: %v", pid, rack[r], replicas)
			}
			seen[rack[r]] = true
		}
	}
}

// TestPlaceSpreadsLeadership: Replicas[0] is the preferred leader, so a
// placement that always starts at the same broker elects one node leader of
// every partition — the load imbalance the planner exists to avoid.
func TestPlaceSpreadsLeadership(t *testing.T) {
	p := NewPlanner(PlannerConfig{ReplicationFactor: 3})
	brokers := flatBrokers(5)

	placements, err := p.Place(20, brokers)
	if err != nil {
		t.Fatalf("place: %v", err)
	}

	leaderCount := map[string]int{}
	for _, r := range placements {
		leaderCount[r[0]]++
	}
	if len(leaderCount) != 5 {
		t.Errorf("only %d of 5 brokers ever lead: %v", len(leaderCount), leaderCount)
	}
	for id, n := range leaderCount {
		if n < 3 || n > 5 {
			t.Errorf("broker %s leads %d of 20 partitions, want ~4", id, n)
		}
	}
}

// TestPlaceNeverDuplicatesABroker: two replicas on one broker is one replica
// with the replication factor lying about it.
func TestPlaceNeverDuplicatesABroker(t *testing.T) {
	p := NewPlanner(PlannerConfig{ReplicationFactor: 3})

	for _, n := range []int{3, 4, 5, 7, 11} {
		placements, err := p.Place(50, flatBrokers(n))
		if err != nil {
			t.Fatalf("place with %d brokers: %v", n, err)
		}
		for pid, replicas := range placements {
			seen := map[string]bool{}
			for _, r := range replicas {
				if seen[r] {
					t.Fatalf("%d brokers: partition %d lists %s twice: %v", n, pid, r, replicas)
				}
				seen[r] = true
			}
			if len(replicas) != 3 {
				t.Fatalf("%d brokers: partition %d has %d replicas", n, pid, len(replicas))
			}
		}
	}
}

// TestPlaceRefusesImpossibleReplicationFactor: reporting rf=3 on two machines
// would be a durability claim the cluster cannot honour.
func TestPlaceRefusesImpossibleReplicationFactor(t *testing.T) {
	p := NewPlanner(PlannerConfig{ReplicationFactor: 3})
	if _, err := p.Place(4, flatBrokers(2)); err == nil {
		t.Fatal("placing 3 replicas on 2 brokers was accepted")
	}
}

// TestPlaceIsDeterministic: the plan is recomputed on every sweep. If it
// wobbled, the cluster would copy partitions back and forth forever.
func TestPlaceIsDeterministic(t *testing.T) {
	p := NewPlanner(PlannerConfig{ReplicationFactor: 3})
	brokers := flatBrokers(6)

	first, _ := p.Place(24, brokers)
	for i := 0; i < 5; i++ {
		again, _ := p.Place(24, brokers)
		for pid := range first {
			if fmt.Sprint(first[pid]) != fmt.Sprint(again[pid]) {
				t.Fatalf("partition %d: %v then %v", pid, first[pid], again[pid])
			}
		}
	}
}

// assignmentsFrom builds assignments from a placement table.
func assignmentsFrom(topic string, placements [][]string) []*PartitionAssignment {
	out := make([]*PartitionAssignment, 0, len(placements))
	for pid, replicas := range placements {
		out = append(out, &PartitionAssignment{
			Topic:     topic,
			Partition: int32(pid),
			Leader:    replicas[0],
			Replicas:  append([]string(nil), replicas...),
			ISR:       append([]string(nil), replicas...),
		})
	}
	return out
}

// TestRebalanceIsQuietWhenBalanced is the single most important property. A
// rebalancer that finds work to do on a balanced cluster will never stop
// copying data.
func TestRebalanceIsQuietWhenBalanced(t *testing.T) {
	p := NewPlanner(PlannerConfig{ReplicationFactor: 3})
	brokers := flatBrokers(6)
	placements, _ := p.Place(24, brokers)

	moves := p.Rebalance(assignmentsFrom("chat", placements), brokers)
	if len(moves) != 0 {
		t.Errorf("balanced cluster produced %d moves: %v", len(moves), moves)
	}
}

// TestRebalanceFillsANewBroker: this is what "scale out" has to mean. Adding a
// machine that never receives a partition adds nothing but cost.
func TestRebalanceFillsANewBroker(t *testing.T) {
	p := NewPlanner(PlannerConfig{ReplicationFactor: 3, MaxConcurrentMoves: 8})
	initial := flatBrokers(3)
	placements, _ := p.Place(12, initial)
	assignments := assignmentsFrom("chat", placements)

	expanded := flatBrokers(4) // n4 joins, holding nothing
	moves := p.Rebalance(assignments, expanded)

	if len(moves) == 0 {
		t.Fatal("a new empty broker attracted no replicas")
	}
	for _, mv := range moves {
		if mv.To != "n4" {
			t.Errorf("move %v targets %s, but only n4 is underloaded", mv, mv.To)
		}
	}
}

// TestRebalanceRespectsConcurrencyCap: every move copies a whole partition.
// Starting them all at once turns a rebalance into an outage.
func TestRebalanceRespectsConcurrencyCap(t *testing.T) {
	p := NewPlanner(PlannerConfig{ReplicationFactor: 3, MaxConcurrentMoves: 2})
	placements, _ := p.Place(30, flatBrokers(3))

	moves := p.Rebalance(assignmentsFrom("chat", placements), flatBrokers(6))
	if len(moves) > 2 {
		t.Errorf("planned %d concurrent moves, cap is 2", len(moves))
	}
}

// TestRebalanceNeverTargetsAnExistingReplica: moving a replica onto a broker
// that already holds one silently drops the replication factor by one.
func TestRebalanceNeverTargetsAnExistingReplica(t *testing.T) {
	p := NewPlanner(PlannerConfig{ReplicationFactor: 2, MaxConcurrentMoves: 10})
	assignments := []*PartitionAssignment{
		{Topic: "chat", Partition: 0, Leader: "n1", Replicas: []string{"n1", "n2"}, ISR: []string{"n1", "n2"}},
		{Topic: "chat", Partition: 1, Leader: "n1", Replicas: []string{"n1", "n2"}, ISR: []string{"n1", "n2"}},
		{Topic: "chat", Partition: 2, Leader: "n1", Replicas: []string{"n1", "n2"}, ISR: []string{"n1", "n2"}},
		{Topic: "chat", Partition: 3, Leader: "n1", Replicas: []string{"n1", "n2"}, ISR: []string{"n1", "n2"}},
	}

	for _, mv := range p.Rebalance(assignments, flatBrokers(4)) {
		for _, a := range assignments {
			if a.Partition != mv.Partition {
				continue
			}
			if containsNode(a.Replicas, mv.To) {
				t.Errorf("move %v targets %s, already a replica of %v", mv, mv.To, a.Replicas)
			}
		}
	}
}

// TestRebalanceEvacuatesADeadBroker: a replica on a broker that is gone is not
// a copy, and it must be replaced regardless of how balanced the load looks.
func TestRebalanceEvacuatesADeadBroker(t *testing.T) {
	p := NewPlanner(PlannerConfig{ReplicationFactor: 3, MaxConcurrentMoves: 10})
	assignments := []*PartitionAssignment{
		{Topic: "chat", Partition: 0, Leader: "n1", Replicas: []string{"n1", "n2", "dead"}, ISR: []string{"n1", "n2"}},
	}
	// "dead" is absent from the broker list entirely — decommissioned, not
	// merely fenced.
	moves := p.Rebalance(assignments, flatBrokers(4))

	found := false
	for _, mv := range moves {
		if mv.From == "dead" {
			found = true
			if mv.To == "n1" || mv.To == "n2" {
				t.Errorf("move %v targets an existing replica", mv)
			}
		}
	}
	if !found {
		t.Errorf("no move evacuates the departed broker: %v", moves)
	}
}

// TestRebalanceConverges: applying the plan repeatedly must reach a fixed
// point. A planner that never converges is worse than none, because it consumes
// network forever while looking busy and healthy.
func TestRebalanceConverges(t *testing.T) {
	p := NewPlanner(PlannerConfig{ReplicationFactor: 3, MaxConcurrentMoves: 4})
	placements, _ := p.Place(24, flatBrokers(3))
	assignments := assignmentsFrom("chat", placements)
	brokers := flatBrokers(6)

	rounds := 0
	for {
		moves := p.Rebalance(assignments, brokers)
		if len(moves) == 0 {
			break
		}
		rounds++
		if rounds > 50 {
			t.Fatalf("did not converge after %d rounds; still planning %v", rounds, moves)
		}
		// Apply the plan the way the controller does: expand then shrink, so
		// the replica set is never smaller than it started.
		for _, mv := range moves {
			for _, a := range assignments {
				if a.Topic != mv.Topic || a.Partition != mv.Partition {
					continue
				}
				next := []string{}
				for _, r := range a.Replicas {
					if r != mv.From {
						next = append(next, r)
					}
				}
				a.Replicas = append(next, mv.To)
				a.ISR = append([]string(nil), a.Replicas...)
				if a.Leader == mv.From {
					a.Leader = a.Replicas[0]
				}
			}
		}
	}

	// And the fixed point must actually be balanced.
	load := map[string]int{}
	for _, b := range brokers {
		load[b.NodeID] = 0
	}
	for _, a := range assignments {
		for _, r := range a.Replicas {
			load[r]++
		}
	}
	avg := (24 * 3) / 6
	for id, n := range load {
		if n < avg-2 || n > avg+2 {
			t.Errorf("broker %s holds %d replicas, average is %d: %v", id, n, avg, load)
		}
	}
}
