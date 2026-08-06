package replication

import (
	"context"
	"errors"
	"fmt"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/boltq/boltq/internal/stream"
)

// --- helpers ---

func freeAddr(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("reserve port: %v", err)
	}
	addr := ln.Addr().String()
	ln.Close()
	return addr
}

func newLog(t *testing.T) *stream.Log {
	t.Helper()
	l, err := stream.OpenLog(stream.LogConfig{
		Dir:          t.TempDir(),
		DefaultTopic: stream.TopicConfig{Partitions: 2},
	})
	if err != nil {
		t.Fatalf("open log: %v", err)
	}
	t.Cleanup(func() { l.Close() })
	return l
}

type cluster struct {
	leaderLog *stream.Log
	leader    *Leader
	addr      string
}

func startLeader(t *testing.T, cfg LeaderConfig) *cluster {
	t.Helper()
	l := newLog(t)

	if cfg.Addr == "" {
		cfg.Addr = freeAddr(t)
	}
	if cfg.NodeID == "" {
		cfg.NodeID = "leader-1"
	}
	ld, err := NewLeader(l, cfg)
	if err != nil {
		t.Fatalf("new leader: %v", err)
	}
	if err := ld.Start(); err != nil {
		t.Fatalf("start leader: %v", err)
	}
	t.Cleanup(ld.Close)

	return &cluster{leaderLog: l, leader: ld, addr: cfg.Addr}
}

func startFollower(t *testing.T, c *cluster, nodeID string, cfg FollowerConfig) (*Follower, *stream.Log) {
	t.Helper()
	fl := newLog(t)

	cfg.LeaderAddr = c.addr
	cfg.NodeID = nodeID
	if cfg.AckInterval == 0 {
		cfg.AckInterval = 20 * time.Millisecond
	}
	if cfg.ReconnectBackoff == 0 {
		cfg.ReconnectBackoff = 50 * time.Millisecond
	}
	if len(cfg.Assignments) == 0 {
		cfg.Assignments = []Assignment{{Topic: "chat", Partition: 0, PartitionCount: 2}}
	}

	f, err := NewFollower(fl, cfg)
	if err != nil {
		t.Fatalf("new follower: %v", err)
	}
	f.Start()
	t.Cleanup(f.Close)
	return f, fl
}

