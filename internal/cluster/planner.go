package cluster

import (
	"fmt"
	"sort"
)

// The planner answers two questions the controller cannot answer by itself:
// where should a new topic's replicas live, and which replicas should move now
// that the set of brokers has changed.
//
// It is pure. Given the same assignments and the same broker list it returns
// the same plan, every time, with no clock and no randomness. That is not
// tidiness — a rebalancer whose output wobbles between sweeps will start a move,
// change its mind, start a different one, and leave the cluster permanently
// copying data without ever converging. Determinism is what makes the plan
// safe to recompute every few seconds and act on only when it differs.
//
// It also never returns a plan that reduces durability. Every move is expressed
// as "add a replica here, then remove one there" — never the reverse — so a
// partition is briefly over-replicated during a move and never under-replicated.

// PlannerConfig tunes placement and rebalancing.
type PlannerConfig struct {
	// ReplicationFactor is how many copies of each partition to place.
	ReplicationFactor int

	// ImbalanceTolerance is how many replicas a broker may hold above the
	// cluster average before the planner tries to move one away.
	//
	// Zero would mean chasing perfect balance, which in a cluster whose
	// partition count is not divisible by its broker count is unreachable —
	// the planner would move a replica back and forth forever. Default 1.
	ImbalanceTolerance int

	// MaxConcurrentMoves bounds how many partitions are in flight at once.
	// Each move copies a whole partition across the network; running them all
	// at once turns a rebalance into an outage. Default 4.
	MaxConcurrentMoves int
}

func (c *PlannerConfig) applyDefaults() {
	if c.ReplicationFactor <= 0 {
		c.ReplicationFactor = 3
	}
	if c.ImbalanceTolerance <= 0 {
		c.ImbalanceTolerance = 1
	}
	if c.MaxConcurrentMoves <= 0 {
		c.MaxConcurrentMoves = 4
	}
}

// Planner computes placements and rebalance plans.
type Planner struct {
	cfg PlannerConfig
}

// NewPlanner creates a planner.
func NewPlanner(cfg PlannerConfig) *Planner {
	cfg.applyDefaults()
	return &Planner{cfg: cfg}
}

// Move is a single replica relocation: partition (Topic, Partition) should stop
// living on From and start living on To.
type Move struct {
	Topic     string `json:"topic"`
	Partition int32  `json:"partition"`
	From      string `json:"from"`
	To        string `json:"to"`
}

func (m Move) String() string {
	return fmt.Sprintf("%s/%d %s->%s", m.Topic, m.Partition, m.From, m.To)
}

// Place computes replica placement for a new topic.
//
// Two properties matter, in this order:
//
//  1. No two replicas of a partition share a rack, while enough racks exist.
//     Replicas that share a failure domain are not replicas; they are one copy
//     with extra steps.
//  2. Leadership spreads evenly. Replicas[0] is the preferred leader, so
//     starting every partition's list at the same broker would elect one node
//     leader of everything.
//
// The shape is Kafka's: walk a rack-interleaved broker list, start partition i
// at offset i, and space that partition's followers by a per-partition stride so
// the same two brokers are not always paired.
func (p *Planner) Place(partitions int32, brokers []BrokerInfo) ([][]string, error) {
	usable := liveSorted(brokers)
	if len(usable) == 0 {
		return nil, fmt.Errorf("cluster: no live broker to place partitions on")
	}

	rf := p.cfg.ReplicationFactor
	if rf > len(usable) {
		// Placing three copies on two machines would report a replication
		// factor the cluster cannot honour. Failing is the honest answer.
		return nil, fmt.Errorf("cluster: replication factor %d exceeds %d live brokers",
			rf, len(usable))
	}

	ordered := interleaveByRack(usable)
	n := len(ordered)

	out := make([][]string, partitions)
	for i := int32(0); i < partitions; i++ {
		start := int(i) % n
		// The stride shifts every time the placement wraps, which is what stops
		// partition 0 and partition n from getting an identical replica set.
		stride := int(i) / n % (n - 1)

		replicas := make([]string, 0, rf)
		replicas = append(replicas, ordered[start].NodeID)
		for j := 1; j < rf; j++ {
			idx := (start + j + stride*j) % n
			// Walk forward until an unused broker turns up. With rf <= n this
			// always terminates.
			for containsNode(replicas, ordered[idx].NodeID) {
				idx = (idx + 1) % n
			}
			replicas = append(replicas, ordered[idx].NodeID)
		}
		out[i] = replicas
	}
	return out, nil
}

