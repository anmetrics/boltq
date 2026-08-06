package stream

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

func tempPartition(t *testing.T, cfg PartitionConfig) (*Partition, string) {
	t.Helper()
	dir := t.TempDir()
	p, err := OpenPartition("test", 0, dir, cfg)
	if err != nil {
		t.Fatalf("open partition: %v", err)
	}
	t.Cleanup(func() { p.Close() })
	return p, dir
}

func rec(payload string) *Record {
	return &Record{Key: []byte("conv-1"), Payload: []byte(payload)}
}

func TestRecordRoundTrip(t *testing.T) {
	orig := &Record{
		Seq:       42,
		Timestamp: 1700000000000000000,
		Flags:     FlagTombstone,
		Key:       []byte("conversation-abc"),
		Headers:   map[string]string{"sender": "u1", "type": "text"},
		Payload:   []byte("hello world"),
	}

	encoded, err := orig.encode()
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	if len(encoded) != orig.encodedSize() {
		t.Errorf("encodedSize()=%d but encode produced %d", orig.encodedSize(), len(encoded))
	}

	got, err := decodeRecordBody(encoded[recordFrameHeaderSize:])
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.Seq != orig.Seq || got.Timestamp != orig.Timestamp || got.Flags != orig.Flags {
		t.Errorf("header mismatch: %+v", got)
	}
	if string(got.Key) != string(orig.Key) || string(got.Payload) != string(orig.Payload) {
		t.Errorf("body mismatch: key=%q payload=%q", got.Key, got.Payload)
	}
	if len(got.Headers) != 2 || got.Headers["sender"] != "u1" || got.Headers["type"] != "text" {
		t.Errorf("headers mismatch: %v", got.Headers)
	}
}

func TestRecordEmptyFields(t *testing.T) {
	orig := &Record{Seq: 1, Timestamp: 5}
	encoded, err := orig.encode()
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	got, err := decodeRecordBody(encoded[recordFrameHeaderSize:])
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(got.Key) != 0 || len(got.Payload) != 0 || len(got.Headers) != 0 {
		t.Errorf("expected empty fields, got %+v", got)
	}
}

func TestAppendAssignsGaplessSequences(t *testing.T) {
	p, _ := tempPartition(t, PartitionConfig{})

	for i := 1; i <= 500; i++ {
		seq, err := p.Append(rec(fmt.Sprintf("msg-%d", i)))
		if err != nil {
			t.Fatalf("append %d: %v", i, err)
		}
		if seq != uint64(i) {
			t.Fatalf("expected seq %d, got %d", i, seq)
		}
	}
	if p.NextSeq() != 501 {
		t.Errorf("NextSeq()=%d, want 501", p.NextSeq())
	}
}

func TestAppendRejectsEphemeral(t *testing.T) {
	p, _ := tempPartition(t, PartitionConfig{})
	r := rec("nope")
	r.Flags = FlagEphemeral
	if _, err := p.Append(r); err == nil {
		t.Fatal("expected ephemeral append to be rejected")
	}
}

func TestReadPreservesOrderAcrossSegments(t *testing.T) {
	// Tiny segments force many rolls, exercising the cross-segment read path.
	p, _ := tempPartition(t, PartitionConfig{SegmentBytes: 512, IndexInterval: 64})

	const n = 300
	for i := 0; i < n; i++ {
		if _, err := p.Append(rec(fmt.Sprintf("payload-%04d", i))); err != nil {
			t.Fatalf("append %d: %v", i, err)
		}
	}

	got, err := p.Read(1, n+10, 0)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if len(got) != n {
		t.Fatalf("read %d records, want %d", len(got), n)
	}
	for i, r := range got {
		if r.Seq != uint64(i+1) {
			t.Fatalf("record %d has seq %d", i, r.Seq)
		}
		want := fmt.Sprintf("payload-%04d", i)
		if string(r.Payload) != want {
			t.Fatalf("record %d payload = %q, want %q", i, r.Payload, want)
		}
	}
}