func waitFor(t *testing.T, what string, timeout time.Duration, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func appendN(t *testing.T, l *stream.Log, topic, key string, n int) []uint64 {
	t.Helper()
	seqs := make([]uint64, 0, n)
	for i := 0; i < n; i++ {
		res, err := l.Append(topic, &stream.Record{
			Key:     []byte(key),
			Payload: []byte(fmt.Sprintf("msg-%d", i)),
			Headers: map[string]string{"i": fmt.Sprint(i)},
		})
		if err != nil {
			t.Fatalf("append %d: %v", i, err)
		}
		seqs = append(seqs, res.Seq)
	}
	return seqs
}

// readAll returns every record in a partition.
func readAll(t *testing.T, l *stream.Log, topic string, partition int32) []*stream.Record {
	t.Helper()
	recs, err := l.Read(topic, partition, 1, 10000)
	if err != nil && !errors.Is(err, stream.ErrNoSuchTopic) {
		t.Fatalf("read %s/%d: %v", topic, partition, err)
	}
	return recs
}

// --- protocol codec ---

func TestFetchRequestRoundTrip(t *testing.T) {
	in := fetchRequest{Topic: "chat.direct.alice:bob", PartitionCount: 16, Partition: 7, FromSeq: 90210}
	got, err := decodeFetch(in.encode())
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got != in {
		t.Errorf("round trip gave %+v, want %+v", got, in)
	}
}

func TestAckMessageRoundTrip(t *testing.T) {
	in := ackMessage{Topic: "chat.inbox.bob", Partition: 3, Seq: 4242}
	got, err := decodeAck(in.encode())
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got != in {
		t.Errorf("round trip gave %+v, want %+v", got, in)
	}
}

func TestCodecRejectsMalformed(t *testing.T) {
	if _, err := decodeFetch([]byte{1, 2, 3}); err == nil {
		t.Error("short fetch accepted")
	}
	if _, err := decodeAck([]byte{1, 2, 3}); err == nil {
		t.Error("short ack accepted")
	}
	// A topic length that disagrees with the payload must be rejected rather
	// than slicing out of range.
	bad := fetchRequest{Topic: "chat", PartitionCount: 1}.encode()
	bad[16] = 0xFF
	if _, err := decodeFetch(bad); err == nil {
		t.Error("fetch with a lying topic length accepted")
	}
	if _, _, err := decodeRecordsHeader([]byte{1, 2}); err == nil {
		t.Error("short records header accepted")
	}
}

func TestFrameRoundTrip(t *testing.T) {
	var buf syncBuffer
	payload := []byte("hello replication")
	if err := writeFrame(&buf, MsgRecords, payload); err != nil {
		t.Fatalf("write: %v", err)
	}
	typ, got, err := readFrame(&buf)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if typ != MsgRecords || string(got) != string(payload) {
		t.Errorf("got type=%#x payload=%q", typ, got)
	}
}

func TestFrameRejectsOversized(t *testing.T) {
	var buf syncBuffer
	if err := writeFrame(&buf, MsgRecords, make([]byte, MaxFramePayload+1)); !errors.Is(err, ErrFrameTooLarge) {
		t.Errorf("oversized write: %v", err)
	}
}

// syncBuffer is a tiny in-memory ReadWriter for codec tests.
type syncBuffer struct {
	mu  sync.Mutex
	buf []byte
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.buf = append(b.buf, p...)
	return len(p), nil
}

func (b *syncBuffer) Read(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if len(b.buf) == 0 {
		return 0, fmt.Errorf("EOF")
	}
	n := copy(p, b.buf)
	b.buf = b.buf[n:]
	return n, nil
}

// --- Record codec across the wire ---

func TestRecordCodecPreservesEverything(t *testing.T) {
	orig := &stream.Record{
		Seq: 42, Timestamp: 1700000000000000000,
		Key:     []byte("conv-1"),
		Headers: map[string]string{"sender": "alice", "kind": "direct"},
		Payload: []byte{0x00, 0xFF, 0x7F, 0x80},
	}
	frame, err := stream.EncodeRecord(orig)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	got, err := stream.DecodeRecord(frame)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.Seq != orig.Seq || got.Timestamp != orig.Timestamp {
		t.Errorf("header lost: %+v", got)
	}
	if string(got.Key) != string(orig.Key) || string(got.Payload) != string(orig.Payload) {
		t.Errorf("body lost: key=%q payload=%v", got.Key, got.Payload)
	}
	if got.Headers["sender"] != "alice" {
		t.Errorf("headers lost: %v", got.Headers)
	}
}

func TestDecodeRecordRejectsCorruption(t *testing.T) {
	frame, _ := stream.EncodeRecord(&stream.Record{Seq: 1, Payload: []byte("x")})

	bad := append([]byte(nil), frame...)
	bad[len(bad)-1] ^= 0xFF // flip a payload bit; CRC must catch it
	if _, err := stream.DecodeRecord(bad); err == nil {
		t.Error("a corrupted record decoded successfully")
	}
	if _, err := stream.DecodeRecord(frame[:4]); err == nil {
		t.Error("a truncated frame decoded successfully")
	}
}

// --- Replication behaviour ---

func TestRecordsReachFollower(t *testing.T) {
	c := startLeader(t, LeaderConfig{})
	// Assign both partitions: which one a key lands in is a property of the
	// hash, and a test that assumes partition 0 would pass or fail by accident.
	_, followerLog := startFollower(t, c, "follower-1", FollowerConfig{
		Assignments: []Assignment{
			{Topic: "chat", Partition: 0, PartitionCount: 2},
			{Topic: "chat", Partition: 1, PartitionCount: 2},
		},
	})

	waitFor(t, "follower to connect", 3*time.Second, func() bool {
		return len(c.leader.Followers()) == 1
	})

	c.leaderLog.CreateTopic("chat", stream.TopicConfig{Partitions: 2})
	appendN(t, c.leaderLog, "chat", "conv-a", 25)

	leaderRecs := readAll(t, c.leaderLog, "chat", partitionOf(t, c.leaderLog, "chat", "conv-a"))
	if len(leaderRecs) == 0 {
		t.Fatal("nothing on the leader")
	}

	waitFor(t, "records to replicate", 5*time.Second, func() bool {
		return len(readAll(t, followerLog, "chat", 0))+len(readAll(t, followerLog, "chat", 1)) >= 25
	})

	// The follower's copy must be byte-identical, in the same order, with the
	// same sequences — otherwise it is not a replica.
	for pid := int32(0); pid < 2; pid++ {
		lead := readAll(t, c.leaderLog, "chat", pid)
		follow := readAll(t, followerLog, "chat", pid)
		if len(lead) != len(follow) {
			t.Fatalf("partition %d: leader has %d records, follower has %d", pid, len(lead), len(follow))
		}
		for i := range lead {
			if lead[i].Seq != follow[i].Seq {
				t.Fatalf("partition %d record %d: leader seq %d, follower seq %d",
					pid, i, lead[i].Seq, follow[i].Seq)
			}
			if string(lead[i].Payload) != string(follow[i].Payload) {
				t.Fatalf("partition %d record %d payload differs", pid, i)
			}
			if lead[i].Timestamp != follow[i].Timestamp {
				t.Fatalf("partition %d record %d timestamp differs", pid, i)
			}
		}
	}
}

// partitionOf resolves which partition a key routes to.
func partitionOf(t *testing.T, l *stream.Log, topic, key string) int32 {
	t.Helper()
	tp, err := l.Topic(topic)
	if err != nil {
		t.Fatalf("topic: %v", err)
	}
	return tp.PartitionForKey([]byte(key))
}

func TestFollowerReplicatesAllPartitions(t *testing.T) {
	c := startLeader(t, LeaderConfig{})
	_, followerLog := startFollower(t, c, "follower-1", FollowerConfig{
		Assignments: []Assignment{
			{Topic: "chat", Partition: 0, PartitionCount: 2},
			{Topic: "chat", Partition: 1, PartitionCount: 2},
		},
	})
	waitFor(t, "follower", 3*time.Second, func() bool { return len(c.leader.Followers()) == 1 })

	c.leaderLog.CreateTopic("chat", stream.TopicConfig{Partitions: 2})
	// Many distinct keys, so both partitions get traffic.
	total := 40
	for i := 0; i < total; i++ {
		c.leaderLog.Append("chat", &stream.Record{
			Key:     []byte(fmt.Sprintf("conv-%d", i)),
			Payload: []byte(fmt.Sprintf("m%d", i)),
		})
	}

	waitFor(t, "all partitions to replicate", 5*time.Second, func() bool {
		return len(readAll(t, followerLog, "chat", 0))+len(readAll(t, followerLog, "chat", 1)) == total
	})

	for pid := int32(0); pid < 2; pid++ {
		lead := readAll(t, c.leaderLog, "chat", pid)
		follow := readAll(t, followerLog, "chat", pid)
		if len(lead) != len(follow) {
			t.Errorf("partition %d: leader has %d, follower has %d", pid, len(lead), len(follow))
		}
	}
}

func TestFollowerCatchesUpFromExistingHistory(t *testing.T) {
	c := startLeader(t, LeaderConfig{})

	// History exists before any follower connects.
	c.leaderLog.CreateTopic("chat", stream.TopicConfig{Partitions: 1})
	appendN(t, c.leaderLog, "chat", "conv-a", 60)

	_, followerLog := startFollower(t, c, "late-joiner", FollowerConfig{
		Assignments: []Assignment{{Topic: "chat", Partition: 0, PartitionCount: 1}},
	})

	waitFor(t, "backfill", 5*time.Second, func() bool {
		return len(readAll(t, followerLog, "chat", 0)) == 60
	})

	got := readAll(t, followerLog, "chat", 0)
	for i, r := range got {
		if r.Seq != uint64(i+1) {
			t.Fatalf("backfilled record %d has seq %d — order or completeness broken", i, r.Seq)
		}
	}
}

func TestFollowerResumesAfterRestart(t *testing.T) {
	c := startLeader(t, LeaderConfig{})
	c.leaderLog.CreateTopic("chat", stream.TopicConfig{Partitions: 1})

	followerLog := newLog(t)
	assignments := []Assignment{{Topic: "chat", Partition: 0, PartitionCount: 1}}

	f1, err := NewFollower(followerLog, FollowerConfig{
		LeaderAddr: c.addr, NodeID: "f1", Assignments: assignments,
		AckInterval: 20 * time.Millisecond,
	})
	if err != nil {
		t.Fatalf("follower: %v", err)
	}
	f1.Start()

	appendN(t, c.leaderLog, "chat", "conv-a", 30)
	waitFor(t, "first batch", 5*time.Second, func() bool {
		return len(readAll(t, followerLog, "chat", 0)) == 30
	})
	f1.Close()

	// More records arrive while the follower is down.
	appendN(t, c.leaderLog, "chat", "conv-a", 20)

	f2, err := NewFollower(followerLog, FollowerConfig{
		LeaderAddr: c.addr, NodeID: "f1", Assignments: assignments,
		AckInterval: 20 * time.Millisecond,
	})
	if err != nil {
		t.Fatalf("follower 2: %v", err)
	}
	f2.Start()
	defer f2.Close()

	waitFor(t, "resume", 5*time.Second, func() bool {
		return len(readAll(t, followerLog, "chat", 0)) == 50
	})

	// Resuming must not duplicate what was already applied.
	got := readAll(t, followerLog, "chat", 0)
	for i, r := range got {
		if r.Seq != uint64(i+1) {
			t.Fatalf("record %d has seq %d — restart duplicated or skipped records", i, r.Seq)
		}
	}
}

// --- Quorum acknowledgement ---

func TestWaitForReturnsImmediatelyWithMinInSyncOne(t *testing.T) {
	c := startLeader(t, LeaderConfig{MinInSync: 1})

	start := time.Now()
	if err := c.leader.WaitFor(context.Background(), "chat", 0, 12345); err != nil {
		t.Fatalf("WaitFor: %v", err)
	}
	if elapsed := time.Since(start); elapsed > 200*time.Millisecond {
		t.Errorf("min_in_sync=1 waited %v — it must not wait at all", elapsed)
	}
}

func TestWaitForFailsFastWithoutEnoughFollowers(t *testing.T) {
	c := startLeader(t, LeaderConfig{MinInSync: 2, AckTimeout: 5 * time.Second})

	start := time.Now()
	err := c.leader.WaitFor(context.Background(), "chat", 0, 1)
	if !errors.Is(err, stream.ErrNotEnoughReplicas) {
		t.Fatalf("got %v, want ErrNotEnoughReplicas", err)
	}
	// It must fail fast rather than block every publisher for the full timeout
	// on a cluster that simply lacks nodes.
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Errorf("failed after %v — should be immediate", elapsed)
	}
}

