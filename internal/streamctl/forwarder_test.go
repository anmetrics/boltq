package streamctl

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/boltq/boltq/internal/cluster"
	"github.com/boltq/boltq/internal/stream"
)

// leaderStub stands in for the peer that leads a partition. It records what it
// was asked to write, which is the only way to prove the write actually left
// this node rather than being quietly dropped.
type leaderStub struct {
	*httptest.Server
	// The handler runs on one goroutine per request, so everything it records
	// needs a lock. Without it the stub itself races under a concurrent test —
	// and a racy stub reports failures that belong to the test, not the code.
	mu      sync.Mutex
	got     []ForwardRequest
	nextSeq uint64
	status  int
	apiKeys []string
}

// requests returns a copy of what the stub was asked to write.
func (s *leaderStub) requests() []ForwardRequest {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]ForwardRequest(nil), s.got...)
}

func newLeaderStub() *leaderStub {
	s := &leaderStub{nextSeq: 41}
	mux := http.NewServeMux()
	mux.HandleFunc("/internal/append", func(w http.ResponseWriter, r *http.Request) {
		var req ForwardRequest
		json.NewDecoder(r.Body).Decode(&req)

		s.mu.Lock()
		s.apiKeys = append(s.apiKeys, r.Header.Get("X-API-Key"))
		s.got = append(s.got, req)
		status := s.status
		s.nextSeq++
		seq := s.nextSeq
		s.mu.Unlock()

		if status != 0 && status != http.StatusOK {
			w.WriteHeader(status)
			return
		}
		json.NewEncoder(w).Encode(ForwardResponse{
			Topic: req.Topic, Partition: req.Partition,
			Seq: seq, Timestamp: 1700000000,
		})
	})
	s.Server = httptest.NewServer(mux)
	return s
}

// addr returns the host:port form the metadata records.
func (s *leaderStub) addr() string { return strings.TrimPrefix(s.URL, "http://") }

// keyForPartition finds a key that hashes to the given partition, using the
// same function clients use. Hardcoding a key would silently test the wrong
// partition the moment the partition count changes.
func keyForPartition(t *testing.T, slog *stream.Log, topic string, want int32) []byte {
	t.Helper()
	tp, err := slog.GetOrCreateTopic(topic)
	if err != nil {
		t.Fatalf("topic: %v", err)
	}
	for i := 0; i < 1000; i++ {
		k := []byte(fmt.Sprintf("conv-%d", i))
		if tp.PartitionForKey(k) == want {
			return k
		}
	}
	t.Fatalf("no key found for %s/%d", topic, want)
	return nil
}

func forwarderFixture(t *testing.T, leaderAddr string) (*metaFixture, *stream.Log) {
	t.Helper()
	f := newMetaFixture(t)
	slog := openTestLog(t)

	f.must(t, &cluster.RaftCommand{
		Type: cluster.CmdMetaRegisterBroker,
		Meta: &cluster.MetaCommand{Broker: &cluster.BrokerInfo{
			NodeID: "n1", StreamAddr: "n1:9200", AdminAddr: "n1:9090",
		}},
	})
	f.must(t, &cluster.RaftCommand{
		Type: cluster.CmdMetaRegisterBroker,
		Meta: &cluster.MetaCommand{Broker: &cluster.BrokerInfo{
			NodeID: "n2", StreamAddr: "n2:9200", AdminAddr: leaderAddr,
		}},
	})
	f.must(t, &cluster.RaftCommand{
		Type: cluster.CmdMetaCreateTopic,
		Meta: &cluster.MetaCommand{
			TopicMeta:  &cluster.TopicMeta{Name: "chat", Partitions: 2, ReplicationFactor: 2},
			Placements: [][]string{{"n1", "n2"}, {"n2", "n1"}},
		},
	})
	return f, slog
}

// TestWriteToARemotePartitionIsForwarded is the whole point: a message sent to
// the wrong node must still reach the user it was addressed to. Before routing,
// enforcement turned this case into an error the sender saw.
func TestWriteToARemotePartitionIsForwarded(t *testing.T) {
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
	res, err := slog.AppendContext(context.Background(), "chat",
		&stream.Record{Key: key, Payload: []byte("hello")})
	if err != nil {
		t.Fatalf("append that should have been forwarded failed: %v", err)
	}

	if len(leader.got) == 0 {
		t.Fatal("the leader received nothing — the write never left this node")
	}
	if string(leader.got[0].Payload) != "hello" {
		t.Errorf("leader received payload %q", leader.got[0].Payload)
	}
	// The sequence must be the leader's, not a local invention: the sender's
	// local echo has to resolve to the coordinates every other client sees.
	if res.Seq != 42 {
		t.Errorf("seq = %d, want the leader's 42", res.Seq)
	}
	if leader.apiKeys[0] != "k" {
		t.Errorf("forwarded without the API key: %q", leader.apiKeys[0])
	}
	if fwd.Stats().Forwarded != 1 {
		t.Errorf("Forwarded = %d, want 1", fwd.Stats().Forwarded)
	}
}

