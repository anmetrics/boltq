package streamctl

import (
	"testing"
	"time"

	"github.com/boltq/boltq/internal/cluster"
	"github.com/boltq/boltq/internal/stream"
)

// metaFixture drives a real metadata store through its FSM, which is the only
// way state is ever mutated in production. Reaching into the store directly
// would test a path that does not exist at runtime.
type metaFixture struct {
	fsm *cluster.BrokerFSM
}

func newMetaFixture(t *testing.T) *metaFixture {
	t.Helper()
	return &metaFixture{fsm: cluster.NewBrokerFSM(nil)}
}

func (m *metaFixture) meta() *cluster.MetadataStore { return m.fsm.Metadata() }

func (m *metaFixture) Apply(cmd *cluster.RaftCommand, _ time.Duration) (*cluster.ApplyResponse, error) {
	data, err := cmd.Encode()
	if err != nil {
		return nil, err
	}
	decoded, err := cluster.DecodeCommand(data)
	if err != nil {
		return nil, err
	}
	return m.fsm.ApplyCommand(decoded), nil
}

func (m *metaFixture) must(t *testing.T, cmd *cluster.RaftCommand) {
	t.Helper()
	resp, err := m.Apply(cmd, time.Second)
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	if resp.Error != nil {
		t.Fatalf("apply rejected: %v", resp.Error)
	}
}

func openTestLog(t *testing.T) *stream.Log {
	t.Helper()
	l, err := stream.OpenLog(stream.LogConfig{
		Dir: t.TempDir(),
		DefaultTopic: stream.TopicConfig{
			Partitions: 2,
			Partition:  stream.PartitionConfig{SegmentBytes: 1 << 20},
		},
	})
	if err != nil {
		t.Fatalf("open log: %v", err)
	}
	t.Cleanup(func() { l.Close() })
	return l
}

// TestReconcilerPromotesAssignedPartitions is the link that makes the control
// plane real: a leadership decision made through consensus has to end up as a
// local epoch on disk, or the node will keep writing under the old term.
func TestReconcilerPromotesAssignedPartitions(t *testing.T) {
	f := newMetaFixture(t)
	slog := openTestLog(t)

	f.must(t, &cluster.RaftCommand{
		Type: cluster.CmdMetaRegisterBroker,
		Meta: &cluster.MetaCommand{Broker: &cluster.BrokerInfo{NodeID: "n1", StreamAddr: "n1:9200"}},
	})
	f.must(t, &cluster.RaftCommand{
		Type: cluster.CmdMetaCreateTopic,
		Meta: &cluster.MetaCommand{
			TopicMeta:  &cluster.TopicMeta{Name: "chat", Partitions: 2, ReplicationFactor: 1},
			Placements: [][]string{{"n1"}, {"n1"}},
		},
	})
	f.must(t, &cluster.RaftCommand{
		Type: cluster.CmdMetaAssignLeader,
		Meta: &cluster.MetaCommand{Topic: "chat", Partition: 0, Leader: "n1", LeaderEpoch: 4},
	})

	r := New(slog, f.meta(), f, nil, Config{NodeID: "n1"})
	r.Reconcile()

	topic, err := slog.Topic("chat")
	if err != nil {
		t.Fatalf("topic not created: %v", err)
	}
	p, err := topic.Partition(0)
	if err != nil {
		t.Fatalf("partition 0: %v", err)
	}
	if epoch, _ := p.LeaderEpoch(); epoch != 4 {
		t.Errorf("chat/0 local epoch = %d, want 4 from the assignment", epoch)
	}

	// Partition 1 was never assigned a leader, so it must not have been
	// promoted — a node that promotes what it was not given is exactly the
	// split brain the control plane exists to prevent.
	p1, err := topic.Partition(1)
	if err != nil {
		t.Fatalf("partition 1: %v", err)
	}
	if epoch, _ := p1.LeaderEpoch(); epoch != stream.UndefinedEpoch {
		t.Errorf("chat/1 epoch = %d, want unpromoted", epoch)
	}
}