func TestWaitForSucceedsOnceFollowerAcks(t *testing.T) {
	c := startLeader(t, LeaderConfig{MinInSync: 2, AckTimeout: 5 * time.Second})
	_, followerLog := startFollower(t, c, "follower-1", FollowerConfig{
		Assignments: []Assignment{{Topic: "chat", Partition: 0, PartitionCount: 1}},
	})
	waitFor(t, "follower", 3*time.Second, func() bool { return len(c.leader.Followers()) == 1 })

	c.leaderLog.CreateTopic("chat", stream.TopicConfig{Partitions: 1})
	c.leaderLog.SetAckWaiter(c.leader)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	res, err := c.leaderLog.AppendContext(ctx, "chat", &stream.Record{
		Key: []byte("conv-a"), Payload: []byte("durable"),
	})
	if err != nil {
		t.Fatalf("AppendContext with quorum: %v", err)
	}

	// A successful quorum append means the follower genuinely holds it.
	waitFor(t, "record on follower", 3*time.Second, func() bool {
		recs := readAll(t, followerLog, "chat", 0)
		return len(recs) >= 1 && recs[0].Seq == res.Seq
	})
}

func TestWaitForTimesOutWhenFollowerStalls(t *testing.T) {
	c := startLeader(t, LeaderConfig{MinInSync: 2, AckTimeout: 300 * time.Millisecond})

	// A connection that completes the handshake and then never acks — exactly
	// what a wedged follower looks like.
	conn, err := net.Dial("tcp", c.addr)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()
	writeFrame(conn, MsgHello, []byte("silent-follower\x00"))

	waitFor(t, "leader to register the follower", 3*time.Second, func() bool {
		return len(c.leader.Followers()) == 1
	})

	err = c.leader.WaitFor(context.Background(), "chat", 0, 1)
	if !errors.Is(err, stream.ErrAckTimeout) {
		t.Fatalf("got %v, want ErrAckTimeout", err)
	}
	if s := c.leader.Stats(); s.AckTimeouts == 0 {
		t.Error("timeout was not counted")
	}
}

