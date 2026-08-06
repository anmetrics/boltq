package stream

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

func TestRecordEpochRoundTrip(t *testing.T) {
	orig := &Record{Seq: 7, Timestamp: 42, Key: []byte("k"), Payload: []byte("v"), Epoch: 9}
	frame, err := orig.encode()
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	got, err := DecodeRecord(frame)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.Epoch != 9 {
		t.Fatalf("epoch = %d, want 9", got.Epoch)
	}
	if got.Flags&FlagHasEpoch != 0 {
		t.Fatalf("FlagHasEpoch leaked into the decoded record's flags")
	}
	if string(got.Payload) != "v" || string(got.Key) != "k" {
		t.Fatalf("payload/key corrupted by the epoch field: %q %q", got.Payload, got.Key)
	}
}

// A record with no epoch must encode to the pre-epoch layout, byte for byte, or
// an upgraded node would write segments an older one cannot read.
func TestRecordWithoutEpochKeepsLegacyLayout(t *testing.T) {
	r := &Record{Seq: 1, Timestamp: 1, Payload: []byte("hello")}
	frame, err := r.encode()
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	wantLen := recordFrameHeaderSize + recordBodyHeaderSize + len("hello")
	if len(frame) != wantLen {
		t.Fatalf("frame is %d bytes, want %d — an epoch was written for a record without one",
			len(frame), wantLen)
	}
	if frame[recordFrameHeaderSize+16]&FlagHasEpoch != 0 {
		t.Fatalf("FlagHasEpoch set on an epoch-less record")
	}
}

// Flags must not be able to smuggle in a false epoch claim: a caller setting
// FlagHasEpoch by hand, with Epoch unset, would otherwise produce a body the
// decoder sizes wrongly.
func TestRecordRejectsForgedEpochFlag(t *testing.T) {
	r := &Record{Seq: 1, Payload: []byte("x"), Flags: FlagHasEpoch}
	frame, err := r.encode()
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	got, err := DecodeRecord(frame)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.Epoch != 0 {
		t.Fatalf("epoch = %d, want 0", got.Epoch)
	}
	if string(got.Payload) != "x" {
		t.Fatalf("payload = %q, want %q", got.Payload, "x")
	}
}

func TestEpochCacheEndOffsetSemantics(t *testing.T) {
	dir := t.TempDir()
	c, err := openEpochCache(dir)
	if err != nil {
		t.Fatalf("open: %v", err)
	}

	// Epoch 1 covers 1..9, epoch 2 covers 10..19, epoch 5 covers 20 onwards.
	for _, e := range []struct {
		epoch uint32
		start uint64
	}{{1, 1}, {2, 10}, {5, 20}} {
		if err := c.assign(e.epoch, e.start); err != nil {
			t.Fatalf("assign %d: %v", e.epoch, err)
		}
	}

	const logEnd = 25
	cases := []struct {
		name      string
		query     uint32
		wantEpoch uint32
		wantEnd   uint64
	}{
		{"open term keeps everything", 5, 5, logEnd},
		{"newer than ours is still capped at our end", 9, 5, logEnd},
		{"closed term ends where the next began", 2, 2, 20},
		{"first term ends where the second began", 1, 1, 10},
		{"unknown older term is unplaceable", 0, UndefinedEpoch, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			gotEpoch, gotEnd := c.endOffsetFor(tc.query, logEnd)
			if gotEpoch != tc.wantEpoch || gotEnd != tc.wantEnd {
				t.Fatalf("endOffsetFor(%d) = (%d, %d), want (%d, %d)",
					tc.query, gotEpoch, gotEnd, tc.wantEpoch, tc.wantEnd)
			}
		})
	}
}

func TestEpochCacheSurvivesReopen(t *testing.T) {
	dir := t.TempDir()
	c, err := openEpochCache(dir)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if err := c.assign(3, 1); err != nil {
		t.Fatalf("assign: %v", err)
	}
	if err := c.assign(4, 11); err != nil {
		t.Fatalf("assign: %v", err)
	}

	reopened, err := openEpochCache(dir)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	got := reopened.snapshot()
	want := []epochEntry{{3, 1}, {4, 11}}
	if len(got) != len(want) {
		t.Fatalf("history = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("entry %d = %v, want %v", i, got[i], want[i])
		}
	}
}

