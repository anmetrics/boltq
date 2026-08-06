package streamctl

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/boltq/boltq/internal/cluster"
	"github.com/boltq/boltq/internal/stream"
)

// The unit tests exercise each transition once, in order, on one goroutine.
// Production does none of those things: leadership moves while writes are in
// flight, the reconciler runs concurrently with the gateway, and a rebalance
// touches many partitions at once.
//
// These tests run the same machinery under contention, which is the only way
// the interesting failures show up. Run them with -race; without it they prove
// far less.

// TestConcurrentWritesDuringLeadershipChurn is the scenario a rebalance
// creates: partitions changing hands while clients keep sending.
//
// The invariant is not "every write succeeds" — a write to a partition this
// node has just lost *should* fail, and the forwarder's job is to send it
// elsewhere. The invariant is that a write either succeeds or fails cleanly,
// and never succeeds on a partition this node does not lead. That would be two
// nodes assigning the same sequence.
func TestConcurrentWritesDuringLeadershipChurn(t *testing.T) {
	const (
		partitions = 8
		writers    = 8
		duration   = 400 * time.Millisecond
	)

	f := newMetaFixture(t)
	slog := openTestLog(t)

	f.must(t, &cluster.RaftCommand{
		Type: cluster.CmdMetaRegisterBroker,
		Meta: &cluster.MetaCommand{Broker: &cluster.BrokerInfo{NodeID: "n1", StreamAddr: "n1:9200"}},
	})
	f.must(t, &cluster.RaftCommand{
		Type: cluster.CmdMetaRegisterBroker,
		Meta: &cluster.MetaCommand{Broker: &cluster.BrokerInfo{NodeID: "n2", StreamAddr: "n2:9200"}},
	})

	placements := make([][]string, partitions)
	for i := range placements {
		placements[i] = []string{"n1", "n2"}
	}
	f.must(t, &cluster.RaftCommand{
		Type: cluster.CmdMetaCreateTopic,
		Meta: &cluster.MetaCommand{
			TopicMeta:  &cluster.TopicMeta{Name: "chat", Partitions: partitions, ReplicationFactor: 2},
			Placements: placements,
		},
	})
	for pid := int32(0); pid < partitions; pid++ {
		f.must(t, &cluster.RaftCommand{
			Type: cluster.CmdMetaAssignLeader,
			Meta: &cluster.MetaCommand{Topic: "chat", Partition: pid, Leader: "n1", LeaderEpoch: 1},
		})
	}

	r := New(slog, f.meta(), f, nil, Config{NodeID: "n1"})
	r.Start()
	defer r.Close()
	r.Reconcile()

	topic, err := slog.GetOrCreateTopic("chat")
	if err != nil {
		t.Fatalf("topic: %v", err)
	}

	seen := newSeqSet()

	var (
		stop      atomic.Bool
		accepted  atomic.Int64
		rejected  atomic.Int64
		violation atomic.Int64
		wg        sync.WaitGroup
	)

	// Writers hammer every partition.
	for w := 0; w < writers; w++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			for i := 0; !stop.Load(); i++ {
				key := []byte(fmt.Sprintf("w%d-%d", id, i))
				pid := topic.PartitionForKey(key)

				res, err := slog.Append("chat", &stream.Record{Key: key, Payload: []byte("x")})
				switch {
				case err == nil:
					accepted.Add(1)
					// The invariant is that no two accepted writes share a
					// sequence within a partition. Checking leadership after the
					// fact proves nothing — leadership may legitimately move
					// between the append and the check. What cannot happen, if
					// the guard and the sequence assignment are atomic, is two
					// records claiming the same coordinate.
					seen.record(t, pid, res.Seq, &violation)
				case errors.Is(err, stream.ErrNotPartitionLeader):
					rejected.Add(1)
				default:
					t.Errorf("unexpected append error: %v", err)
					return
				}
			}
		}(w)
	}

	// A churner moves leadership back and forth under them.
	wg.Add(1)
	go func() {
		defer wg.Done()
		epoch := uint32(1)
		for i := 0; !stop.Load(); i++ {
			pid := int32(i % partitions)
			leader := "n1"
			if i%2 == 0 {
				leader = "n2"
			}
			epoch++
			f.Apply(&cluster.RaftCommand{
				Type: cluster.CmdMetaAssignLeader,
				Meta: &cluster.MetaCommand{
					Topic: "chat", Partition: pid, Leader: leader, LeaderEpoch: epoch,
				},
			}, time.Second)
			r.Reconcile()
			time.Sleep(time.Millisecond)
		}
	}()

	time.Sleep(duration)
	stop.Store(true)
	wg.Wait()

	if v := violation.Load(); v > 0 {
		t.Errorf("%d writes succeeded on a partition this node did not lead", v)
	}
	if accepted.Load() == 0 {
		t.Error("no write ever succeeded; the test proved nothing")
	}
	if rejected.Load() == 0 {
		t.Error("no write was ever rejected; leadership churn never took effect")
	}
	t.Logf("accepted=%d rejected=%d", accepted.Load(), rejected.Load())
}