func TestWaitForRespectsContextCancellation(t *testing.T) {
	c := startLeader(t, LeaderConfig{MinInSync: 2, AckTimeout: 30 * time.Second})

	conn, err := net.Dial("tcp", c.addr)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()
	writeFrame(conn, MsgHello, []byte("silent\x00"))
	waitFor(t, "follower", 3*time.Second, func() bool { return len(c.leader.Followers()) == 1 })

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()

	if err := c.leader.WaitFor(ctx, "chat", 0, 1); !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("got %v, want the context deadline", err)
	}
}

func TestFollowerDisconnectStopsCountingTowardQuorum(t *testing.T) {
	c := startLeader(t, LeaderConfig{MinInSync: 2, AckTimeout: 300 * time.Millisecond})
	f, _ := startFollower(t, c, "follower-1", FollowerConfig{
		Assignments: []Assignment{{Topic: "chat", Partition: 0, PartitionCount: 1}},
	})
	waitFor(t, "follower", 3*time.Second, func() bool { return len(c.leader.Followers()) == 1 })

	c.leaderLog.CreateTopic("chat", stream.TopicConfig{Partitions: 1})
	appendN(t, c.leaderLog, "chat", "conv-a", 5)

	waitFor(t, "acks", 3*time.Second, func() bool {
		return c.leader.WaitFor(context.Background(), "chat", 0, 1) == nil
	})

	f.Close()
	waitFor(t, "leader to notice the disconnect", 3*time.Second, func() bool {
		return len(c.leader.Followers()) == 0
	})

	// With the replica gone, writes must stop claiming replicated durability.
	err := c.leader.WaitFor(context.Background(), "chat", 0, 99)
	if !errors.Is(err, stream.ErrNotEnoughReplicas) {
		t.Errorf("after disconnect got %v, want ErrNotEnoughReplicas", err)
	}
}

