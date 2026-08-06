package replication

import (
	"testing"
	"time"

	"github.com/boltq/boltq/internal/stream"
)

func TestEpochRequestRoundTrip(t *testing.T) {
	orig := epochRequest{Topic: "chat", Partition: 3, Epoch: 12}
	got, err := decodeEpochRequest(orig.encode())
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got != orig {
		t.Fatalf("round trip = %+v, want %+v", got, orig)
	}
}

func TestEpochResponseRoundTrip(t *testing.T) {
	orig := epochResponse{Topic: "chat", Partition: 3, Epoch: 12, EndSeq: 9001}
	got, err := decodeEpochResponse(orig.encode())
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got != orig {
		t.Fatalf("round trip = %+v, want %+v", got, orig)
	}
}

func TestEpochCodecRejectsMalformed(t *testing.T) {
	if _, err := decodeEpochRequest([]byte{1, 2, 3}); err == nil {
		t.Fatal("short epoch request accepted")
	}
	if _, err := decodeEpochResponse([]byte{1, 2, 3}); err == nil {
		t.Fatal("short epoch response accepted")
	}

	// A topic length that disagrees with the frame must be refused rather than
	// read past the buffer.
	req := epochRequest{Topic: "chat", Partition: 0, Epoch: 1}.encode()
	req[8] = 99
	if _, err := decodeEpochRequest(req); err == nil {
		t.Fatal("epoch request with a lying topic length accepted")
	}
}

// The scenario the whole mechanism exists for.
//
// A follower holds records its old leader accepted but died before
// replicating. The surviving leader never had them and has since assigned the
// same sequences to different records. On reconnect the follower must discard
// its orphaned tail rather than sit at a permanent gap — or worse, keep two
// different records at one sequence.
func TestFollowerTruncatesOrphanedTailOnFailover(t *testing.T) {
	c := startLeader(t, LeaderConfig{})
	if _, err := c.leaderLog.CreateTopic("chat", stream.TopicConfig{Partitions: 1}); err != nil {
		t.Fatalf("create topic: %v", err)
	}
	if err := c.leaderLog.BecomeLeader(1); err != nil {
		t.Fatalf("leader epoch 1: %v", err)
	}

	followerLog := newLog(t)
	assignments := []Assignment{{Topic: "chat", Partition: 0, PartitionCount: 1}}
	followerCfg := FollowerConfig{
		LeaderAddr: c.addr, NodeID: "f1", Assignments: assignments,
		AckInterval: 20 * time.Millisecond,
	}

	f1, err := NewFollower(followerLog, followerCfg)
	if err != nil {
		t.Fatalf("follower: %v", err)
	}
	f1.Start()

	appendN(t, c.leaderLog, "chat", "conv-a", 5)
	waitFor(t, "initial replication", 5*time.Second, func() bool {
		return len(readAll(t, followerLog, "chat", 0)) == 5
	})
	f1.Close()

	// Give the follower a tail the leader never had: records 6 and 7, written
	// under the old leader's epoch 1 and acknowledged to nobody.
	for seq := uint64(6); seq <= 7; seq++ {
		err := followerLog.ApplyReplicated("chat", 1, 0, &stream.Record{
			Seq: seq, Epoch: 1, Payload: []byte("orphaned"),
		})
		if err != nil {
			t.Fatalf("seed orphaned record %d: %v", seq, err)
		}
	}
	if got := len(readAll(t, followerLog, "chat", 0)); got != 7 {
		t.Fatalf("follower has %d records before failover, want 7", got)
	}

	// A new leader takes over and writes its own record at sequence 6.
	if err := c.leaderLog.BecomeLeader(2); err != nil {
		t.Fatalf("leader epoch 2: %v", err)
	}
	appendN(t, c.leaderLog, "chat", "conv-a", 1)

	f2, err := NewFollower(followerLog, followerCfg)
	if err != nil {
		t.Fatalf("follower 2: %v", err)
	}
	f2.Start()
	defer f2.Close()

	waitFor(t, "truncate and re-replicate", 5*time.Second, func() bool {
		got := readAll(t, followerLog, "chat", 0)
		return len(got) == 6 && string(got[5].Payload) != "orphaned"
	})

	got := readAll(t, followerLog, "chat", 0)
	if len(got) != 6 {
		t.Fatalf("follower has %d records, want 6", len(got))
	}
	if string(got[5].Payload) == "orphaned" {
		t.Fatal("orphaned record survived; the follower and leader now disagree at sequence 6")
	}
	if got[5].Epoch != 2 {
		t.Fatalf("record 6 has epoch %d, want 2 — it did not come from the new leader", got[5].Epoch)
	}

	// The follower's own copy of the leader's history must reflect the new term,
	// or the next failover would ask the wrong question.
	leaderRecs := readAll(t, c.leaderLog, "chat", 0)
	if len(leaderRecs) != 6 {
		t.Fatalf("leader has %d records, want 6", len(leaderRecs))
	}
	for i := range got {
		if string(got[i].Payload) != string(leaderRecs[i].Payload) {
			t.Fatalf("record %d differs: follower %q, leader %q",
				i+1, got[i].Payload, leaderRecs[i].Payload)
		}
	}

	if s := f2.Stats(); s.Truncations == 0 || s.RecordsTruncated != 2 {
		t.Fatalf("stats report %d truncations / %d records, want 1+ / 2",
			s.Truncations, s.RecordsTruncated)
	}
}

