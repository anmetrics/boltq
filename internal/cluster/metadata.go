package cluster

import (
	"errors"
	"fmt"
	"sort"
	"sync"
)

// The metadata store is the cluster's answer to "who leads what".
//
// It deliberately holds no message data. Records are replicated by the stream
// layer's own leader/follower protocol, which is a sequential-append pipeline;
// pushing them through Raft instead would make every write pay a quorum fsync
// and cap the cluster at consensus throughput. What Raft is good at is small,
// rarely-changing, must-never-diverge facts: which node leads a partition,
// which replicas are in sync, and what leadership term we are on. That split —
// metadata through consensus, data through log replication — is the same one
// Kafka arrived at with KRaft, and it is what lets partition count scale into
// the hundreds of thousands without the control plane melting.
//
// Everything here is mutated exclusively from BrokerFSM.Apply, which Raft calls
// on one goroutine in log order on every node. That is what makes the state
// identical everywhere without any locking discipline beyond protecting readers.

var (
	// ErrNoSuchBroker means the node was never registered, or was unregistered.
	ErrNoSuchBroker = errors.New("cluster: no such broker")
	// ErrNoSuchPartition means the partition has no assignment recorded.
	ErrNoSuchPartition = errors.New("cluster: no such partition")
	// ErrNoEligibleLeader means every replica of a partition is fenced or out of
	// sync, so no leader can be chosen without risking data loss.
	ErrNoEligibleLeader = errors.New("cluster: no in-sync replica eligible for leadership")
)

// BrokerInfo is one node's registration in the cluster.
type BrokerInfo struct {
	NodeID string `json:"node_id"`
	// RaftAddr is the consensus port. StreamAddr is the replication listener a
	// follower dials. GatewayAddr is the client-facing address a redirect points
	// at. They are separate because they have different exposure: only
	// GatewayAddr may ever face the internet.
	RaftAddr    string `json:"raft_addr,omitempty"`
	StreamAddr  string `json:"stream_addr,omitempty"`
	GatewayAddr string `json:"gateway_addr,omitempty"`
	// AdminAddr is where this node's control HTTP API listens. It is recorded
	// so any node can find the controller's endpoint from metadata it already
	// replicates, instead of needing a second discovery mechanism just to
	// deliver a heartbeat.
	AdminAddr string `json:"admin_addr,omitempty"`
	// Rack lets replica placement spread across failure domains. Empty means
	// "unknown", which is treated as its own domain rather than as a match, so
	// a partially-labelled cluster degrades to no spreading instead of to a
	// false belief that replicas are separated.
	Rack string `json:"rack,omitempty"`

	// Session increments on every registration. A broker that restarts gets a
	// new session, so a heartbeat or an ack that was in flight from the dead
	// process cannot revive or advance the new one.
	Session uint64 `json:"session"`

	// Fenced means the controller stopped hearing from this broker. A fenced
	// broker holds no partition leadership and is not counted as in-sync, but
	// its registration and replica placements survive — fencing is a liveness
	// statement, not a decommission.
	Fenced bool `json:"fenced"`

	// RegisteredAt and FencedAt are proposer-supplied unix nanos. They are
	// carried in the command rather than read from the clock inside Apply,
	// because Apply must produce byte-identical state on every replica.
	RegisteredAt int64 `json:"registered_at,omitempty"`
	FencedAt     int64 `json:"fenced_at,omitempty"`
}

// Live reports whether the broker may currently hold leadership.
func (b BrokerInfo) Live() bool { return !b.Fenced }

// PartitionAssignment records leadership and replica placement for one
// partition.
type PartitionAssignment struct {
	Topic     string `json:"topic"`
	Partition int32  `json:"partition"`

	// Leader is the node that accepts writes. Empty means the partition is
	// offline: no eligible replica exists, and writes must be rejected rather
	// than accepted by whoever happens to ask.
	Leader string `json:"leader,omitempty"`
	// LeaderEpoch is the fencing token. It increases on every leadership change
	// and is stamped into every record the leader writes, which is what lets a
	// returning replica discover that its tail belongs to a term the cluster
	// abandoned. See internal/stream/epoch.go for the truncation protocol this
	// number drives.
	LeaderEpoch uint32 `json:"leader_epoch"`

	// Replicas is the placement, in preference order. Replicas[0] is the
	// preferred leader; the controller drifts leadership back to it when it is
	// healthy, which is what keeps load even after a failover storm.
	Replicas []string `json:"replicas"`
	// ISR is the subset of Replicas known to be caught up. Only an ISR member
	// may be elected, because electing a lagging replica silently discards
	// every record it had not yet fetched.
	ISR []string `json:"isr"`

	// Version increments on every change to this assignment. Clients cache
	// metadata and send it back; a stale version is how a node discovers it has
	// been demoted without having to be told.
	Version uint64 `json:"version"`
}