func TestReadFromMiddle(t *testing.T) {
	p, _ := tempPartition(t, PartitionConfig{SegmentBytes: 1024, IndexInterval: 128})
	for i := 0; i < 200; i++ {
		p.Append(rec(fmt.Sprintf("m%d", i)))
	}

	// Every possible starting point must land exactly on that sequence.
	for _, from := range []uint64{1, 2, 57, 100, 199, 200} {
		got, err := p.Read(from, 5, 0)
		if err != nil {
			t.Fatalf("read from %d: %v", from, err)
		}
		if len(got) == 0 {
			t.Fatalf("read from %d returned nothing", from)
		}
		if got[0].Seq != from {
			t.Fatalf("read from %d returned first seq %d", from, got[0].Seq)
		}
	}
}

func TestReadPastHeadIsEmptyNotError(t *testing.T) {
	p, _ := tempPartition(t, PartitionConfig{})
	p.Append(rec("only"))

	got, err := p.Read(2, 10, 0)
	if err != nil {
		t.Fatalf("read past head: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("expected empty read, got %d records", len(got))
	}
}

func TestReadRespectsLimits(t *testing.T) {
	p, _ := tempPartition(t, PartitionConfig{})
	for i := 0; i < 100; i++ {
		p.Append(rec("x"))
	}

	got, _ := p.Read(1, 7, 0)
	if len(got) != 7 {
		t.Errorf("maxRecords=7 returned %d", len(got))
	}

	got, _ = p.Read(1, 1000, 100)
	if len(got) == 0 || len(got) >= 100 {
		t.Errorf("byte budget ignored: returned %d records", len(got))
	}
}

func TestReopenRecoversState(t *testing.T) {
	dir := t.TempDir()
	cfg := PartitionConfig{SegmentBytes: 1024, IndexInterval: 128}

	p, err := OpenPartition("t", 0, dir, cfg)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	for i := 0; i < 250; i++ {
		p.Append(rec(fmt.Sprintf("v%d", i)))
	}
	if err := p.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	p2, err := OpenPartition("t", 0, dir, cfg)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer p2.Close()

	if p2.NextSeq() != 251 {
		t.Errorf("after reopen NextSeq()=%d, want 251", p2.NextSeq())
	}

	got, err := p2.Read(1, 1000, 0)
	if err != nil {
		t.Fatalf("read after reopen: %v", err)
	}
	if len(got) != 250 {
		t.Fatalf("recovered %d records, want 250", len(got))
	}
	if string(got[249].Payload) != "v249" {
		t.Errorf("last record = %q", got[249].Payload)
	}

	// Appends must continue from the recovered sequence.
	seq, _ := p2.Append(rec("after-restart"))
	if seq != 251 {
		t.Errorf("post-restart append got seq %d, want 251", seq)
	}
}

func TestRecoveryTruncatesTornWrite(t *testing.T) {
	dir := t.TempDir()
	cfg := PartitionConfig{}

	p, _ := OpenPartition("t", 0, dir, cfg)
	for i := 0; i < 20; i++ {
		p.Append(rec(fmt.Sprintf("good-%d", i)))
	}
	p.Close()

	// Simulate a crash mid-write: append a truncated frame to the segment.
	logPath, _ := segmentPaths(dir, 1)
	f, err := os.OpenFile(logPath, os.O_WRONLY|os.O_APPEND, 0644)
	if err != nil {
		t.Fatalf("open segment: %v", err)
	}
	f.Write([]byte{0xDE, 0xAD, 0xBE, 0xEF, 0x40, 0x00, 0x00, 0x00, 0x01, 0x02})
	f.Close()

	sizeBefore, _ := os.Stat(logPath)

	p2, err := OpenPartition("t", 0, dir, cfg)
	if err != nil {
		t.Fatalf("reopen after torn write: %v", err)
	}
	defer p2.Close()

	got, err := p2.Read(1, 1000, 0)
	if err != nil {
		t.Fatalf("read after torn write: %v", err)
	}
	if len(got) != 20 {
		t.Fatalf("recovered %d records, want the 20 intact ones", len(got))
	}
	if p2.NextSeq() != 21 {
		t.Errorf("NextSeq()=%d, want 21", p2.NextSeq())
	}

	sizeAfter, _ := os.Stat(logPath)
	if sizeAfter.Size() >= sizeBefore.Size() {
		t.Error("torn tail was not truncated")
	}
}

func TestConcurrentAppendsAreSerialised(t *testing.T) {
	p, _ := tempPartition(t, PartitionConfig{SegmentBytes: 4096, IndexInterval: 256})

	const writers, each = 8, 100
	var wg sync.WaitGroup
	seqs := make([][]uint64, writers)

	for w := 0; w < writers; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			for i := 0; i < each; i++ {
				seq, err := p.Append(rec(fmt.Sprintf("w%d-%d", w, i)))
				if err != nil {
					t.Errorf("append: %v", err)
					return
				}
				seqs[w] = append(seqs[w], seq)
			}
		}(w)
	}
	wg.Wait()

	// Every sequence in 1..N must be handed out exactly once.
	seen := make(map[uint64]bool)
	for _, list := range seqs {
		for _, s := range list {
			if seen[s] {
				t.Fatalf("sequence %d handed out twice", s)
			}
			seen[s] = true
		}
	}
	if len(seen) != writers*each {
		t.Fatalf("got %d unique sequences, want %d", len(seen), writers*each)
	}
	for i := uint64(1); i <= uint64(writers*each); i++ {
		if !seen[i] {
			t.Fatalf("gap: sequence %d never assigned", i)
		}
	}

	got, err := p.Read(1, writers*each+10, 0)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if len(got) != writers*each {
		t.Fatalf("read back %d records, want %d", len(got), writers*each)
	}
	for i, r := range got {
		if r.Seq != uint64(i+1) {
			t.Fatalf("record at index %d has seq %d — log is not ordered", i, r.Seq)
		}
	}
}