// A follower whose tail is a clean prefix of the leader's must lose nothing:
// truncation is for divergence, not for every reconnect.
func TestFollowerKeepsTailWhenNoDivergence(t *testing.T) {
	c := startLeader(t, LeaderConfig{})
	if _, err := c.leaderLog.CreateTopic("chat", stream.TopicConfig{Partitions: 1}); err != nil {
		t.Fatalf("create topic: %v", err)
	}
	if err := c.leaderLog.BecomeLeader(1); err != nil {
		t.Fatalf("leader epoch: %v", err)
	}

	followerLog := newLog(t)
	assignments := []Assignment{{Topic: "chat", Partition: 0, PartitionCount: 1}}
	cfg := FollowerConfig{
		LeaderAddr: c.addr, NodeID: "f1", Assignments: assignments,
		AckInterval: 20 * time.Millisecond,
	}

	f1, _ := NewFollower(followerLog, cfg)
	f1.Start()
	appendN(t, c.leaderLog, "chat", "conv-a", 10)
	waitFor(t, "replication", 5*time.Second, func() bool {
		return len(readAll(t, followerLog, "chat", 0)) == 10
	})
	f1.Close()

	appendN(t, c.leaderLog, "chat", "conv-a", 5)

	f2, _ := NewFollower(followerLog, cfg)
	f2.Start()
	defer f2.Close()

	waitFor(t, "resume", 5*time.Second, func() bool {
		return len(readAll(t, followerLog, "chat", 0)) == 15
	})
	if s := f2.Stats(); s.Truncations != 0 {
		t.Fatalf("%d truncation(s) on a non-divergent resume; the follower discarded good data",
			s.Truncations)
	}
}

// Nodes upgrading into this version have logs with no epoch history at all.
// Reconciliation must be a no-op there rather than refusing to replicate.
func TestReplicationWorksWithoutEpochHistory(t *testing.T) {
	c := startLeader(t, LeaderConfig{})
	if _, err := c.leaderLog.CreateTopic("chat", stream.TopicConfig{Partitions: 1}); err != nil {
		t.Fatalf("create topic: %v", err)
	}
	// Deliberately no BecomeLeader: this is a pre-epoch leader.

	followerLog := newLog(t)
	cfg := FollowerConfig{
		LeaderAddr: c.addr, NodeID: "f1",
		Assignments: []Assignment{{Topic: "chat", Partition: 0, PartitionCount: 1}},
		AckInterval: 20 * time.Millisecond,
	}

	f1, _ := NewFollower(followerLog, cfg)
	f1.Start()
	appendN(t, c.leaderLog, "chat", "conv-a", 10)
	waitFor(t, "replication", 5*time.Second, func() bool {
		return len(readAll(t, followerLog, "chat", 0)) == 10
	})
	f1.Close()

	appendN(t, c.leaderLog, "chat", "conv-a", 5)
	f2, _ := NewFollower(followerLog, cfg)
	f2.Start()
	defer f2.Close()

	waitFor(t, "resume without epochs", 5*time.Second, func() bool {
		return len(readAll(t, followerLog, "chat", 0)) == 15
	})
	if s := f2.Stats(); s.EpochConflicts != 0 {
		t.Fatalf("%d epoch conflict(s) on a pre-epoch log; upgrades would stall", s.EpochConflicts)
	}
}