// IsLeader reports whether nodeID currently leads this partition.
func (a *PartitionAssignment) IsLeader(nodeID string) bool {
	return a.Leader != "" && a.Leader == nodeID
}

// InISR reports whether nodeID is in the in-sync set.
func (a *PartitionAssignment) InISR(nodeID string) bool {
	for _, id := range a.ISR {
		if id == nodeID {
			return true
		}
	}
	return false
}

// clone returns a deep copy, so readers can never alias FSM-owned slices.
func (a *PartitionAssignment) clone() *PartitionAssignment {
	c := *a
	c.Replicas = append([]string(nil), a.Replicas...)
	c.ISR = append([]string(nil), a.ISR...)
	return &c
}

// TopicMeta records a topic's shape. Partition count lives in metadata rather
// than being inferred from disk because a node that has never hosted a
// partition of a topic still has to route to it.
type TopicMeta struct {
	Name              string `json:"name"`
	Partitions        int32  `json:"partitions"`
	ReplicationFactor int32  `json:"replication_factor"`
	MinInSync         int32  `json:"min_in_sync"`
}

// MetadataEvent is emitted after each committed change so local components —
// the stream log, the gateway's routing table — can react without polling.
type MetadataEvent struct {
	// Assignment is the state after the change, or nil for broker-only events.
	Assignment *PartitionAssignment
	// Broker is set for registration, fencing and unfencing events.
	Broker *BrokerInfo
	// Version is the store version after the change.
	Version uint64
}

// MetadataState is the serialisable whole, used for Raft snapshots.
type MetadataState struct {
	Brokers    []BrokerInfo           `json:"brokers"`
	Topics     []TopicMeta            `json:"topics"`
	Partitions []*PartitionAssignment `json:"partitions"`
	Version    uint64                 `json:"version"`
}

// MetadataStore holds the replicated cluster metadata.
//
// Writes come only from the FSM; reads come from everywhere. The mutex protects
// readers from torn views, not writers from each other — Raft already
// serialises those.
type MetadataStore struct {
	mu         sync.RWMutex
	brokers    map[string]*BrokerInfo
	topics     map[string]*TopicMeta
	partitions map[string]*PartitionAssignment
	version    uint64

	subs map[int]chan MetadataEvent
	next int
}

// NewMetadataStore returns an empty store.
func NewMetadataStore() *MetadataStore {
	return &MetadataStore{
		brokers:    make(map[string]*BrokerInfo),
		topics:     make(map[string]*TopicMeta),
		partitions: make(map[string]*PartitionAssignment),
		subs:       make(map[int]chan MetadataEvent),
	}
}

// PartitionKey is the map key for a partition. Exported so callers can key
// their own caches identically.
func PartitionKey(topic string, partition int32) string {
	return fmt.Sprintf("%s/%d", topic, partition)
}

// --- reads -------------------------------------------------------------

// Broker returns a copy of a broker's registration.
func (m *MetadataStore) Broker(nodeID string) (BrokerInfo, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	b, ok := m.brokers[nodeID]
	if !ok {
		return BrokerInfo{}, false
	}
	return *b, true
}

// Brokers returns every registration, sorted by node ID for stable output.
func (m *MetadataStore) Brokers() []BrokerInfo {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make([]BrokerInfo, 0, len(m.brokers))
	for _, b := range m.brokers {
		out = append(out, *b)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].NodeID < out[j].NodeID })
	return out
}

// LiveBrokers returns the unfenced registrations, sorted by node ID.
func (m *MetadataStore) LiveBrokers() []BrokerInfo {
	out := m.Brokers()
	live := out[:0]
	for _, b := range out {
		if b.Live() {
			live = append(live, b)
		}
	}
	return live
}