// TestReconcilerFollowsEpochAdvances: each new term must open locally, or
// records written after a failover would carry the previous leader's epoch and
// be indistinguishable from records the cluster abandoned.
func TestReconcilerFollowsEpochAdvances(t *testing.T) {
	f := newMetaFixture(t)
	slog := openTestLog(t)

	f.must(t, &cluster.RaftCommand{
		Type: cluster.CmdMetaRegisterBroker,
		Meta: &cluster.MetaCommand{Broker: &cluster.BrokerInfo{NodeID: "n1"}},
	})
	f.must(t, &cluster.RaftCommand{
		Type: cluster.CmdMetaCreateTopic,
		Meta: &cluster.MetaCommand{
			TopicMeta:  &cluster.TopicMeta{Name: "chat", Partitions: 1, ReplicationFactor: 1},
			Placements: [][]string{{"n1"}},
		},
	})

	r := New(slog, f.meta(), f, nil, Config{NodeID: "n1"})

	for _, epoch := range []uint32{1, 2, 9} {
		f.must(t, &cluster.RaftCommand{
			Type: cluster.CmdMetaAssignLeader,
			Meta: &cluster.MetaCommand{Topic: "chat", Partition: 0, Leader: "n1", LeaderEpoch: epoch},
		})
		r.Reconcile()

		topic, _ := slog.Topic("chat")
		p, _ := topic.Partition(0)
		if got, _ := p.LeaderEpoch(); got != epoch {
			t.Fatalf("local epoch = %d after assignment at %d", got, epoch)
		}
	}
}

// TestReconcilerIsIdempotent: reconcile runs on every metadata event and on a
// timer. Re-promoting an already-open term would be rejected by the log and
// logged as an error on every tick.
func TestReconcilerIsIdempotent(t *testing.T) {
	f := newMetaFixture(t)
	slog := openTestLog(t)

	f.must(t, &cluster.RaftCommand{
		Type: cluster.CmdMetaRegisterBroker,
		Meta: &cluster.MetaCommand{Broker: &cluster.BrokerInfo{NodeID: "n1"}},
	})
	f.must(t, &cluster.RaftCommand{
		Type: cluster.CmdMetaCreateTopic,
		Meta: &cluster.MetaCommand{
			TopicMeta:  &cluster.TopicMeta{Name: "chat", Partitions: 1, ReplicationFactor: 1},
			Placements: [][]string{{"n1"}},
		},
	})
	f.must(t, &cluster.RaftCommand{
		Type: cluster.CmdMetaAssignLeader,
		Meta: &cluster.MetaCommand{Topic: "chat", Partition: 0, Leader: "n1", LeaderEpoch: 3},
	})

	r := New(slog, f.meta(), f, nil, Config{NodeID: "n1"})
	for i := 0; i < 5; i++ {
		r.Reconcile()
	}

	topic, _ := slog.Topic("chat")
	p, _ := topic.Partition(0)
	if got, _ := p.LeaderEpoch(); got != 3 {
		t.Errorf("epoch = %d after repeated reconciles, want a stable 3", got)
	}
}

// TestReconcilerIgnoresOtherNodesPartitions: a node must only promote what it
// was assigned. This is the check that a copy-paste of the node ID into the
// wrong comparison would break.
func TestReconcilerIgnoresOtherNodesPartitions(t *testing.T) {
	f := newMetaFixture(t)
	slog := openTestLog(t)

	for _, id := range []string{"n1", "n2"} {
		f.must(t, &cluster.RaftCommand{
			Type: cluster.CmdMetaRegisterBroker,
			Meta: &cluster.MetaCommand{Broker: &cluster.BrokerInfo{NodeID: id}},
		})
	}
	f.must(t, &cluster.RaftCommand{
		Type: cluster.CmdMetaCreateTopic,
		Meta: &cluster.MetaCommand{
			TopicMeta:  &cluster.TopicMeta{Name: "chat", Partitions: 1, ReplicationFactor: 2},
			Placements: [][]string{{"n2", "n1"}},
		},
	})
	f.must(t, &cluster.RaftCommand{
		Type: cluster.CmdMetaAssignLeader,
		Meta: &cluster.MetaCommand{Topic: "chat", Partition: 0, Leader: "n2", LeaderEpoch: 1},
	})

	r := New(slog, f.meta(), f, nil, Config{NodeID: "n1"})
	r.Reconcile()

	if topic, err := slog.Topic("chat"); err == nil {
		p, _ := topic.Partition(0)
		if epoch, _ := p.LeaderEpoch(); epoch != stream.UndefinedEpoch {
			t.Errorf("n1 promoted chat/0 at epoch %d, but n2 leads it", epoch)
		}
	}
}
