package queuelog

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/boltq/boltq/internal/stream"
)

// fakeLeadership lets a test move partition leadership the way the controller
// would, without standing up consensus.
type fakeLeadership struct {
	mu   sync.Mutex
	owns map[int32]bool
}

func newFakeLeadership(owned ...int32) *fakeLeadership {
	f := &fakeLeadership{owns: map[int32]bool{}}
	for _, p := range owned {
		f.owns[p] = true
	}
	return f
}

func (f *fakeLeadership) LeadsPartition(_ string, partition int32) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.owns[partition]
}

func (f *fakeLeadership) set(partition int32, leads bool) {
	f.mu.Lock()
	f.owns[partition] = leads
	f.mu.Unlock()
}

func openTestQueue(t *testing.T, partitions int32) (*Queue, *stream.Log) {
	t.Helper()

	slog, err := stream.OpenLog(stream.LogConfig{
		Dir: t.TempDir(),
		DefaultTopic: stream.TopicConfig{
			Partitions: partitions,
			Partition:  stream.PartitionConfig{SegmentBytes: 1 << 20},
		},
	})
	if err != nil {
		t.Fatalf("open log: %v", err)
	}
	t.Cleanup(func() { slog.Close() })

	cursors, err := stream.OpenCursorStore(t.TempDir())
	if err != nil {
		t.Fatalf("open cursors: %v", err)
	}
	t.Cleanup(func() { cursors.Close() })

	q, err := Open(slog, cursors, "jobs", Config{
		Partitions: partitions,
		AckTimeout: 30 * time.Second,
	})
	if err != nil {
		t.Fatalf("open queue: %v", err)
	}
	t.Cleanup(q.Close)
	return q, slog
}

// publishTo appends a record that lands on the given partition.
func publishTo(t *testing.T, q *Queue, slog *stream.Log, partition int32, body string) {
	t.Helper()
	topic, err := slog.GetOrCreateTopic("jobs")
	if err != nil {
		t.Fatalf("topic: %v", err)
	}
	for i := 0; i < 2000; i++ {
		key := []byte(body + "-" + string(rune('a'+i%26)) + string(rune('0'+i/26%10)))
		if topic.PartitionForKey(key) != partition {
			continue
		}
		if _, err := q.Publish(context.Background(), &stream.Record{
			Key: key, Payload: []byte(body),
		}); err != nil {
			t.Fatalf("publish: %v", err)
		}
		return
	}
	t.Fatalf("no key maps to partition %d", partition)
}