// Assignment returns a copy of a partition's assignment.
func (m *MetadataStore) Assignment(topic string, partition int32) (*PartitionAssignment, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	a, ok := m.partitions[PartitionKey(topic, partition)]
	if !ok {
		return nil, false
	}
	return a.clone(), true
}

// Assignments returns every assignment, sorted by topic then partition.
func (m *MetadataStore) Assignments() []*PartitionAssignment {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make([]*PartitionAssignment, 0, len(m.partitions))
	for _, a := range m.partitions {
		out = append(out, a.clone())
	}
	sortAssignments(out)
	return out
}

// LedBy returns the partitions nodeID currently leads.
func (m *MetadataStore) LedBy(nodeID string) []*PartitionAssignment {
	m.mu.RLock()
	defer m.mu.RUnlock()
	var out []*PartitionAssignment
	for _, a := range m.partitions {
		if a.IsLeader(nodeID) {
			out = append(out, a.clone())
		}
	}
	sortAssignments(out)
	return out
}

// ReplicatedBy returns the partitions nodeID hosts but does not lead — exactly
// the set a follower should be fetching.
func (m *MetadataStore) ReplicatedBy(nodeID string) []*PartitionAssignment {
	m.mu.RLock()
	defer m.mu.RUnlock()
	var out []*PartitionAssignment
	for _, a := range m.partitions {
		if a.IsLeader(nodeID) {
			continue
		}
		for _, r := range a.Replicas {
			if r == nodeID {
				out = append(out, a.clone())
				break
			}
		}
	}
	sortAssignments(out)
	return out
}

// Topic returns a topic's shape.
func (m *MetadataStore) Topic(name string) (TopicMeta, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	t, ok := m.topics[name]
	if !ok {
		return TopicMeta{}, false
	}
	return *t, true
}

// Topics returns every topic, sorted by name.
func (m *MetadataStore) Topics() []TopicMeta {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make([]TopicMeta, 0, len(m.topics))
	for _, t := range m.topics {
		out = append(out, *t)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// Version returns the current store version.
func (m *MetadataStore) Version() uint64 {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.version
}

func sortAssignments(a []*PartitionAssignment) {
	sort.Slice(a, func(i, j int) bool {
		if a[i].Topic != a[j].Topic {
			return a[i].Topic < a[j].Topic
		}
		return a[i].Partition < a[j].Partition
	})
}

// --- subscription ------------------------------------------------------

// Subscribe returns a channel of change events and a function to stop it.
//
// The channel is buffered and lossy on overflow: a subscriber that cannot keep
// up gets the newest state on its next read rather than stalling FSM apply.
// This is safe because every event carries the full post-change assignment —
// there is no delta a subscriber can miss and be left inconsistent by.
func (m *MetadataStore) Subscribe(buffer int) (<-chan MetadataEvent, func()) {
	if buffer <= 0 {
		buffer = 64
	}
	ch := make(chan MetadataEvent, buffer)
	m.mu.Lock()
	id := m.next
	m.next++
	m.subs[id] = ch
	m.mu.Unlock()

	return ch, func() {
		m.mu.Lock()
		if c, ok := m.subs[id]; ok {
			delete(m.subs, id)
			close(c)
		}
		m.mu.Unlock()
	}
}

// publishLocked fans an event out to subscribers. The caller holds m.mu.
func (m *MetadataStore) publishLocked(ev MetadataEvent) {
	ev.Version = m.version
	for _, ch := range m.subs {
		select {
		case ch <- ev:
		default:
			// Dropped; see Subscribe.
		}
	}
}

// --- writes (FSM-only) -------------------------------------------------

// applyRegisterBroker records or refreshes a broker registration.
//
// Re-registration bumps the session and clears fencing: a broker that comes
// back is live by definition, and leaving it fenced would require a second
// round trip before it could take any leadership.
func (m *MetadataStore) applyRegisterBroker(b BrokerInfo) *BrokerInfo {
	m.mu.Lock()
	defer m.mu.Unlock()

	prev, existed := m.brokers[b.NodeID]
	if existed {
		b.Session = prev.Session + 1
	} else if b.Session == 0 {
		b.Session = 1
	}
	b.Fenced = false
	b.FencedAt = 0

	cp := b
	m.brokers[b.NodeID] = &cp
	m.version++
	m.publishLocked(MetadataEvent{Broker: &cp})
	return &cp
}

// applyUnregisterBroker removes a registration entirely.
//
// Unlike fencing, this is a decommission: the node is dropped from every ISR so
// it stops counting toward durability. It is deliberately left in Replicas —
// removing placements is a rebalance decision, not a liveness one, and doing it
// implicitly here would silently under-replicate every partition it held.
func (m *MetadataStore) applyUnregisterBroker(nodeID string) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	if _, ok := m.brokers[nodeID]; !ok {
		return ErrNoSuchBroker
	}
	delete(m.brokers, nodeID)
	m.shrinkISRLocked(nodeID)
	m.version++
	m.publishLocked(MetadataEvent{})
	return nil
}

// applyFenceBroker marks a broker live or dead. Returns whether anything
// changed, so the controller can skip a no-op Raft round trip.
func (m *MetadataStore) applyFenceBroker(nodeID string, fenced bool, at int64) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	b, ok := m.brokers[nodeID]
	if !ok {
		return false, ErrNoSuchBroker
	}
	if b.Fenced == fenced {
		return false, nil
	}
	b.Fenced = fenced
	if fenced {
		b.FencedAt = at
		// A fenced broker cannot be counted as in-sync: its acknowledgements
		// are what durability is measured in, and we have stopped hearing them.
		m.shrinkISRLocked(nodeID)
	} else {
		b.FencedAt = 0
	}
	m.version++
	cp := *b
	m.publishLocked(MetadataEvent{Broker: &cp})
	return true, nil
}