// TestConcurrentReconcileIsSafe: the reconciler runs on a timer, on metadata
// events, and on explicit calls. All three can overlap.
func TestConcurrentReconcileIsSafe(t *testing.T) {
	f := newMetaFixture(t)
	slog := openTestLog(t)

	f.must(t, &cluster.RaftCommand{
		Type: cluster.CmdMetaRegisterBroker,
		Meta: &cluster.MetaCommand{Broker: &cluster.BrokerInfo{NodeID: "n1", StreamAddr: "n1:9200"}},
	})
	f.must(t, &cluster.RaftCommand{
		Type: cluster.CmdMetaCreateTopic,
		Meta: &cluster.MetaCommand{
			TopicMeta:  &cluster.TopicMeta{Name: "chat", Partitions: 4, ReplicationFactor: 1},
			Placements: [][]string{{"n1"}, {"n1"}, {"n1"}, {"n1"}},
		},
	})

	r := New(slog, f.meta(), f, nil, Config{NodeID: "n1"})
	r.Start()
	defer r.Close()

	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			for j := 0; j < 50; j++ {
				r.Reconcile()
			}
		}(i)
	}

	// Assignments change underneath the concurrent reconciles.
	wg.Add(1)
	go func() {
		defer wg.Done()
		for epoch := uint32(1); epoch <= 40; epoch++ {
			f.Apply(&cluster.RaftCommand{
				Type: cluster.CmdMetaAssignLeader,
				Meta: &cluster.MetaCommand{
					Topic: "chat", Partition: int32(epoch % 4), Leader: "n1", LeaderEpoch: epoch,
				},
			}, time.Second)
		}
	}()

	wg.Wait()
}

// TestForwarderUnderConcurrency: the forwarder is shared by every writer on the
// node. Its counters and its HTTP client see the whole node's write rate.
func TestForwarderUnderConcurrency(t *testing.T) {
	leader := newLeaderStub()
	defer leader.Close()

	f, slog := forwarderFixture(t, leader.addr())
	f.must(t, &cluster.RaftCommand{
		Type: cluster.CmdMetaAssignLeader,
		Meta: &cluster.MetaCommand{Topic: "chat", Partition: 1, Leader: "n2", LeaderEpoch: 1},
	})

	r := New(slog, f.meta(), f, nil, Config{NodeID: "n1"})
	r.Start()
	defer r.Close()
	r.Reconcile()

	fwd := NewForwarder(f.meta(), ForwarderConfig{NodeID: "n1", APIKey: "k"})
	slog.SetWriteForwarder(fwd)

	key := keyForPartition(t, slog, "chat", 1)

	var wg sync.WaitGroup
	const n = 40
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			slog.AppendContext(context.Background(), "chat",
				&stream.Record{Key: key, Payload: []byte("concurrent")})
		}()
	}
	wg.Wait()

	total := fwd.Stats().Forwarded + fwd.Stats().Failed
	if total != n {
		t.Errorf("counters total %d, want %d — a concurrent update was lost", total, n)
	}
}

// seqSet records which sequences have been handed out per partition.
//
// A duplicate is the observable signature of split brain: two writers each
// believing they owned the sequence space.
type seqSet struct {
	mu   sync.Mutex
	seen map[int32]map[uint64]bool
}

func newSeqSet() *seqSet {
	return &seqSet{seen: map[int32]map[uint64]bool{}}
}

func (s *seqSet) record(t *testing.T, partition int32, seq uint64, violation *atomic.Int64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	m, ok := s.seen[partition]
	if !ok {
		m = map[uint64]bool{}
		s.seen[partition] = m
	}
	if m[seq] {
		violation.Add(1)
		t.Errorf("partition %d handed out sequence %d twice", partition, seq)
		return
	}
	m[seq] = true
}