// --- Authentication ---

func TestUnauthenticatedFollowerRejected(t *testing.T) {
	c := startLeader(t, LeaderConfig{Secret: "s3cret"})

	conn, err := net.Dial("tcp", c.addr)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()

	writeFrame(conn, MsgHello, []byte("intruder\x00wrong-secret"))
	typ, payload, err := readFrame(conn)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if typ != MsgError {
		t.Fatalf("got type %#x, want an error", typ)
	}
	if string(payload) != "unauthorized" {
		t.Errorf("payload = %q", payload)
	}
	if s := c.leader.Stats(); s.AuthFailures == 0 {
		t.Error("auth failure not counted")
	}
	if len(c.leader.Followers()) != 0 {
		t.Error("an unauthenticated peer was registered as a follower")
	}
}

func TestCorrectSecretAccepted(t *testing.T) {
	c := startLeader(t, LeaderConfig{Secret: "s3cret"})
	_, followerLog := startFollower(t, c, "follower-1", FollowerConfig{
		Secret:      "s3cret",
		Assignments: []Assignment{{Topic: "chat", Partition: 0, PartitionCount: 1}},
	})
	waitFor(t, "follower", 3*time.Second, func() bool { return len(c.leader.Followers()) == 1 })

	c.leaderLog.CreateTopic("chat", stream.TopicConfig{Partitions: 1})
	appendN(t, c.leaderLog, "chat", "conv-a", 3)
	waitFor(t, "replication", 3*time.Second, func() bool {
		return len(readAll(t, followerLog, "chat", 0)) == 3
	})
}

