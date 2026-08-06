package cluster

import (
	"bytes"
	"encoding/json"
	"io"
	"testing"

	"github.com/boltq/boltq/pkg/protocol"
)

// TestMetadataFSMRejectsQueueCommands is the property that makes the split
// worth having. If the control-plane state machine quietly accepted queue
// commands, a misrouted publish would land in the metadata log — and the
// metadata log's whole value is that it stays small enough for every node in a
// large cluster to follow.
func TestMetadataFSMRejectsQueueCommands(t *testing.T) {
	f := NewMetadataFSM()

	queueCommands := []*RaftCommand{
		{Type: CmdRaftPublish, Topic: "jobs", Message: &protocol.Message{ID: "m1"}},
		{Type: CmdRaftConsume, Topic: "jobs"},
		{Type: CmdRaftAck, MessageID: "m1"},
		{Type: CmdRaftExchangeDeclare, ExchangeName: "ex"},
	}

	for _, cmd := range queueCommands {
		resp := f.ApplyCommand(cmd)
		if resp.Error == nil {
			t.Errorf("command %d was accepted by the control-plane FSM", cmd.Type)
		}
	}
	if f.Store().Version() != 0 {
		t.Errorf("metadata version moved to %d; queue commands must not touch it", f.Store().Version())
	}
}

// TestMetadataFSMAppliesControlCommands: the other half. Everything the
// controller emits must work here, or the split would have moved the control
// plane somewhere it cannot function.
func TestMetadataFSMAppliesControlCommands(t *testing.T) {
	f := NewMetadataFSM()

	steps := []*RaftCommand{
		{Type: CmdMetaRegisterBroker, Meta: &MetaCommand{
			Broker: &BrokerInfo{NodeID: "n1", StreamAddr: "n1:9200"}}},
		{Type: CmdMetaRegisterBroker, Meta: &MetaCommand{
			Broker: &BrokerInfo{NodeID: "n2", StreamAddr: "n2:9200"}}},
		{Type: CmdMetaCreateTopic, Meta: &MetaCommand{
			TopicMeta:  &TopicMeta{Name: "chat", Partitions: 2, ReplicationFactor: 2},
			Placements: [][]string{{"n1", "n2"}, {"n2", "n1"}}}},
		{Type: CmdMetaAssignLeader, Meta: &MetaCommand{
			Topic: "chat", Partition: 0, Leader: "n1", LeaderEpoch: 1}},
		{Type: CmdMetaUpdateISR, Meta: &MetaCommand{
			Topic: "chat", Partition: 0, LeaderEpoch: 1, ISR: []string{"n1", "n2"}}},
		{Type: CmdMetaFenceBroker, Meta: &MetaCommand{NodeID: "n2", Fenced: true, Timestamp: 1}},
	}

	for i, cmd := range steps {
		if resp := f.ApplyCommand(cmd); resp.Error != nil {
			t.Fatalf("step %d (command %d): %v", i, cmd.Type, resp.Error)
		}
	}

	a, ok := f.Store().Assignment("chat", 0)
	if !ok || a.Leader != "n1" || a.LeaderEpoch != 1 {
		t.Fatalf("assignment = %+v", a)
	}
	// Fencing n2 must have removed it from the ISR it had just joined.
	if a.InISR("n2") {
		t.Errorf("fenced n2 remains in ISR %v", a.ISR)
	}
}

// TestMetadataFSMSnapshotRoundTrip: a node joining a large cluster catches up
// from a snapshot, not by replaying history. Losing state here would silently
// give the new node a blank view of who leads what.
func TestMetadataFSMSnapshotRoundTrip(t *testing.T) {
	src := NewMetadataFSM()
	src.ApplyCommand(&RaftCommand{Type: CmdMetaRegisterBroker, Meta: &MetaCommand{
		Broker: &BrokerInfo{NodeID: "n1", StreamAddr: "n1:9200", Rack: "az-a"}}})
	src.ApplyCommand(&RaftCommand{Type: CmdMetaCreateTopic, Meta: &MetaCommand{
		TopicMeta:  &TopicMeta{Name: "chat", Partitions: 1, ReplicationFactor: 1},
		Placements: [][]string{{"n1"}}}})
	src.ApplyCommand(&RaftCommand{Type: CmdMetaAssignLeader, Meta: &MetaCommand{
		Topic: "chat", Partition: 0, Leader: "n1", LeaderEpoch: 9}})

	snap, err := src.Snapshot()
	if err != nil {
		t.Fatalf("snapshot: %v", err)
	}
	sink := &memSink{}
	if err := snap.Persist(sink); err != nil {
		t.Fatalf("persist: %v", err)
	}

	dst := NewMetadataFSM()
	if err := dst.Restore(io.NopCloser(bytes.NewReader(sink.Bytes()))); err != nil {
		t.Fatalf("restore: %v", err)
	}

	a, ok := dst.Store().Assignment("chat", 0)
	if !ok || a.Leader != "n1" || a.LeaderEpoch != 9 {
		t.Errorf("restored assignment = %+v", a)
	}
	if b, ok := dst.Store().Broker("n1"); !ok || b.Rack != "az-a" {
		t.Errorf("restored broker = %+v", b)
	}
}

// TestMetadataSnapshotStaysSmall is the number the whole split exists to
// protect. The control-plane log holds one entry per leadership change, not per
// record, so a node in a thousand-node cluster can hold the entire thing.
func TestMetadataSnapshotStaysSmall(t *testing.T) {
	f := NewMetadataFSM()

	// 50 brokers, 500 partitions — a substantial cluster.
	brokers := make([]string, 0, 50)
	for i := 0; i < 50; i++ {
		id := "node-" + string(rune('a'+i%26)) + string(rune('0'+i/26))
		brokers = append(brokers, id)
		f.ApplyCommand(&RaftCommand{Type: CmdMetaRegisterBroker, Meta: &MetaCommand{
			Broker: &BrokerInfo{NodeID: id, StreamAddr: id + ":9200"}}})
	}

	placements := make([][]string, 500)
	for i := range placements {
		placements[i] = []string{brokers[i%50], brokers[(i+1)%50], brokers[(i+2)%50]}
	}
	if resp := f.ApplyCommand(&RaftCommand{Type: CmdMetaCreateTopic, Meta: &MetaCommand{
		TopicMeta:  &TopicMeta{Name: "chat", Partitions: 500, ReplicationFactor: 3},
		Placements: placements,
	}}); resp.Error != nil {
		t.Fatalf("create topic: %v", resp.Error)
	}

	data, err := json.Marshal(f.Store().Snapshot())
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	// Roughly 200 bytes per partition. A megabyte here would still be fine; the
	// point of the bound is to fail loudly if message data ever leaks in, which
	// would blow past it by orders of magnitude.
	const limit = 512 * 1024
	if len(data) > limit {
		t.Errorf("metadata snapshot is %d bytes for 500 partitions, over the %d limit — "+
			"something other than metadata is being replicated", len(data), limit)
	}
	t.Logf("500 partitions, 50 brokers: %d bytes of metadata", len(data))
}

// memSink captures a snapshot in memory.
type memSink struct {
	bytes.Buffer
	cancelled bool
}

func (s *memSink) Close() error  { return nil }
func (s *memSink) ID() string    { return "test" }
func (s *memSink) Cancel() error { s.cancelled = true; return nil }