// Rebalance returns the moves that would even out replica load.
//
// It deliberately returns *replica* moves, not leadership moves. Leadership is
// already pulled back toward the preferred replica by the controller on every
// sweep, and it costs nothing to move; a replica move copies a whole partition
// and is the expensive, careful thing this function exists to ration.
func (p *Planner) Rebalance(assignments []*PartitionAssignment, brokers []BrokerInfo) []Move {
	usable := liveSorted(brokers)
	if len(usable) < 2 || len(assignments) == 0 {
		return nil
	}

	rackOf := make(map[string]string, len(usable))
	live := make(map[string]bool, len(usable))
	for _, b := range usable {
		rackOf[b.NodeID] = b.Rack
		live[b.NodeID] = true
	}

	// Current load: replicas held per live broker. Brokers with none must still
	// appear, or a freshly-added node looks like it does not exist and never
	// receives anything — which is precisely the bug this function fixes.
	load := make(map[string]int, len(usable))
	for _, b := range usable {
		load[b.NodeID] = 0
	}
	total := 0
	for _, a := range assignments {
		for _, r := range a.Replicas {
			if live[r] {
				load[r]++
			}
			total++
		}
	}

	avg := float64(total) / float64(len(usable))
	ceiling := int(avg) + p.cfg.ImbalanceTolerance

	// Work on a copy: each planned move changes the load the next decision sees,
	// so a plan of several moves is internally consistent rather than every move
	// targeting the same unlucky broker.
	sorted := append([]*PartitionAssignment(nil), assignments...)
	sortAssignments(sorted)

	var moves []Move
	for _, a := range sorted {
		if len(moves) >= p.cfg.MaxConcurrentMoves {
			break
		}

		for _, from := range a.Replicas {
			if len(moves) >= p.cfg.MaxConcurrentMoves {
				break
			}
			// A replica on a broker that no longer exists must be moved
			// regardless of load: it is a copy that is never coming back.
			dead := !live[from]
			if !dead && load[from] <= ceiling {
				continue
			}

			to := p.pickTarget(a, from, usable, load, rackOf, ceiling)
			if to == "" {
				continue
			}

			moves = append(moves, Move{Topic: a.Topic, Partition: a.Partition, From: from, To: to})
			if !dead {
				load[from]--
			}
			load[to]++
			// One move per partition per plan. Two simultaneous moves on one
			// partition would take it below its replication factor if either
			// stalls.
			break
		}
	}
	return moves
}

// pickTarget chooses the best broker to receive a replica.
//
// Least-loaded first, and among equals the one that preserves rack diversity.
// A target that already holds this partition is not a target at all — it would
// silently collapse the replication factor by one.
func (p *Planner) pickTarget(
	a *PartitionAssignment, from string, brokers []BrokerInfo,
	load map[string]int, rackOf map[string]string, ceiling int,
) string {
	racksInUse := make(map[string]int)
	for _, r := range a.Replicas {
		if r != from {
			racksInUse[rackOf[r]]++
		}
	}

	best := ""
	bestScore := 0
	for _, b := range brokers {
		if containsNode(a.Replicas, b.NodeID) {
			continue
		}
		if load[b.NodeID] >= ceiling {
			continue
		}

		// Lower is better: load first, then whether this rack is already
		// represented. Multiplying load by two lets rack diversity break ties
		// between brokers one replica apart, without ever letting it override a
		// genuinely large load difference.
		score := load[b.NodeID] * 2
		if racksInUse[rackOf[b.NodeID]] > 0 {
			score++
		}
		if best == "" || score < bestScore {
			best, bestScore = b.NodeID, score
		}
	}
	return best
}

// liveSorted returns the unfenced brokers that can actually host a partition,
// in a stable order.
//
// A broker with no replication listener is excluded, and that is a correctness
// rule rather than a filter for tidiness: a replica placed on a node that
// advertises no StreamAddr can never be fetched from and can never fetch, so it
// would count toward the replication factor while being incapable of holding a
// copy. Dedicated controllers are exactly this case — they run consensus and
// host no data.
func liveSorted(brokers []BrokerInfo) []BrokerInfo {
	out := make([]BrokerInfo, 0, len(brokers))
	for _, b := range brokers {
		if b.Live() && b.StreamAddr != "" {
			out = append(out, b)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].NodeID < out[j].NodeID })
	return out
}

// interleaveByRack orders brokers so consecutive entries come from different
// racks wherever possible — round-robin across racks rather than rack by rack.
//
// Placement then gets rack diversity for free by walking the list, instead of
// every call site having to reason about failure domains.
func interleaveByRack(brokers []BrokerInfo) []BrokerInfo {
	byRack := map[string][]BrokerInfo{}
	var rackNames []string
	for _, b := range brokers {
		// An unlabelled broker is its own rack, not a member of a shared one.
		// Treating blank as a rack would report replicas as separated when
		// nothing established that they are.
		rack := b.Rack
		if rack == "" {
			rack = "\x00" + b.NodeID
		}
		if _, seen := byRack[rack]; !seen {
			rackNames = append(rackNames, rack)
		}
		byRack[rack] = append(byRack[rack], b)
	}
	sort.Strings(rackNames)

	out := make([]BrokerInfo, 0, len(brokers))
	for i := 0; len(out) < len(brokers); i++ {
		for _, rack := range rackNames {
			if i < len(byRack[rack]) {
				out = append(out, byRack[rack][i])
			}
		}
	}
	return out
}

func containsNode(list []string, id string) bool {
	for _, x := range list {
		if x == id {
			return true
		}
	}
	return false
}