func TestEmptyNodeIDRejected(t *testing.T) {
	c := startLeader(t, LeaderConfig{})

	conn, err := net.Dial("tcp", c.addr)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()

	writeFrame(conn, MsgHello, []byte("\x00"))
	typ, _, err := readFrame(conn)
	if err == nil && typ != MsgError {
		t.Errorf("a follower with no node ID was accepted (type %#x)", typ)
	}
}

// --- Replication state reporting ---

func TestReplicationState(t *testing.T) {
	c := startLeader(t, LeaderConfig{MinInSync: 2})
	startFollower(t, c, "follower-1", FollowerConfig{
		Assignments: []Assignment{{Topic: "chat", Partition: 0, PartitionCount: 1}},
	})
	waitFor(t, "follower", 3*time.Second, func() bool { return len(c.leader.Followers()) == 1 })

	c.leaderLog.CreateTopic("chat", stream.TopicConfig{Partitions: 1})
	appendN(t, c.leaderLog, "chat", "conv-a", 10)

	waitFor(t, "replica state to populate", 3*time.Second, func() bool {
		return len(c.leader.Replication("chat", 0).Replicas) == 1
	})

	st := c.leader.Replication("chat", 0)
	if st.LeaderSeq != 11 {
		t.Errorf("LeaderSeq = %d, want 11", st.LeaderSeq)
	}
	if st.Replicas[0].NodeID != "follower-1" {
		t.Errorf("replica = %+v", st.Replicas[0])
	}
	// leader + one follower
	if st.InSync != 2 {
		t.Errorf("InSync = %d, want 2", st.InSync)
	}

	waitFor(t, "high watermark to advance", 3*time.Second, func() bool {
		return c.leader.Replication("chat", 0).HighWatermark >= 10
	})
}

func TestQuorumPosition(t *testing.T) {
	cases := []struct {
		leaderSeq uint64
		acked     map[string]uint64
		minInSync int
		want      uint64
	}{
		{100, nil, 1, 100},                                   // leader alone
		{100, map[string]uint64{"a": 90}, 2, 90},             // slowest of two
		{100, map[string]uint64{"a": 90, "b": 80}, 2, 90},    // best follower counts
		{100, map[string]uint64{"a": 90, "b": 80}, 3, 80},    // all three needed
		{100, map[string]uint64{"a": 90}, 3, 0},              // not enough replicas
		{100, map[string]uint64{"a": 100, "b": 100}, 3, 100}, // everyone caught up
	}
	for _, c := range cases {
		if got := quorumPosition(c.leaderSeq, c.acked, c.minInSync); got != c.want {
			t.Errorf("quorumPosition(%d, %v, %d) = %d, want %d",
				c.leaderSeq, c.acked, c.minInSync, got, c.want)
		}
	}
}

// --- Failure handling ---