// TestLeaseOnlyFromLedPartitions is the double-delivery guard. Two nodes both
// holding a replica would each keep their own lease window over the same
// records and hand the same job to two workers, with neither learning of the
// other's ack.
func TestLeaseOnlyFromLedPartitions(t *testing.T) {
	q, slog := openTestQueue(t, 4)

	publishTo(t, q, slog, 0, "for-p0")
	publishTo(t, q, slog, 2, "for-p2")

	// This node leads partition 0 only.
	q.EnforceLeadership(newFakeLeadership(0))

	got, err := q.TryConsume("worker-1", 10)
	if err != nil {
		t.Fatalf("consume: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("leased %d records, want only the one on the led partition", len(got))
	}
	if got[0].Partition != 0 {
		t.Errorf("leased from partition %d, which this node does not lead", got[0].Partition)
	}
}

// TestLeaseReleasedOnLosingLeadership: leases held after leadership moves are
// records two workers could be processing, because the new leader hands them out
// from its own view. Dropping them immediately bounds the overlap to a reconcile
// interval instead of an ack timeout.
func TestLeaseReleasedOnLosingLeadership(t *testing.T) {
	q, slog := openTestQueue(t, 2)
	publishTo(t, q, slog, 0, "job")

	lead := newFakeLeadership(0)
	q.EnforceLeadership(lead)

	got, err := q.TryConsume("worker-1", 10)
	if err != nil || len(got) != 1 {
		t.Fatalf("setup: leased %d records, err=%v", len(got), err)
	}

	// The controller moves partition 0 elsewhere.
	lead.set(0, false)
	q.RefreshLeadership()

	if q.LeadsPartition(0) {
		t.Fatal("still reporting leadership of a partition that moved")
	}
	stats := q.Stats()
	if stats.InFlight != 0 {
		t.Errorf("%d leases still held after losing leadership", stats.InFlight)
	}

	// And no new lease may be taken.
	if got, _ := q.TryConsume("worker-2", 10); len(got) != 0 {
		t.Errorf("leased %d records from a partition led elsewhere", len(got))
	}
}

// TestAckRejectedAfterLosingLeadership: an ack for a partition served elsewhere
// refers to a lease that no longer exists here. Succeeding silently would tell
// the consumer its work was retired when it is about to be redelivered.
func TestAckRejectedAfterLosingLeadership(t *testing.T) {
	q, slog := openTestQueue(t, 2)
	publishTo(t, q, slog, 0, "job")

	lead := newFakeLeadership(0)
	q.EnforceLeadership(lead)

	got, err := q.TryConsume("worker-1", 1)
	if err != nil || len(got) != 1 {
		t.Fatalf("setup: %v", err)
	}
	d := got[0]

	lead.set(0, false)
	q.RefreshLeadership()

	if err := q.Ack("worker-1", d.Partition, d.Seq); !errors.Is(err, ErrNotQueueLeader) {
		t.Errorf("ack returned %v, want ErrNotQueueLeader", err)
	}
	if err := q.Nack("worker-1", d.Partition, d.Seq, true); !errors.Is(err, ErrNotQueueLeader) {
		t.Errorf("nack returned %v, want ErrNotQueueLeader", err)
	}
}

// TestLeadershipRegained: the gate must not be one-way, or a failover and
// failback leaves the partition permanently unservable.
func TestLeadershipRegained(t *testing.T) {
	q, slog := openTestQueue(t, 2)
	publishTo(t, q, slog, 0, "job")

	lead := newFakeLeadership(0)
	q.EnforceLeadership(lead)

	lead.set(0, false)
	q.RefreshLeadership()
	if got, _ := q.TryConsume("w", 1); len(got) != 0 {
		t.Fatal("leased while not leading")
	}

	lead.set(0, true)
	q.RefreshLeadership()

	got, err := q.TryConsume("w", 1)
	if err != nil {
		t.Fatalf("consume after regaining leadership: %v", err)
	}
	if len(got) != 1 {
		t.Errorf("leased %d records after regaining leadership, want 1", len(got))
	}
}

// TestUngatedWithoutAControlPlane: a single-node queue has exactly one server.
// A gate there would refuse every legitimate lease and break the standalone
// deployment outright.
func TestUngatedWithoutAControlPlane(t *testing.T) {
	q, slog := openTestQueue(t, 2)
	publishTo(t, q, slog, 0, "job")
	publishTo(t, q, slog, 1, "job")

	got, err := q.TryConsume("worker", 10)
	if err != nil {
		t.Fatalf("consume: %v", err)
	}
	if len(got) != 2 {
		t.Errorf("leased %d records without a control plane, want both", len(got))
	}
}

// TestPublishIsNotGated: publishing goes through the stream log, which has its
// own leadership guard and its own forwarding. Gating it here as well would
// reject writes the log would have routed correctly.
func TestPublishIsNotGated(t *testing.T) {
	q, slog := openTestQueue(t, 2)
	q.EnforceLeadership(newFakeLeadership()) // leads nothing

	topic, _ := slog.GetOrCreateTopic("jobs")
	_ = topic
	if _, err := q.Publish(context.Background(), &stream.Record{
		Key: []byte("k"), Payload: []byte("v"),
	}); err != nil {
		t.Errorf("publish was blocked by the queue's lease gate: %v", err)
	}
}