// shrinkISRLocked drops nodeID from every ISR. The caller holds m.mu.
//
// Leadership is not reassigned here. That is the controller's job and it needs
// to be a separate, explicitly-logged decision — an ISR shrink that silently
// moved leadership would make the reason for a failover impossible to audit.
func (m *MetadataStore) shrinkISRLocked(nodeID string) {
	for _, a := range m.partitions {
		if !a.InISR(nodeID) {
			continue
		}
		isr := a.ISR[:0]
		for _, id := range a.ISR {
			if id != nodeID {
				isr = append(isr, id)
			}
		}
		a.ISR = isr
		a.Version++
	}
}

// applyCreateTopic records a topic's shape and its initial placements.
func (m *MetadataStore) applyCreateTopic(meta TopicMeta, placements [][]string) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	if _, exists := m.topics[meta.Name]; exists {
		return fmt.Errorf("cluster: topic %q already exists", meta.Name)
	}
	if int(meta.Partitions) != len(placements) {
		return fmt.Errorf("cluster: topic %q declares %d partitions but got %d placements",
			meta.Name, meta.Partitions, len(placements))
	}

	cp := meta
	m.topics[meta.Name] = &cp
	for pid, replicas := range placements {
		a := &PartitionAssignment{
			Topic:     meta.Name,
			Partition: int32(pid),
			Replicas:  append([]string(nil), replicas...),
			// Every replica starts in-sync: they all hold the same empty log,
			// which is trivially caught up. Starting with an empty ISR would
			// leave the partition unelectable until the first fetch.
			ISR:     append([]string(nil), replicas...),
			Version: 1,
		}
		m.partitions[PartitionKey(meta.Name, int32(pid))] = a
	}
	m.version++
	m.publishLocked(MetadataEvent{})
	return nil
}

// applyAssignLeader installs a new leader and epoch for one partition.
//
// The epoch must strictly increase. Rejecting a stale or equal epoch is what
// makes the command safe to retry: a controller that failed over mid-decision
// may re-propose an assignment the previous controller already committed, and
// applying it twice would reuse an epoch that records were already written
// under.
func (m *MetadataStore) applyAssignLeader(topic string, partition int32, leader string, epoch uint32, isr []string) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	a, ok := m.partitions[PartitionKey(topic, partition)]
	if !ok {
		return ErrNoSuchPartition
	}
	if epoch <= a.LeaderEpoch {
		return fmt.Errorf("cluster: leader epoch %d for %s/%d is not newer than %d",
			epoch, topic, partition, a.LeaderEpoch)
	}
	a.Leader = leader
	a.LeaderEpoch = epoch
	if isr != nil {
		a.ISR = append([]string(nil), isr...)
	}
	a.Version++
	m.version++
	m.publishLocked(MetadataEvent{Assignment: a.clone()})
	return nil
}

