package streamctl

import (
	"errors"
	"testing"
	"time"

	"github.com/boltq/boltq/internal/cluster"
	"github.com/boltq/boltq/internal/stream"
)

// These tests cover the split-brain hole that per-partition leadership opens.
//
// Before a control plane existed, a node was either the single writer or a
// read-only follower, and nothing had to check. Once leadership is granted per
// partition and a gateway runs on every data node, a client can reach a node
// that hosts a partition but does not lead it — and without a guard, that node
// happily assigns its own sequence numbers to the same partition the real
// leader is writing. Two logs, one name, and epoch fencing cannot tell them
// apart afterwards because each write carries an epoch its own writer believed.

func setupLeadershipFixture(t *testing.T) (*metaFixture, *stream.Log) {
	t.Helper()
	f := newMetaFixture(t)
	slog := openTestLog(t)

	for _, id := range []string{"n1", "n2"} {
		f.must(t, &cluster.RaftCommand{
			Type: cluster.CmdMetaRegisterBroker,
			Meta: &cluster.MetaCommand{Broker: &cluster.BrokerInfo{NodeID: id, StreamAddr: id + ":9200"}},
		})
	}
	f.must(t, &cluster.RaftCommand{
		Type: cluster.CmdMetaCreateTopic,
		Meta: &cluster.MetaCommand{
			TopicMeta:  &cluster.TopicMeta{Name: "chat", Partitions: 2, ReplicationFactor: 2},
			Placements: [][]string{{"n1", "n2"}, {"n2", "n1"}},
		},
	})
	return f, slog
}

func appendTo(slog *stream.Log, topic string, partition int32, body string) error {
	t, err := slog.GetOrCreateTopic(topic)
	if err != nil {
		return err
	}
	p, err := t.Partition(partition)
	if err != nil {
		return err
	}
	_, err = p.Append(&stream.Record{Payload: []byte(body)})
	return err
}

// TestWriteRejectedOnAPartitionThisNodeDoesNotLead is the hole itself. A node
// holding a replica must refuse local writes, or two nodes assign the same
// sequence to different records.
func TestWriteRejectedOnAPartitionThisNodeDoesNotLead(t *testing.T) {
	f, slog := setupLeadershipFixture(t)

	// n1 leads partition 0; n2 leads partition 1.
	f.must(t, &cluster.RaftCommand{
		Type: cluster.CmdMetaAssignLeader,
		Meta: &cluster.MetaCommand{Topic: "chat", Partition: 0, Leader: "n1", LeaderEpoch: 1},
	})
	f.must(t, &cluster.RaftCommand{
		Type: cluster.CmdMetaAssignLeader,
		Meta: &cluster.MetaCommand{Topic: "chat", Partition: 1, Leader: "n2", LeaderEpoch: 1},
	})

	r := New(slog, f.meta(), f, nil, Config{NodeID: "n1"})
	r.Start()
	defer r.Close()
	r.Reconcile()

	if err := appendTo(slog, "chat", 0, "mine"); err != nil {
		t.Errorf("write to the partition this node leads was rejected: %v", err)
	}

	err := appendTo(slog, "chat", 1, "not mine")
	if err == nil {
		t.Fatal("write accepted on a partition led by another node — this is a split brain")
	}
	if !errors.Is(err, stream.ErrNotPartitionLeader) {
		t.Errorf("error = %v, want ErrNotPartitionLeader so callers can route to the leader", err)
	}
}

// TestWriteRejectedAfterLosingLeadership: leadership moves. A node that keeps
// writing after the controller reassigned its partition is the same divergence,
// arriving a few seconds later.
func TestWriteRejectedAfterLosingLeadership(t *testing.T) {
	f, slog := setupLeadershipFixture(t)

	f.must(t, &cluster.RaftCommand{
		Type: cluster.CmdMetaAssignLeader,
		Meta: &cluster.MetaCommand{Topic: "chat", Partition: 0, Leader: "n1", LeaderEpoch: 1},
	})

	r := New(slog, f.meta(), f, nil, Config{NodeID: "n1"})
	r.Start()
	defer r.Close()
	r.Reconcile()

	if err := appendTo(slog, "chat", 0, "while leader"); err != nil {
		t.Fatalf("setup: write as leader failed: %v", err)
	}

	// The controller hands the partition to n2.
	f.must(t, &cluster.RaftCommand{
		Type: cluster.CmdMetaAssignLeader,
		Meta: &cluster.MetaCommand{Topic: "chat", Partition: 0, Leader: "n2", LeaderEpoch: 2},
	})
	r.Reconcile()

	if err := appendTo(slog, "chat", 0, "after demotion"); !errors.Is(err, stream.ErrNotPartitionLeader) {
		t.Errorf("write after demotion returned %v, want ErrNotPartitionLeader", err)
	}
}