func TestAppendBatch(t *testing.T) {
	p, _ := tempPartition(t, PartitionConfig{SegmentBytes: 512, IndexInterval: 64})

	batch := make([]*Record, 50)
	for i := range batch {
		batch[i] = rec(fmt.Sprintf("b%d", i))
	}
	seqs, err := p.AppendBatch(batch)
	if err != nil {
		t.Fatalf("batch: %v", err)
	}
	if len(seqs) != 50 || seqs[0] != 1 || seqs[49] != 50 {
		t.Fatalf("unexpected sequences: first=%d last=%d n=%d", seqs[0], seqs[49], len(seqs))
	}

	got, _ := p.Read(1, 100, 0)
	if len(got) != 50 {
		t.Fatalf("read back %d, want 50", len(got))
	}
}

func TestTailDeliversNewRecords(t *testing.T) {
	p, _ := tempPartition(t, PartitionConfig{})

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	received := make(chan uint64, 100)
	go func() {
		p.Tail(ctx, 1, 10, func(recs []*Record) error {
			for _, r := range recs {
				received <- r.Seq
			}
			return nil
		})
	}()

	// Give the tailer a moment to register, then write.
	time.Sleep(50 * time.Millisecond)
	for i := 0; i < 20; i++ {
		if _, err := p.Append(rec(fmt.Sprintf("live-%d", i))); err != nil {
			t.Fatalf("append: %v", err)
		}
	}

	for want := uint64(1); want <= 20; want++ {
		select {
		case got := <-received:
			if got != want {
				t.Fatalf("tail delivered seq %d, want %d", got, want)
			}
		case <-ctx.Done():
			t.Fatalf("timed out waiting for seq %d", want)
		}
	}
}