// A checkpoint whose tail was torn by a crash must still yield its valid
// prefix: a missing recent epoch costs an over-conservative truncation, while
// refusing to open would strand the partition entirely.
func TestEpochCacheToleratesTornCheckpoint(t *testing.T) {
	dir := t.TempDir()
	c, _ := openEpochCache(dir)
	if err := c.assign(1, 1); err != nil {
		t.Fatalf("assign: %v", err)
	}
	if err := c.assign(2, 5); err != nil {
		t.Fatalf("assign: %v", err)
	}

	path := filepath.Join(dir, epochCheckpointFile)
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read checkpoint: %v", err)
	}
	// Lop off half of the final entry.
	if err := os.WriteFile(path, data[:len(data)-epochEntrySize/2], 0644); err != nil {
		t.Fatalf("write torn checkpoint: %v", err)
	}

	reopened, err := openEpochCache(dir)
	if err != nil {
		t.Fatalf("reopen after torn write: %v", err)
	}
	got := reopened.snapshot()
	if len(got) != 1 || got[0] != (epochEntry{1, 1}) {
		t.Fatalf("history = %v, want the intact prefix [{1 1}]", got)
	}
}

func TestPartitionStampsLeaderEpoch(t *testing.T) {
	p, _ := tempPartition(t, PartitionConfig{})

	if _, err := p.Append(rec("before")); err != nil {
		t.Fatalf("append: %v", err)
	}
	if err := p.BecomeLeader(4); err != nil {
		t.Fatalf("become leader: %v", err)
	}
	if _, err := p.Append(rec("after")); err != nil {
		t.Fatalf("append: %v", err)
	}

	got, err := p.Read(1, 10, 0)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("read %d records, want 2", len(got))
	}
	if got[0].Epoch != UndefinedEpoch {
		t.Fatalf("pre-promotion record has epoch %d, want 0", got[0].Epoch)
	}
	if got[1].Epoch != 4 {
		t.Fatalf("post-promotion record has epoch %d, want 4", got[1].Epoch)
	}

	// The term began at the sequence after the pre-promotion record.
	if epoch, start := p.LeaderEpoch(); epoch != 4 || start != 2 {
		t.Fatalf("LeaderEpoch = (%d, %d), want (4, 2)", epoch, start)
	}
}

func TestPartitionRefusesStaleLeaderEpoch(t *testing.T) {
	p, _ := tempPartition(t, PartitionConfig{})
	if err := p.BecomeLeader(7); err != nil {
		t.Fatalf("become leader: %v", err)
	}
	if err := p.BecomeLeader(7); err == nil {
		t.Fatal("re-taking the same epoch was accepted; two leaders could then write the same term")
	}
	if err := p.BecomeLeader(6); err == nil {
		t.Fatal("going backwards in epoch was accepted")
	}
}

func TestPartitionTruncateTo(t *testing.T) {
	// Small segments so the truncation crosses a segment boundary.
	p, _ := tempPartition(t, PartitionConfig{SegmentBytes: 256, IndexInterval: 32})

	const n = 100
	for i := 0; i < n; i++ {
		if _, err := p.Append(rec(fmt.Sprintf("m-%03d", i))); err != nil {
			t.Fatalf("append %d: %v", i, err)
		}
	}
	if p.NextSeq() != n+1 {
		t.Fatalf("NextSeq = %d, want %d", p.NextSeq(), n+1)
	}

	removed, err := p.TruncateTo(41)
	if err != nil {
		t.Fatalf("truncate: %v", err)
	}
	if removed != 60 {
		t.Fatalf("removed %d records, want 60", removed)
	}
	if p.NextSeq() != 41 {
		t.Fatalf("NextSeq = %d after truncation, want 41", p.NextSeq())
	}

	got, err := p.Read(1, n+10, 0)
	if err != nil {
		t.Fatalf("read after truncate: %v", err)
	}
	if len(got) != 40 {
		t.Fatalf("read %d records after truncate, want 40", len(got))
	}
	for i, r := range got {
		if r.Seq != uint64(i+1) {
			t.Fatalf("record %d has seq %d", i, r.Seq)
		}
		if want := fmt.Sprintf("m-%03d", i); string(r.Payload) != want {
			t.Fatalf("record %d payload = %q, want %q", i, r.Payload, want)
		}
	}

	// The log must still accept writes, continuing from the truncation point.
	seq, err := p.Append(rec("after-truncate"))
	if err != nil {
		t.Fatalf("append after truncate: %v", err)
	}
	if seq != 41 {
		t.Fatalf("append after truncate got seq %d, want 41", seq)
	}
	got, err = p.Read(41, 5, 0)
	if err != nil || len(got) != 1 || string(got[0].Payload) != "after-truncate" {
		t.Fatalf("read back after truncate: %v %v", got, err)
	}
}