// TestLeadershipRegainedAfterReassignment: the guard must not be a one-way
// door. A partition that comes back has to be writable again, or a failover and
// failback leaves it permanently read-only.
func TestLeadershipRegainedAfterReassignment(t *testing.T) {
	f, slog := setupLeadershipFixture(t)

	r := New(slog, f.meta(), f, nil, Config{NodeID: "n1"})
	r.Start()
	defer r.Close()

	for _, step := range []struct {
		leader string
		epoch  uint32
	}{{"n1", 1}, {"n2", 2}, {"n1", 3}} {
		f.must(t, &cluster.RaftCommand{
			Type: cluster.CmdMetaAssignLeader,
			Meta: &cluster.MetaCommand{
				Topic: "chat", Partition: 0, Leader: step.leader, LeaderEpoch: step.epoch,
			},
		})
		r.Reconcile()

		err := appendTo(slog, "chat", 0, "probe")
		if step.leader == "n1" && err != nil {
			t.Errorf("epoch %d: n1 leads but the write failed: %v", step.epoch, err)
		}
		if step.leader != "n1" && err == nil {
			t.Errorf("epoch %d: n1 does not lead but the write was accepted", step.epoch)
		}
	}
}

// TestEnforcementIsOffWithoutAControlPlane: a standalone node has exactly one
// writer by construction. Enforcing there would reject every legitimate append
// and break single-node deployments outright.
func TestEnforcementIsOffWithoutAControlPlane(t *testing.T) {
	slog := openTestLog(t)

	if err := appendTo(slog, "chat", 0, "standalone"); err != nil {
		t.Errorf("standalone write rejected: %v", err)
	}
}

// TestEnforcementCoversTopicsCreatedLater: a partition placed on this node by a
// rebalance, or a topic created at runtime, must start guarded. Otherwise there
// is a window where a brand-new replica is writable by a node that does not
// lead it.
func TestEnforcementCoversTopicsCreatedLater(t *testing.T) {
	f, slog := setupLeadershipFixture(t)

	r := New(slog, f.meta(), f, nil, Config{NodeID: "n1"})
	r.Start()
	defer r.Close()
	r.Reconcile()

	// A topic this node has never been given leadership of.
	if _, err := slog.GetOrCreateTopic("rooms"); err != nil {
		t.Fatalf("create topic: %v", err)
	}
	if err := appendTo(slog, "rooms", 0, "unassigned"); !errors.Is(err, stream.ErrNotPartitionLeader) {
		t.Errorf("write to an unassigned new topic returned %v, want ErrNotPartitionLeader", err)
	}
}

// TestReplicationIsUnaffectedByTheGuard: followers write through
// AppendReplicated with sequences the leader assigned. Blocking that path would
// stop replication entirely — the guard is about *local* writes only.
func TestReplicationIsUnaffectedByTheGuard(t *testing.T) {
	f, slog := setupLeadershipFixture(t)

	r := New(slog, f.meta(), f, nil, Config{NodeID: "n1"})
	r.Start()
	defer r.Close()
	r.Reconcile()

	topic, err := slog.GetOrCreateTopic("chat")
	if err != nil {
		t.Fatalf("topic: %v", err)
	}
	p, err := topic.Partition(1) // led by n2, replicated here
	if err != nil {
		t.Fatalf("partition: %v", err)
	}

	// Sequence 0 is reserved as "unset" on the replicated path, so a real
	// follower's first record is 1.
	rec := &stream.Record{Seq: 1, Epoch: 1, Payload: []byte("replicated"), Timestamp: time.Now().UnixNano()}
	if err := p.AppendReplicated(rec); err != nil {
		t.Errorf("replicated append was blocked by the leadership guard: %v", err)
	}
}