func TestTailFromHistory(t *testing.T) {
	p, _ := tempPartition(t, PartitionConfig{})
	for i := 0; i < 10; i++ {
		p.Append(rec("old"))
	}

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	got := make(chan uint64, 20)
	go p.Tail(ctx, 5, 10, func(recs []*Record) error {
		for _, r := range recs {
			got <- r.Seq
		}
		return nil
	})

	select {
	case first := <-got:
		if first != 5 {
			t.Fatalf("tail from 5 delivered %d first", first)
		}
	case <-ctx.Done():
		t.Fatal("tail did not deliver history")
	}
}

func TestTailStopsOnContextCancel(t *testing.T) {
	p, _ := tempPartition(t, PartitionConfig{})
	ctx, cancel := context.WithCancel(context.Background())

	done := make(chan error, 1)
	go func() {
		done <- p.Tail(ctx, 1, 10, func([]*Record) error { return nil })
	}()

	time.Sleep(50 * time.Millisecond)
	cancel()

	select {
	case err := <-done:
		if err != context.Canceled {
			t.Errorf("tail returned %v, want context.Canceled", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("tail did not stop on cancel")
	}
}

func TestRetentionDropsOldSegments(t *testing.T) {
	dir := t.TempDir()
	p, err := OpenPartition("t", 0, dir, PartitionConfig{
		SegmentBytes:   512,
		IndexInterval:  64,
		RetentionBytes: 2048,
	})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer p.Close()

	for i := 0; i < 500; i++ {
		p.Append(rec(fmt.Sprintf("retained-%d", i)))
	}

	removed, err := p.EnforceRetention()
	if err != nil {
		t.Fatalf("retention: %v", err)
	}
	if removed == 0 {
		t.Fatal("retention removed nothing despite exceeding the byte cap")
	}
	if p.FirstSeq() == 1 {
		t.Error("FirstSeq did not advance after retention")
	}

	// Reading below the horizon must fail loudly, not return a silent gap.
	if _, err := p.Read(1, 10, 0); err == nil {
		t.Error("expected ErrSeqTruncated reading below the retention horizon")
	}

	// Reading at the horizon must still work.
	got, err := p.Read(p.FirstSeq(), 10, 0)
	if err != nil {
		t.Fatalf("read at horizon: %v", err)
	}
	if len(got) == 0 {
		t.Error("no records readable at the retention horizon")
	}
}

func TestRetentionKeepsActiveSegment(t *testing.T) {
	dir := t.TempDir()
	p, _ := OpenPartition("t", 0, dir, PartitionConfig{
		SegmentBytes:   512,
		RetentionBytes: 1, // absurdly small: everything is over the cap
	})
	defer p.Close()

	for i := 0; i < 100; i++ {
		p.Append(rec("x"))
	}
	if _, err := p.EnforceRetention(); err != nil {
		t.Fatalf("retention: %v", err)
	}

	// The active segment is never removed, so recent history survives.
	got, err := p.Read(p.FirstSeq(), 100, 0)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if len(got) == 0 {
		t.Fatal("retention emptied the partition entirely")
	}
}

// --- Log / topic tests ---

func TestTopicPartitionRouting(t *testing.T) {
	l, err := OpenLog(DefaultLogConfig(t.TempDir()))
	if err != nil {
		t.Fatalf("open log: %v", err)
	}
	defer l.Close()

	topic, err := l.CreateTopic("chat.conv", TopicConfig{Partitions: 8})
	if err != nil {
		t.Fatalf("create topic: %v", err)
	}

	// The same key must always route to the same partition — this is the
	// entire basis of per-conversation ordering.
	key := []byte("conversation-xyz")
	first := topic.PartitionForKey(key)
	for i := 0; i < 100; i++ {
		if got := topic.PartitionForKey(key); got != first {
			t.Fatalf("key routed to partition %d then %d", first, got)
		}
	}

	// Keys should spread across partitions rather than pile onto one.
	used := make(map[int32]bool)
	for i := 0; i < 200; i++ {
		used[topic.PartitionForKey([]byte(fmt.Sprintf("conv-%d", i)))] = true
	}
	if len(used) < 6 {
		t.Errorf("200 keys only touched %d of 8 partitions — poor distribution", len(used))
	}
}

func TestLogAppendAndReadByKey(t *testing.T) {
	l, _ := OpenLog(DefaultLogConfig(t.TempDir()))
	defer l.Close()

	key := []byte("conv-42")
	for i := 0; i < 30; i++ {
		res, err := l.Append("chat", &Record{Key: key, Payload: []byte(fmt.Sprintf("m%d", i))})
		if err != nil {
			t.Fatalf("append: %v", err)
		}
		if res.Seq != uint64(i+1) {
			t.Fatalf("append %d got seq %d", i, res.Seq)
		}
	}

	got, pid, err := l.ReadByKey("chat", key, 1, 100)
	if err != nil {
		t.Fatalf("read by key: %v", err)
	}
	if len(got) != 30 {
		t.Fatalf("read %d records, want 30", len(got))
	}
	_ = pid

	for i, r := range got {
		if string(r.Payload) != fmt.Sprintf("m%d", i) {
			t.Fatalf("record %d out of order: %q", i, r.Payload)
		}
	}
}

func TestLogInterleavedKeysStayOrderedPerKey(t *testing.T) {
	l, _ := OpenLog(LogConfig{
		Dir:          t.TempDir(),
		DefaultTopic: TopicConfig{Partitions: 4},
	})
	defer l.Close()

	// Two conversations writing concurrently must each stay internally ordered.
	keys := [][]byte{[]byte("conv-a"), []byte("conv-b"), []byte("conv-c")}
	var wg sync.WaitGroup
	for _, k := range keys {
		wg.Add(1)
		go func(k []byte) {
			defer wg.Done()
			for i := 0; i < 50; i++ {
				l.Append("chat", &Record{Key: k, Payload: []byte(fmt.Sprintf("%s-%03d", k, i))})
			}
		}(k)
	}
	wg.Wait()

	for _, k := range keys {
		got, _, err := l.ReadByKey("chat", k, 1, 1000)
		if err != nil {
			t.Fatalf("read %s: %v", k, err)
		}
		var seen int
		prefix := string(k) + "-"
		for _, r := range got {
			body := string(r.Payload)
			if !strings.HasPrefix(body, prefix) {
				continue // another conversation sharing the partition
			}
			idx, err := strconv.Atoi(strings.TrimPrefix(body, prefix))
			if err != nil {
				t.Fatalf("unparsable payload %q", body)
			}
			if idx != seen {
				t.Fatalf("key %s: expected index %d, got %d — per-key order broken", k, seen, idx)
			}
			seen++
		}
		if seen != 50 {
			t.Fatalf("key %s: found %d of 50 records", k, seen)
		}
	}
}

func TestLogReopenRestoresTopics(t *testing.T) {
	dir := t.TempDir()

	l, _ := OpenLog(DefaultLogConfig(dir))
	l.CreateTopic("chat.direct", TopicConfig{Partitions: 4})
	l.CreateTopic("chat.group", TopicConfig{Partitions: 2})
	l.Append("chat.direct", &Record{Key: []byte("k"), Payload: []byte("hello")})
	l.Close()

	l2, err := OpenLog(DefaultLogConfig(dir))
	if err != nil {
		t.Fatalf("reopen log: %v", err)
	}
	defer l2.Close()

	names := l2.TopicNames()
	if len(names) != 2 || names[0] != "chat.direct" || names[1] != "chat.group" {
		t.Fatalf("recovered topics: %v", names)
	}

	direct, err := l2.Topic("chat.direct")
	if err != nil {
		t.Fatalf("topic: %v", err)
	}
	if direct.PartitionCount() != 4 {
		t.Errorf("chat.direct has %d partitions, want 4", direct.PartitionCount())
	}

	got, _, err := l2.ReadByKey("chat.direct", []byte("k"), 1, 10)
	if err != nil || len(got) != 1 || string(got[0].Payload) != "hello" {
		t.Errorf("message did not survive reopen: %v %v", got, err)
	}
}

func TestCreateTopicRejectsPartitionMismatch(t *testing.T) {
	l, _ := OpenLog(DefaultLogConfig(t.TempDir()))
	defer l.Close()

	if _, err := l.CreateTopic("chat", TopicConfig{Partitions: 8}); err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, err := l.CreateTopic("chat", TopicConfig{Partitions: 8}); err != nil {
		t.Errorf("recreating with the same count should be idempotent: %v", err)
	}
	if _, err := l.CreateTopic("chat", TopicConfig{Partitions: 16}); err == nil {
		t.Error("expected an error when changing the partition count")
	}
}

func TestTopicNameEncodingIsPathSafe(t *testing.T) {
	l, _ := OpenLog(DefaultLogConfig(t.TempDir()))
	defer l.Close()

	// A topic name must never escape the data directory.
	for _, name := range []string{"../../etc/passwd", "a/b/c", "with space", "unicode-Ξ"} {
		if _, err := l.CreateTopic(name, TopicConfig{Partitions: 1}); err != nil {
			t.Fatalf("create %q: %v", name, err)
		}
		enc := encodeTopicDir(name)
		if filepath.Base(enc) != enc {
			t.Errorf("encoded topic %q is not a single path element: %q", name, enc)
		}
		dec, err := decodeTopicDir(enc)
		if err != nil || dec != name {
			t.Errorf("round trip of %q gave %q (%v)", name, dec, err)
		}
	}
}

func TestDeleteTopic(t *testing.T) {
	dir := t.TempDir()
	l, _ := OpenLog(DefaultLogConfig(dir))

	l.CreateTopic("doomed", TopicConfig{Partitions: 2})
	l.Append("doomed", &Record{Payload: []byte("x")})

	if err := l.DeleteTopic("doomed"); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if _, err := l.Topic("doomed"); err == nil {
		t.Error("topic still present after delete")
	}
	if _, err := os.Stat(filepath.Join(dir, "doomed")); !os.IsNotExist(err) {
		t.Error("topic directory not removed")
	}
	l.Close()
}

func TestLogStats(t *testing.T) {
	l, _ := OpenLog(DefaultLogConfig(t.TempDir()))
	defer l.Close()

	l.CreateTopic("chat", TopicConfig{Partitions: 2})
	for i := 0; i < 10; i++ {
		l.Append("chat", &Record{Key: []byte("k"), Payload: []byte("hello")})
	}

	stats := l.Stats()
	if len(stats) != 1 || stats[0].Name != "chat" {
		t.Fatalf("unexpected stats: %+v", stats)
	}
	if len(stats[0].Partitions) != 2 {
		t.Fatalf("expected 2 partitions in stats")
	}

	var total uint64
	for _, ps := range stats[0].Partitions {
		total += ps.Records
	}
	if total != 10 {
		t.Errorf("stats report %d records, want 10", total)
	}
	if stats[0].TotalBytes == 0 {
		t.Error("TotalBytes is zero")
	}
}

// --- Cursor tests ---

func TestCursorCommitAndRead(t *testing.T) {
	cs, err := OpenCursorStore(t.TempDir())
	if err != nil {
		t.Fatalf("open cursors: %v", err)
	}
	defer cs.Close()

	key := CursorKey{Topic: "chat", Partition: 3, Group: "user:1", Member: "phone"}

	if _, ok := cs.Position(key); ok {
		t.Error("unset cursor reported as present")
	}
	if got := cs.PositionOr(key, 99); got != 99 {
		t.Errorf("PositionOr default = %d, want 99", got)
	}

	if err := cs.Commit(key, 100); err != nil {
		t.Fatalf("commit: %v", err)
	}
	if seq, ok := cs.Position(key); !ok || seq != 100 {
		t.Errorf("after commit: seq=%d ok=%v", seq, ok)
	}
}

func TestCursorCommitIsMonotonic(t *testing.T) {
	cs, _ := OpenCursorStore(t.TempDir())
	defer cs.Close()

	key := CursorKey{Topic: "chat", Group: "user:1", Member: "phone"}
	cs.Commit(key, 500)

	// A stale in-flight commit must not rewind the cursor and replay messages.
	if err := cs.Commit(key, 200); err != nil {
		t.Fatalf("stale commit: %v", err)
	}
	if seq, _ := cs.Position(key); seq != 500 {
		t.Errorf("cursor rewound to %d — stale commit was applied", seq)
	}

	cs.Commit(key, 501)
	if seq, _ := cs.Position(key); seq != 501 {
		t.Errorf("forward commit not applied: %d", seq)
	}
}

func TestCursorPersistsAcrossReopen(t *testing.T) {
	dir := t.TempDir()
	cs, _ := OpenCursorStore(dir)

	keys := map[string]uint64{"phone": 10, "laptop": 25, "tablet": 3}
	for member, seq := range keys {
		cs.Commit(CursorKey{Topic: "chat", Partition: 1, Group: "user:7", Member: member}, seq)
	}
	if err := cs.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	cs2, err := OpenCursorStore(dir)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer cs2.Close()

	for member, want := range keys {
		got, ok := cs2.Position(CursorKey{Topic: "chat", Partition: 1, Group: "user:7", Member: member})
		if !ok || got != want {
			t.Errorf("member %s: got %d (ok=%v), want %d", member, got, ok, want)
		}
	}
}

func TestCursorMultiDeviceGroup(t *testing.T) {
	cs, _ := OpenCursorStore(t.TempDir())
	defer cs.Close()

	positions := map[string]uint64{"phone": 100, "laptop": 80, "tablet": 140}
	for member, seq := range positions {
		cs.Commit(CursorKey{Topic: "chat", Partition: 0, Group: "user:9", Member: member}, seq)
	}

	members := cs.GroupMembers("chat", 0, "user:9")
	if len(members) != 3 {
		t.Fatalf("GroupMembers returned %d, want 3", len(members))
	}
	for m, want := range positions {
		if members[m] != want {
			t.Errorf("member %s at %d, want %d", m, members[m], want)
		}
	}

	// The watermark is the slowest device — the laptop that has been offline.
	slowest, ok := cs.SlowestInGroup("chat", 0, "user:9")
	if !ok || slowest != 80 {
		t.Errorf("SlowestInGroup = %d (ok=%v), want 80", slowest, ok)
	}

	if _, ok := cs.SlowestInGroup("chat", 0, "user:nobody"); ok {
		t.Error("SlowestInGroup reported a value for an unknown group")
	}
}

func TestCursorGroupIsolation(t *testing.T) {
	cs, _ := OpenCursorStore(t.TempDir())
	defer cs.Close()

	cs.Commit(CursorKey{Topic: "chat", Partition: 0, Group: "user:1", Member: "phone"}, 10)
	cs.Commit(CursorKey{Topic: "chat", Partition: 0, Group: "user:2", Member: "phone"}, 20)
	cs.Commit(CursorKey{Topic: "chat", Partition: 1, Group: "user:1", Member: "phone"}, 30)
	cs.Commit(CursorKey{Topic: "other", Partition: 0, Group: "user:1", Member: "phone"}, 40)

	m := cs.GroupMembers("chat", 0, "user:1")
	if len(m) != 1 || m["phone"] != 10 {
		t.Errorf("group scoping leaked across topic/partition/group: %v", m)
	}
}

func TestCursorDelete(t *testing.T) {
	cs, _ := OpenCursorStore(t.TempDir())
	defer cs.Close()

	key := CursorKey{Topic: "chat", Group: "user:1", Member: "old-device"}
	cs.Commit(key, 55)
	if err := cs.Delete(key); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if _, ok := cs.Position(key); ok {
		t.Error("cursor still present after delete")
	}
}

func TestCursorCompactionPreservesState(t *testing.T) {
	dir := t.TempDir()
	cs, _ := OpenCursorStore(dir)

	// Many commits to the same small key set — the case compaction targets.
	for round := uint64(1); round <= 2000; round++ {
		for d := 0; d < 5; d++ {
			cs.Commit(CursorKey{
				Topic: "chat", Partition: 0,
				Group: "user:1", Member: fmt.Sprintf("device-%d", d),
			}, round)
		}
	}

	if err := cs.Compact(); err != nil {
		t.Fatalf("compact: %v", err)
	}
	cs.Close()

	cs2, err := OpenCursorStore(dir)
	if err != nil {
		t.Fatalf("reopen after compaction: %v", err)
	}
	defer cs2.Close()

	if cs2.Count() != 5 {
		t.Errorf("after compaction %d cursors survive, want 5", cs2.Count())
	}
	for d := 0; d < 5; d++ {
		key := CursorKey{Topic: "chat", Partition: 0, Group: "user:1", Member: fmt.Sprintf("device-%d", d)}
		if seq, ok := cs2.Position(key); !ok || seq != 2000 {
			t.Errorf("device-%d at %d (ok=%v), want 2000", d, seq, ok)
		}
	}

	info, _ := os.Stat(filepath.Join(dir, cursorFileName))
	if info.Size() > 2000 {
		t.Errorf("compacted log is %d bytes — compaction did not shrink it", info.Size())
	}
}

func TestCursorConcurrentCommits(t *testing.T) {
	cs, _ := OpenCursorStore(t.TempDir())
	defer cs.Close()

	var wg sync.WaitGroup
	for d := 0; d < 10; d++ {
		wg.Add(1)
		go func(d int) {
			defer wg.Done()
			key := CursorKey{Topic: "chat", Group: "user:1", Member: fmt.Sprintf("d%d", d)}
			for i := uint64(1); i <= 200; i++ {
				if err := cs.Commit(key, i); err != nil {
					t.Errorf("commit: %v", err)
					return
				}
			}
		}(d)
	}
	wg.Wait()

	for d := 0; d < 10; d++ {
		key := CursorKey{Topic: "chat", Group: "user:1", Member: fmt.Sprintf("d%d", d)}
		if seq, ok := cs.Position(key); !ok || seq != 200 {
			t.Errorf("d%d ended at %d (ok=%v), want 200", d, seq, ok)
		}
	}
}

func TestCursorKeyRoundTrip(t *testing.T) {
	key := CursorKey{Topic: "chat.conv", Partition: 17, Group: "user:abc", Member: "device-1"}
	got, err := parseCursorKey(key.String())
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if got != key {
		t.Errorf("round trip gave %+v, want %+v", got, key)
	}
}

// --- Benchmarks ---

func BenchmarkAppend(b *testing.B) {
	dir, _ := os.MkdirTemp("", "boltq-bench")
	defer os.RemoveAll(dir)

	p, _ := OpenPartition("bench", 0, dir, PartitionConfig{})
	defer p.Close()

	payload := []byte("a typical short chat message body")
	b.ResetTimer()
	b.SetBytes(int64(len(payload)))
	for i := 0; i < b.N; i++ {
		p.Append(&Record{Key: []byte("conv"), Payload: payload})
	}
}

func BenchmarkReadSequential(b *testing.B) {
	dir, _ := os.MkdirTemp("", "boltq-bench")
	defer os.RemoveAll(dir)

	p, _ := OpenPartition("bench", 0, dir, PartitionConfig{})
	defer p.Close()

	for i := 0; i < 100000; i++ {
		p.Append(&Record{Key: []byte("conv"), Payload: []byte("message body here")})
	}

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		p.Read(uint64(i%99000)+1, 50, 0)
	}
}

func BenchmarkCursorCommit(b *testing.B) {
	dir, _ := os.MkdirTemp("", "boltq-bench")
	defer os.RemoveAll(dir)

	cs, _ := OpenCursorStore(dir)
	defer cs.Close()

	key := CursorKey{Topic: "chat", Partition: 0, Group: "user:1", Member: "phone"}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		cs.Commit(key, uint64(i+1))
	}
}