func TestFollowerReconnectsAfterLeaderRestart(t *testing.T) {
	addr := freeAddr(t)
	leaderLog := newLog(t)
	leaderLog.CreateTopic("chat", stream.TopicConfig{Partitions: 1})

	ld1, err := NewLeader(leaderLog, LeaderConfig{Addr: addr, NodeID: "leader-1"})
	if err != nil {
		t.Fatalf("leader: %v", err)
	}
	if err := ld1.Start(); err != nil {
		t.Fatalf("start: %v", err)
	}

	followerLog := newLog(t)
	f, err := NewFollower(followerLog, FollowerConfig{
		LeaderAddr: addr, NodeID: "f1",
		Assignments:      []Assignment{{Topic: "chat", Partition: 0, PartitionCount: 1}},
		AckInterval:      20 * time.Millisecond,
		ReconnectBackoff: 50 * time.Millisecond,
	})
	if err != nil {
		t.Fatalf("follower: %v", err)
	}
	f.Start()
	defer f.Close()

	appendN(t, leaderLog, "chat", "conv-a", 10)
	waitFor(t, "first replication", 5*time.Second, func() bool {
		return len(readAll(t, followerLog, "chat", 0)) == 10
	})

	// The leader goes away and comes back on the same address.
	ld1.Close()
	time.Sleep(100 * time.Millisecond)

	ld2, err := NewLeader(leaderLog, LeaderConfig{Addr: addr, NodeID: "leader-1"})
	if err != nil {
		t.Fatalf("leader 2: %v", err)
	}
	if err := ld2.Start(); err != nil {
		t.Fatalf("restart: %v", err)
	}
	defer ld2.Close()

	appendN(t, leaderLog, "chat", "conv-a", 10)

	waitFor(t, "replication to resume", 10*time.Second, func() bool {
		return len(readAll(t, followerLog, "chat", 0)) == 20
	})

	got := readAll(t, followerLog, "chat", 0)
	for i, r := range got {
		if r.Seq != uint64(i+1) {
			t.Fatalf("record %d has seq %d after leader restart", i, r.Seq)
		}
	}
}

func TestConcurrentAppendsReplicateInOrder(t *testing.T) {
	c := startLeader(t, LeaderConfig{})
	_, followerLog := startFollower(t, c, "follower-1", FollowerConfig{
		Assignments: []Assignment{{Topic: "chat", Partition: 0, PartitionCount: 1}},
	})
	waitFor(t, "follower", 3*time.Second, func() bool { return len(c.leader.Followers()) == 1 })

	c.leaderLog.CreateTopic("chat", stream.TopicConfig{Partitions: 1})

	const writers, each = 6, 50
	var wg sync.WaitGroup
	for w := 0; w < writers; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			for i := 0; i < each; i++ {
				c.leaderLog.Append("chat", &stream.Record{
					Key:     []byte("conv-a"),
					Payload: []byte(fmt.Sprintf("w%d-%d", w, i)),
				})
			}
		}(w)
	}
	wg.Wait()

	total := writers * each
	waitFor(t, "all records", 10*time.Second, func() bool {
		return len(readAll(t, followerLog, "chat", 0)) == total
	})

	lead := readAll(t, c.leaderLog, "chat", 0)
	follow := readAll(t, followerLog, "chat", 0)
	if len(lead) != len(follow) {
		t.Fatalf("leader %d records, follower %d", len(lead), len(follow))
	}
	// The follower's order must be exactly the leader's, record for record.
	for i := range lead {
		if lead[i].Seq != follow[i].Seq || string(lead[i].Payload) != string(follow[i].Payload) {
			t.Fatalf("divergence at %d: leader seq=%d %q, follower seq=%d %q",
				i, lead[i].Seq, lead[i].Payload, follow[i].Seq, follow[i].Payload)
		}
	}
}

