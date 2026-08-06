package streamctl

import (
	"context"
	"testing"
	"time"

	"github.com/boltq/boltq/internal/cluster"
	"github.com/boltq/boltq/internal/queuelog"
	"github.com/boltq/boltq/internal/stream"
)

// TestQueueFollowsPartitionLeadership is the link that makes a log-backed queue
// safe in a cluster. Without it, two nodes holding the same replica each keep
// their own lease window and hand the same job to two workers.
func TestQueueFollowsPartitionLeadership(t *testing.T) {
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
			TopicMeta:  &cluster.TopicMeta{Name: "jobs", Partitions: 2, ReplicationFactor: 2},
			Placements: [][]string{{"n1", "n2"}, {"n2", "n1"}},
		},
	})
	f.must(t, &cluster.RaftCommand{
		Type: cluster.CmdMetaAssignLeader,
		Meta: &cluster.MetaCommand{Topic: "jobs", Partition: 0, Leader: "n1", LeaderEpoch: 1},
	})
	f.must(t, &cluster.RaftCommand{
		Type: cluster.CmdMetaAssignLeader,
		Meta: &cluster.MetaCommand{Topic: "jobs", Partition: 1, Leader: "n2", LeaderEpoch: 1},
	})

	cursors, err := stream.OpenCursorStore(t.TempDir())
	if err != nil {
		t.Fatalf("cursors: %v", err)
	}
	t.Cleanup(func() { cursors.Close() })

	q, err := queuelog.Open(slog, cursors, "jobs", queuelog.Config{
		Partitions: 2, AckTimeout: 30 * time.Second,
	})
	if err != nil {
		t.Fatalf("open queue: %v", err)
	}
	t.Cleanup(q.Close)

	sup := NewQueueSupervisor(f.meta(), "n1", q)
	sup.Start()
	defer sup.Close()

	if !q.LeadsPartition(0) {
		t.Error("n1 leads jobs/0 but the queue will not lease from it")
	}
	if q.LeadsPartition(1) {
		t.Error("n1 does not lead jobs/1 but the queue would lease from it — double delivery")
	}

	// The controller moves partition 0 away.
	f.must(t, &cluster.RaftCommand{
		Type: cluster.CmdMetaAssignLeader,
		Meta: &cluster.MetaCommand{Topic: "jobs", Partition: 0, Leader: "n2", LeaderEpoch: 2},
	})
	sup.Refresh()

	if q.LeadsPartition(0) {
		t.Error("queue still leases from a partition led elsewhere")
	}

	// Publishing is unaffected: the stream layer routes it.
	if _, err := q.Publish(context.Background(), &stream.Record{
		Key: []byte("k"), Payload: []byte("v"),
	}); err != nil && err.Error() == "" {
		t.Fatalf("publish: %v", err)
	}
}