// applyUpdateISR replaces a partition's in-sync set.
//
// Guarded by the leader epoch: only the node that currently leads may report
// who is caught up with it, and a report stamped with an older epoch comes from
// a leader that has since been replaced.
func (m *MetadataStore) applyUpdateISR(topic string, partition int32, epoch uint32, isr []string) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	a, ok := m.partitions[PartitionKey(topic, partition)]
	if !ok {
		return ErrNoSuchPartition
	}
	if epoch != a.LeaderEpoch {
		return fmt.Errorf("cluster: ISR update for %s/%d carries epoch %d, current is %d",
			topic, partition, epoch, a.LeaderEpoch)
	}

	// The leader is always in its own ISR, and every member must be a replica —
	// an ISR entry that is not a placement would be counted toward durability
	// while holding no obligation to keep the data.
	seen := map[string]bool{}
	clean := make([]string, 0, len(isr))
	for _, id := range isr {
		if seen[id] {
			continue
		}
		placed := false
		for _, r := range a.Replicas {
			if r == id {
				placed = true
				break
			}
		}
		if !placed {
			return fmt.Errorf("cluster: %s is not a replica of %s/%d", id, topic, partition)
		}
		seen[id] = true
		clean = append(clean, id)
	}
	if a.Leader != "" && !seen[a.Leader] {
		clean = append([]string{a.Leader}, clean...)
	}

	a.ISR = clean
	a.Version++
	m.version++
	m.publishLocked(MetadataEvent{Assignment: a.clone()})
	return nil
}

// applyReassign changes a partition's placement without touching leadership.
func (m *MetadataStore) applyReassign(topic string, partition int32, replicas []string) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	a, ok := m.partitions[PartitionKey(topic, partition)]
	if !ok {
		return ErrNoSuchPartition
	}
	a.Replicas = append([]string(nil), replicas...)

	// An ISR member that is no longer a replica stops counting immediately;
	// keeping it would let a node that has been told to drop the data still
	// satisfy min-in-sync.
	keep := a.ISR[:0]
	for _, id := range a.ISR {
		for _, r := range a.Replicas {
			if id == r {
				keep = append(keep, id)
				break
			}
		}
	}
	a.ISR = keep
	a.Version++
	m.version++
	m.publishLocked(MetadataEvent{Assignment: a.clone()})
	return nil
}

// --- snapshot ----------------------------------------------------------

// Snapshot returns a deep copy for Raft snapshotting.
func (m *MetadataStore) Snapshot() MetadataState {
	m.mu.RLock()
	defer m.mu.RUnlock()

	st := MetadataState{Version: m.version}
	for _, b := range m.brokers {
		st.Brokers = append(st.Brokers, *b)
	}
	for _, t := range m.topics {
		st.Topics = append(st.Topics, *t)
	}
	for _, a := range m.partitions {
		st.Partitions = append(st.Partitions, a.clone())
	}
	sort.Slice(st.Brokers, func(i, j int) bool { return st.Brokers[i].NodeID < st.Brokers[j].NodeID })
	sort.Slice(st.Topics, func(i, j int) bool { return st.Topics[i].Name < st.Topics[j].Name })
	sortAssignments(st.Partitions)
	return st
}

// Restore replaces all state from a snapshot.
func (m *MetadataStore) Restore(st MetadataState) {
	m.mu.Lock()
	defer m.mu.Unlock()

	m.brokers = make(map[string]*BrokerInfo, len(st.Brokers))
	m.topics = make(map[string]*TopicMeta, len(st.Topics))
	m.partitions = make(map[string]*PartitionAssignment, len(st.Partitions))
	for i := range st.Brokers {
		b := st.Brokers[i]
		m.brokers[b.NodeID] = &b
	}
	for i := range st.Topics {
		t := st.Topics[i]
		m.topics[t.Name] = &t
	}
	for _, a := range st.Partitions {
		m.partitions[PartitionKey(a.Topic, a.Partition)] = a.clone()
	}
	m.version = st.Version
	m.publishLocked(MetadataEvent{})
}
