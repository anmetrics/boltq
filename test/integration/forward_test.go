package integration

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/boltq/boltq/internal/api"
	"github.com/boltq/boltq/internal/broker"
	"github.com/boltq/boltq/internal/cluster"
	"github.com/boltq/boltq/internal/config"
	"github.com/boltq/boltq/internal/metrics"
	"github.com/boltq/boltq/internal/stream"
	"github.com/boltq/boltq/internal/streamctl"
)

// This test exists because the unit test for forwarding stubs the receiving
// side, and a stub proves only that the sender is self-consistent. Here both
// ends are the real thing: a real stream.Log, the real HTTP handler, the real
// forwarder, and a real network round trip between them.
//
// The failure it is built to catch is the one a stub can never catch — the two
// sides disagreeing about the wire. A field renamed on one end, a status code
// interpreted differently, a payload that encodes but does not decode.

// realNode is one BoltQ node's write path: its log and its admin HTTP server.
type realNode struct {
	id     string
	log    *stream.Log
	server *httptest.Server
}

func (n *realNode) addr() string { return strings.TrimPrefix(n.server.URL, "http://") }

func newRealNode(t *testing.T, id, apiKey string) *realNode {
	t.Helper()

	slog, err := stream.OpenLog(stream.LogConfig{
		Dir: t.TempDir(),
		DefaultTopic: stream.TopicConfig{
			Partitions: 2,
			Partition:  stream.PartitionConfig{SegmentBytes: 1 << 20},
		},
	})
	if err != nil {
		t.Fatalf("open log for %s: %v", id, err)
	}
	t.Cleanup(func() { slog.Close() })

	b := broker.New(broker.Config{})
	t.Cleanup(func() { b.Close() })

	h := api.NewHTTPServer(b, metrics.Global(), config.ServerConfig{}, apiKey)
	h.SetStreamLog(slog)

	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)

	return &realNode{id: id, log: slog, server: srv}
}

// buildMetadata records both nodes and a topic whose two partitions are led by
// different nodes — the arrangement that forces a forward.
func buildMetadata(t *testing.T, n1, n2 *realNode) *cluster.MetadataStore {
	t.Helper()
	fsm := cluster.NewBrokerFSM(nil)

	apply := func(cmd *cluster.RaftCommand) {
		t.Helper()
		if resp := fsm.ApplyCommand(cmd); resp.Error != nil {
			t.Fatalf("apply %d: %v", cmd.Type, resp.Error)
		}
	}

	for _, n := range []*realNode{n1, n2} {
		apply(&cluster.RaftCommand{
			Type: cluster.CmdMetaRegisterBroker,
			Meta: &cluster.MetaCommand{Broker: &cluster.BrokerInfo{
				NodeID:     n.id,
				AdminAddr:  n.addr(),
				StreamAddr: n.id + ":9200",
			}},
		})
	}
	apply(&cluster.RaftCommand{
		Type: cluster.CmdMetaCreateTopic,
		Meta: &cluster.MetaCommand{
			TopicMeta:  &cluster.TopicMeta{Name: "chat", Partitions: 2, ReplicationFactor: 2},
			Placements: [][]string{{n1.id, n2.id}, {n2.id, n1.id}},
		},
	})
	apply(&cluster.RaftCommand{
		Type: cluster.CmdMetaAssignLeader,
		Meta: &cluster.MetaCommand{Topic: "chat", Partition: 0, Leader: n1.id, LeaderEpoch: 1},
	})
	apply(&cluster.RaftCommand{
		Type: cluster.CmdMetaAssignLeader,
		Meta: &cluster.MetaCommand{Topic: "chat", Partition: 1, Leader: n2.id, LeaderEpoch: 1},
	})
	return fsm.Metadata()
}

// promote opens the local leadership term for the partitions a node leads, the
// way the reconciler does.
func promote(t *testing.T, n *realNode, meta *cluster.MetadataStore) {
	t.Helper()
	n.log.EnforceLeadership(true)
	for _, a := range meta.LedBy(n.id) {
		if _, err := n.log.GetOrCreateTopic(a.Topic); err != nil {
			t.Fatalf("%s: create topic: %v", n.id, err)
		}
		if err := n.log.BecomeLeaderFor(a.Topic, a.Partition, a.LeaderEpoch); err != nil {
			t.Fatalf("%s: promote %s/%d: %v", n.id, a.Topic, a.Partition, err)
		}
	}
}

func keyFor(t *testing.T, n *realNode, topic string, want int32) []byte {
	t.Helper()
	tp, err := n.log.GetOrCreateTopic(topic)
	if err != nil {
		t.Fatalf("topic: %v", err)
	}
	for i := 0; i < 1000; i++ {
		k := []byte(string(rune('a'+i%26)) + strings.Repeat("x", i/26))
		if tp.PartitionForKey(k) == want {
			return k
		}
	}
	t.Fatalf("no key maps to %s/%d", topic, want)
	return nil
}