// TestLocalWriteIsNotForwarded: routing must be the exception. Forwarding a
// write this node can perform would double the latency of the common case and
// put a network hop in the hot path for no reason.
func TestLocalWriteIsNotForwarded(t *testing.T) {
	leader := newLeaderStub()
	defer leader.Close()

	f, slog := forwarderFixture(t, leader.addr())
	f.must(t, &cluster.RaftCommand{
		Type: cluster.CmdMetaAssignLeader,
		Meta: &cluster.MetaCommand{Topic: "chat", Partition: 0, Leader: "n1", LeaderEpoch: 1},
	})

	r := New(slog, f.meta(), f, nil, Config{NodeID: "n1"})
	r.Start()
	defer r.Close()
	r.Reconcile()

	fwd := NewForwarder(f.meta(), ForwarderConfig{NodeID: "n1"})
	slog.SetWriteForwarder(fwd)

	key := keyForPartition(t, slog, "chat", 0) // partition 0 is led here

	if _, err := slog.AppendContext(context.Background(), "chat",
		&stream.Record{Key: key, Payload: []byte("local")}); err != nil {
		t.Fatalf("local write failed: %v", err)
	}

	if len(leader.got) != 0 {
		t.Errorf("a write this node leads was forwarded anyway: %+v", leader.got)
	}
	if fwd.Stats().Forwarded != 0 {
		t.Errorf("Forwarded = %d, want 0", fwd.Stats().Forwarded)
	}
}

// TestForwardFailsWhenPartitionIsOffline: with no leader, the honest answer is
// an error. Picking a replica here would be an unclean election made by the
// write path, which is the one place least equipped to decide it.
func TestForwardFailsWhenPartitionIsOffline(t *testing.T) {
	leader := newLeaderStub()
	defer leader.Close()

	f, slog := forwarderFixture(t, leader.addr())
	// No leader is ever assigned to partition 1.

	r := New(slog, f.meta(), f, nil, Config{NodeID: "n1"})
	r.Start()
	defer r.Close()
	r.Reconcile()

	fwd := NewForwarder(f.meta(), ForwarderConfig{NodeID: "n1"})
	slog.SetWriteForwarder(fwd)

	_, err := slog.AppendContext(context.Background(), "chat",
		&stream.Record{Key: keyForPartition(t, slog, "chat", 1), Payload: []byte("nowhere")})
	if err == nil {
		t.Fatal("write to an offline partition was accepted")
	}
	if fwd.Stats().NoLeader == 0 {
		t.Error("NoLeader counter did not move")
	}
}

// TestForwardSurfacesPeerRejection: if the peer has also been demoted, the
// write must fail rather than be bounced onward. Two nodes forwarding to each
// other is a loop that ends in a stack of timeouts.
func TestForwardSurfacesPeerRejection(t *testing.T) {
	leader := newLeaderStub()
	leader.status = http.StatusMisdirectedRequest
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

	fwd := NewForwarder(f.meta(), ForwarderConfig{NodeID: "n1"})
	slog.SetWriteForwarder(fwd)

	_, err := slog.AppendContext(context.Background(), "chat",
		&stream.Record{Key: keyForPartition(t, slog, "chat", 1), Payload: []byte("bounce")})
	if err == nil {
		t.Fatal("a peer rejection was reported as success")
	}
	if fwd.Stats().Failed == 0 {
		t.Error("Failed counter did not move")
	}
	if len(leader.got) != 1 {
		t.Errorf("peer was called %d times; a rejection must not be retried in a loop", len(leader.got))
	}
}

// TestReadsAreServedLocally is the reason forwarding stays cheap. A replica
// holds the data, so only writes travel — in a chat workload reads outnumber
// writes by the fan-out factor, often a hundredfold.
func TestReadsAreServedLocally(t *testing.T) {
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

	fwd := NewForwarder(f.meta(), ForwarderConfig{NodeID: "n1"})
	slog.SetWriteForwarder(fwd)

	// A replica would hold records written by the leader; simulate one arriving
	// through replication.
	topic, _ := slog.GetOrCreateTopic("chat")
	p, _ := topic.Partition(1)
	if err := p.AppendReplicated(&stream.Record{
		Seq: 1, Epoch: 1, Payload: []byte("from the leader"), Timestamp: 1700000000,
	}); err != nil {
		t.Fatalf("replicated append: %v", err)
	}

	recs, err := slog.Read("chat", 1, 1, 10)
	if err != nil {
		t.Fatalf("local read of a followed partition failed: %v", err)
	}
	if len(recs) == 0 {
		t.Fatal("read returned nothing from a partition this node replicates")
	}
	if len(leader.got) != 0 {
		t.Errorf("a read was forwarded: %+v", leader.got)
	}
}