func TestPartitionTruncateSurvivesReopen(t *testing.T) {
	dir := t.TempDir()
	p, err := OpenPartition("test", 0, dir, PartitionConfig{SegmentBytes: 256, IndexInterval: 32})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	for i := 0; i < 50; i++ {
		if _, err := p.Append(rec(fmt.Sprintf("m-%03d", i))); err != nil {
			t.Fatalf("append: %v", err)
		}
	}
	if _, err := p.TruncateTo(21); err != nil {
		t.Fatalf("truncate: %v", err)
	}
	if err := p.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	reopened, err := OpenPartition("test", 0, dir, PartitionConfig{SegmentBytes: 256, IndexInterval: 32})
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer reopened.Close()

	if reopened.NextSeq() != 21 {
		t.Fatalf("NextSeq after reopen = %d, want 21 — truncated records came back", reopened.NextSeq())
	}
	got, err := reopened.Read(1, 100, 0)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if len(got) != 20 {
		t.Fatalf("read %d records after reopen, want 20", len(got))
	}
}

// Truncation must drop epoch history that no longer has records behind it,
// or a later query would report a term the log cannot serve.
func TestTruncateDropsOrphanedEpochs(t *testing.T) {
	p, _ := tempPartition(t, PartitionConfig{})

	if err := p.BecomeLeader(1); err != nil {
		t.Fatalf("become leader: %v", err)
	}
	for i := 0; i < 10; i++ {
		p.Append(rec("a"))
	}
	if err := p.BecomeLeader(2); err != nil {
		t.Fatalf("become leader: %v", err)
	}
	for i := 0; i < 10; i++ {
		p.Append(rec("b"))
	}

	// Epoch 2 begins at 11; truncating to 11 removes all of its records.
	if _, err := p.TruncateTo(11); err != nil {
		t.Fatalf("truncate: %v", err)
	}
	if epoch, _ := p.LeaderEpoch(); epoch != 1 {
		t.Fatalf("LeaderEpoch = %d after truncating away epoch 2, want 1", epoch)
	}
	if got := p.EpochHistory(); len(got) != 1 {
		t.Fatalf("epoch history = %v, want only epoch 1", got)
	}
}

func TestTruncateRefusesToEmptyThePartition(t *testing.T) {
	p, _ := tempPartition(t, PartitionConfig{})
	for i := 0; i < 5; i++ {
		p.Append(rec("m"))
	}
	if _, err := p.TruncateTo(1); err == nil {
		t.Fatal("truncating to the first sequence was accepted; that is deletion, not reconciliation")
	}
	if p.NextSeq() != 6 {
		t.Fatalf("NextSeq = %d, want 6 — the refused truncation still modified the log", p.NextSeq())
	}
}

// The whole point of the mechanism: a follower whose tail was written by a dead
// leader must be able to find where the surviving leader's history diverges.
func TestEndOffsetForEpochLocatesDivergence(t *testing.T) {
	p, _ := tempPartition(t, PartitionConfig{})

	if err := p.BecomeLeader(1); err != nil {
		t.Fatalf("become leader: %v", err)
	}
	for i := 0; i < 5; i++ {
		p.Append(rec("committed"))
	}
	// A new leader takes over at sequence 6.
	if err := p.BecomeLeader(2); err != nil {
		t.Fatalf("become leader: %v", err)
	}
	p.Append(rec("new-term"))

	// A follower still on epoch 1 is told epoch 1 ended at 6 — everything it
	// holds at 6 or beyond was written by the old leader and never committed.
	epoch, end := p.EndOffsetForEpoch(1)
	if epoch != 1 || end != 6 {
		t.Fatalf("EndOffsetForEpoch(1) = (%d, %d), want (1, 6)", epoch, end)
	}

	// A follower already on epoch 2 keeps everything up to our own end.
	epoch, end = p.EndOffsetForEpoch(2)
	if epoch != 2 || end != p.NextSeq() {
		t.Fatalf("EndOffsetForEpoch(2) = (%d, %d), want (2, %d)", epoch, end, p.NextSeq())
	}
}