// TestForwardedWriteLandsOnTheRealLeader is the end-to-end proof: a write
// submitted to the node that does not lead the partition must end up durable in
// the leader's log, readable from the leader, and absent from the sender's.
func TestForwardedWriteLandsOnTheRealLeader(t *testing.T) {
	const apiKey = "integration-key"

	n1 := newRealNode(t, "n1", apiKey)
	n2 := newRealNode(t, "n2", apiKey)
	meta := buildMetadata(t, n1, n2)

	promote(t, n1, meta)
	promote(t, n2, meta)

	// n1 forwards what it may not write.
	n1.log.SetWriteForwarder(streamctl.NewForwarder(meta, streamctl.ForwarderConfig{
		NodeID: "n1", APIKey: apiKey,
	}))

	// Partition 1 is led by n2, so this write has to travel.
	key := keyFor(t, n1, "chat", 1)
	res, err := n1.log.AppendContext(context.Background(), "chat",
		&stream.Record{Key: key, Payload: []byte("crosses the wire")})
	if err != nil {
		t.Fatalf("forwarded append failed: %v", err)
	}
	if res.Partition != 1 {
		t.Fatalf("result reports partition %d, want 1", res.Partition)
	}

	// The record must be real on the leader, not merely acknowledged.
	recs, err := n2.log.Read("chat", 1, res.Seq, 10)
	if err != nil {
		t.Fatalf("read from the leader: %v", err)
	}
	if len(recs) == 0 {
		t.Fatal("the leader's log has no record at the sequence it returned")
	}
	if string(recs[0].Payload) != "crosses the wire" {
		t.Errorf("leader stored %q", recs[0].Payload)
	}

	// And it must not exist on the sender. A local copy would mean the write
	// happened twice, under two different sequence spaces.
	if local, err := n1.log.Read("chat", 1, 1, 10); err == nil && len(local) > 0 {
		t.Errorf("the forwarding node also stored the record locally: %d record(s)", len(local))
	}
}

// TestLocalWriteStaysLocal: only the partitions this node does not lead may
// travel. Forwarding the common case would put a network hop in the hot path.
func TestLocalWriteStaysLocal(t *testing.T) {
	const apiKey = "integration-key"

	n1 := newRealNode(t, "n1", apiKey)
	n2 := newRealNode(t, "n2", apiKey)
	meta := buildMetadata(t, n1, n2)

	promote(t, n1, meta)
	promote(t, n2, meta)

	fwd := streamctl.NewForwarder(meta, streamctl.ForwarderConfig{NodeID: "n1", APIKey: apiKey})
	n1.log.SetWriteForwarder(fwd)

	key := keyFor(t, n1, "chat", 0) // n1 leads partition 0
	if _, err := n1.log.AppendContext(context.Background(), "chat",
		&stream.Record{Key: key, Payload: []byte("stays home")}); err != nil {
		t.Fatalf("local append failed: %v", err)
	}

	if got := fwd.Stats().Forwarded; got != 0 {
		t.Errorf("Forwarded = %d, want 0 for a partition this node leads", got)
	}
	recs, err := n1.log.Read("chat", 0, 1, 10)
	if err != nil || len(recs) == 0 {
		t.Fatalf("local record missing: %v (%d records)", err, len(recs))
	}
}

// TestForwardIsRejectedWithoutTheAPIKey: the internal endpoint accepts writes
// with no authorisation check of its own — the forwarding peer already did
// those. The API key is the only thing standing between that endpoint and
// anyone who can reach the admin port.
func TestForwardIsRejectedWithoutTheAPIKey(t *testing.T) {
	n1 := newRealNode(t, "n1", "the-real-key")
	n2 := newRealNode(t, "n2", "the-real-key")
	meta := buildMetadata(t, n1, n2)

	promote(t, n1, meta)
	promote(t, n2, meta)

	n1.log.SetWriteForwarder(streamctl.NewForwarder(meta, streamctl.ForwarderConfig{
		NodeID: "n1", APIKey: "wrong-key",
	}))

	key := keyFor(t, n1, "chat", 1)
	_, err := n1.log.AppendContext(context.Background(), "chat",
		&stream.Record{Key: key, Payload: []byte("should not land")})
	if err == nil {
		t.Fatal("a forward with the wrong API key was accepted")
	}

	if recs, rerr := n2.log.Read("chat", 1, 1, 10); rerr == nil && len(recs) > 0 {
		t.Errorf("an unauthenticated write reached the leader's log: %d record(s)", len(recs))
	}
}

// TestPeerRejectsWhenNotLeader covers the double-failover case over a real
// connection: the destination has been demoted too, and must say so with 421
// rather than accept a write it has no right to sequence.
func TestPeerRejectsWhenNotLeader(t *testing.T) {
	const apiKey = "integration-key"

	n1 := newRealNode(t, "n1", apiKey)
	n2 := newRealNode(t, "n2", apiKey)
	meta := buildMetadata(t, n1, n2)

	promote(t, n1, meta)
	// n2 is deliberately never promoted: it hosts partition 1 but holds no term.
	n2.log.EnforceLeadership(true)
	if _, err := n2.log.GetOrCreateTopic("chat"); err != nil {
		t.Fatalf("create topic on n2: %v", err)
	}

	n1.log.SetWriteForwarder(streamctl.NewForwarder(meta, streamctl.ForwarderConfig{
		NodeID: "n1", APIKey: apiKey,
	}))

	key := keyFor(t, n1, "chat", 1)
	_, err := n1.log.AppendContext(context.Background(), "chat",
		&stream.Record{Key: key, Payload: []byte("nobody can take this")})
	if err == nil {
		t.Fatal("a write was accepted by a node holding no leadership term")
	}
	if !strings.Contains(err.Error(), "421") {
		t.Errorf("error = %v, want the peer's 421 so the caller re-resolves", err)
	}
}

// TestInternalAppendRequiresAuth checks the endpoint directly, since it is the
// one door into a node's log that carries no user identity.
func TestInternalAppendRequiresAuth(t *testing.T) {
	n := newRealNode(t, "n1", "guarded")

	req, _ := http.NewRequest(http.MethodPost, n.server.URL+"/internal/append",
		strings.NewReader(`{"topic":"chat","partition":0}`))
	req.Header.Set("Content-Type", "application/json")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("unauthenticated /internal/append returned %d, want 401", resp.StatusCode)
	}
}