func TestTwoFollowers(t *testing.T) {
	c := startLeader(t, LeaderConfig{MinInSync: 3, AckTimeout: 5 * time.Second})
	assignments := []Assignment{{Topic: "chat", Partition: 0, PartitionCount: 1}}
	_, logA := startFollower(t, c, "f-a", FollowerConfig{Assignments: assignments})
	_, logB := startFollower(t, c, "f-b", FollowerConfig{Assignments: assignments})

	waitFor(t, "both followers", 5*time.Second, func() bool {
		return len(c.leader.Followers()) == 2
	})

	c.leaderLog.CreateTopic("chat", stream.TopicConfig{Partitions: 1})
	c.leaderLog.SetAckWaiter(c.leader)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	for i := 0; i < 10; i++ {
		if _, err := c.leaderLog.AppendContext(ctx, "chat", &stream.Record{
			Key: []byte("conv-a"), Payload: []byte(fmt.Sprint(i)),
		}); err != nil {
			t.Fatalf("append %d with min_in_sync=3: %v", i, err)
		}
	}

	// A successful append at min_in_sync=3 means both followers hold it.
	for name, l := range map[string]*stream.Log{"f-a": logA, "f-b": logB} {
		if got := len(readAll(t, l, "chat", 0)); got != 10 {
			t.Errorf("follower %s has %d records, want 10", name, got)
		}
	}
	if st := c.leader.Replication("chat", 0); st.InSync != 3 {
		t.Errorf("InSync = %d, want 3 (leader + 2)", st.InSync)
	}
}

func TestLeaderStats(t *testing.T) {
	c := startLeader(t, LeaderConfig{})
	startFollower(t, c, "follower-1", FollowerConfig{
		Assignments: []Assignment{{Topic: "chat", Partition: 0, PartitionCount: 1}},
	})
	waitFor(t, "follower", 3*time.Second, func() bool { return len(c.leader.Followers()) == 1 })

	c.leaderLog.CreateTopic("chat", stream.TopicConfig{Partitions: 1})
	appendN(t, c.leaderLog, "chat", "conv-a", 20)

	waitFor(t, "stats to reflect shipping", 5*time.Second, func() bool {
		s := c.leader.Stats()
		return s.RecordsShipped >= 20 && s.AcksReceived > 0 && s.BytesShipped > 0
	})
	if s := c.leader.Stats(); s.FollowersConnected != 1 {
		t.Errorf("FollowersConnected = %d", s.FollowersConnected)
	}
}

func TestFollowerStats(t *testing.T) {
	c := startLeader(t, LeaderConfig{})
	f, _ := startFollower(t, c, "follower-1", FollowerConfig{
		Assignments: []Assignment{{Topic: "chat", Partition: 0, PartitionCount: 1}},
	})
	waitFor(t, "connect", 3*time.Second, func() bool { return f.Stats().Connected })

	c.leaderLog.CreateTopic("chat", stream.TopicConfig{Partitions: 1})
	appendN(t, c.leaderLog, "chat", "conv-a", 15)

	waitFor(t, "applied", 5*time.Second, func() bool {
		return f.Stats().RecordsApplied >= 15
	})
	s := f.Stats()
	if s.LeaderNodeID != "leader-1" {
		t.Errorf("LeaderNodeID = %q", s.LeaderNodeID)
	}
	if s.BatchesApplied == 0 {
		t.Error("BatchesApplied is zero")
	}
	if s.LastAppliedAt.IsZero() {
		t.Error("LastAppliedAt not set")
	}
}

func TestNewValidatesConfig(t *testing.T) {
	l := newLog(t)

	if _, err := NewLeader(nil, LeaderConfig{}); err == nil {
		t.Error("NewLeader accepted a nil log")
	}
	if _, err := NewFollower(nil, FollowerConfig{LeaderAddr: "x", NodeID: "y"}); err == nil {
		t.Error("NewFollower accepted a nil log")
	}
	if _, err := NewFollower(l, FollowerConfig{NodeID: "y"}); err == nil {
		t.Error("NewFollower accepted an empty leader address")
	}
	if _, err := NewFollower(l, FollowerConfig{LeaderAddr: "x"}); err == nil {
		t.Error("NewFollower accepted an empty node ID")
	}

	ld, _ := NewLeader(l, LeaderConfig{})
	if err := ld.Start(); err == nil {
		t.Error("Start succeeded with no listen address")
	}
}

func TestCloseIsIdempotent(t *testing.T) {
	c := startLeader(t, LeaderConfig{})
	f, _ := startFollower(t, c, "f1", FollowerConfig{})

	f.Close()
	f.Close()
	c.leader.Close()
	c.leader.Close()
}
