package stream

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// --- Record encoding limits ---

func TestEncodeRejectsOversizedRecord(t *testing.T) {
	r := &Record{Payload: make([]byte, MaxRecordSize+1)}
	_, err := r.encode()
	if !errors.Is(err, ErrRecordTooLarge) {
		t.Errorf("oversized record: got %v, want ErrRecordTooLarge", err)
	}
}

func TestEncodeRejectsOversizedKey(t *testing.T) {
	// keyLen is a uint16 on the wire; a longer key would silently truncate.
	r := &Record{Key: make([]byte, 70000), Payload: []byte("x")}
	if _, err := r.encode(); err == nil {
		t.Error("a key longer than 65535 bytes was accepted")
	}
}

func TestEncodeAcceptsMaxSizeKey(t *testing.T) {
	r := &Record{Key: make([]byte, 0xFFFF), Payload: []byte("x")}
	encoded, err := r.encode()
	if err != nil {
		t.Fatalf("max-length key rejected: %v", err)
	}
	back, err := decodeRecordBody(encoded[recordFrameHeaderSize:])
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(back.Key) != 0xFFFF {
		t.Errorf("key length = %d after round trip", len(back.Key))
	}
}

func TestDecodeRejectsTruncatedBody(t *testing.T) {
	if _, err := decodeRecordBody([]byte{1, 2, 3}); !errors.Is(err, ErrCorruptRecord) {
		t.Errorf("short body: got %v", err)
	}
}

func TestDecodeRejectsInconsistentLengths(t *testing.T) {
	r := &Record{Key: []byte("k"), Payload: []byte("payload")}
	encoded, _ := r.encode()
	body := encoded[recordFrameHeaderSize:]

	// Claim a payload longer than the body actually holds.
	binary.LittleEndian.PutUint32(body[23:27], 9999)
	if _, err := decodeRecordBody(body); !errors.Is(err, ErrCorruptRecord) {
		t.Errorf("inconsistent lengths: got %v", err)
	}
}

func TestDecodeHeadersRejectsMalformed(t *testing.T) {
	// Length prefix promising more bytes than remain.
	if _, err := decodeHeaders([]byte{0xFF, 0xFF, 'a'}); !errors.Is(err, ErrCorruptRecord) {
		t.Errorf("malformed headers: got %v", err)
	}
	// Truncated mid-pair.
	if _, err := decodeHeaders([]byte{0x01, 0x00}); !errors.Is(err, ErrCorruptRecord) {
		t.Errorf("truncated headers: got %v", err)
	}
}

func TestHeadersWithEmptyValues(t *testing.T) {
	r := &Record{Headers: map[string]string{"empty": "", "": "novalue"}, Payload: []byte("x")}
	encoded, err := r.encode()
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	back, err := decodeRecordBody(encoded[recordFrameHeaderSize:])
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if back.Headers["empty"] != "" || back.Headers[""] != "novalue" {
		t.Errorf("headers = %v", back.Headers)
	}
}

func TestRecordsAreCopiedNotAliased(t *testing.T) {
	// Records outlive the read buffer inside fan-out channels; aliasing would
	// corrupt them when the buffer is reused.
	p, _ := tempPartition(t, PartitionConfig{})
	p.Append(&Record{Key: []byte("k"), Payload: []byte("original")})

	first, _ := p.Read(1, 1, 0)
	second, _ := p.Read(1, 1, 0)

	first[0].Payload[0] = 'X'
	if string(second[0].Payload) != "original" {
		t.Error("two reads of the same record share a backing array")
	}
}

// --- Partition edge cases ---

func TestReadWithZeroMaxRecordsUsesDefault(t *testing.T) {
	p, _ := tempPartition(t, PartitionConfig{})
	for i := 0; i < 200; i++ {
		p.Append(rec("x"))
	}
	got, err := p.Read(1, 0, 0)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if len(got) != 100 {
		t.Errorf("zero maxRecords returned %d, want the 100 default", len(got))
	}
}

func TestReadFromZeroSequence(t *testing.T) {
	p, _ := tempPartition(t, PartitionConfig{})
	p.Append(rec("first"))

	// Sequence 0 never exists; reading from it must still start at 1 rather
	// than tripping the truncation check.
	got, err := p.Read(0, 10, 0)
	if err != nil {
		t.Fatalf("read from 0: %v", err)
	}
	if len(got) != 1 || got[0].Seq != 1 {
		t.Errorf("read from 0 gave %v", got)
	}
}

func TestAppendBatchEmpty(t *testing.T) {
	p, _ := tempPartition(t, PartitionConfig{})
	seqs, err := p.AppendBatch(nil)
	if err != nil || seqs != nil {
		t.Errorf("empty batch: %v, %v", seqs, err)
	}
	if p.NextSeq() != 1 {
		t.Errorf("empty batch advanced the sequence to %d", p.NextSeq())
	}
}

func TestAppendBatchRejectsEphemeral(t *testing.T) {
	p, _ := tempPartition(t, PartitionConfig{})
	batch := []*Record{rec("ok"), {Flags: FlagEphemeral, Payload: []byte("no")}}

	if _, err := p.AppendBatch(batch); err == nil {
		t.Fatal("a batch containing an ephemeral record was accepted")
	}
	// The first record was already written before the failure was detected —
	// assert the partition is still internally consistent.
	got, err := p.Read(1, 10, 0)
	if err != nil {
		t.Fatalf("read after failed batch: %v", err)
	}
	if uint64(len(got))+1 != p.NextSeq() {
		t.Errorf("read %d records but NextSeq is %d — sequence accounting is broken",
			len(got), p.NextSeq())
	}
}

func TestOperationsAfterCloseReturnErrClosed(t *testing.T) {
	dir := t.TempDir()
	p, _ := OpenPartition("t", 0, dir, PartitionConfig{})
	p.Append(rec("before"))
	p.Close()

	if _, err := p.Append(rec("after")); !errors.Is(err, ErrClosed) {
		t.Errorf("append after close: %v", err)
	}
	if _, err := p.AppendBatch([]*Record{rec("after")}); !errors.Is(err, ErrClosed) {
		t.Errorf("batch after close: %v", err)
	}
	if _, err := p.Read(1, 10, 0); !errors.Is(err, ErrClosed) {
		t.Errorf("read after close: %v", err)
	}
	if err := p.Close(); err != nil {
		t.Errorf("double close: %v", err)
	}
	if err := p.Sync(); err != nil {
		t.Errorf("sync after close should be a no-op, got %v", err)
	}
}

func TestTailWakesOnClose(t *testing.T) {
	dir := t.TempDir()
	p, _ := OpenPartition("t", 0, dir, PartitionConfig{})

	done := make(chan error, 1)
	go func() {
		done <- p.Tail(context.Background(), 1, 10, func([]*Record) error { return nil })
	}()
	time.Sleep(50 * time.Millisecond)
	p.Close()

	// A tailer blocked on an empty partition must observe the close rather
	// than hanging forever.
	select {
	case err := <-done:
		if !errors.Is(err, ErrClosed) {
			t.Errorf("tail returned %v, want ErrClosed", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("tail did not wake when the partition closed")
	}
}

func TestTailPropagatesCallbackError(t *testing.T) {
	p, _ := tempPartition(t, PartitionConfig{})
	p.Append(rec("x"))

	boom := errors.New("consumer failed")
	err := p.Tail(context.Background(), 1, 10, func([]*Record) error { return boom })
	if !errors.Is(err, boom) {
		t.Errorf("tail returned %v, want the callback error", err)
	}
}

func TestNotifyChanFiresOnAppend(t *testing.T) {
	p, _ := tempPartition(t, PartitionConfig{})

	wake := p.NotifyChan()
	select {
	case <-wake:
		t.Fatal("notify channel was already closed before any append")
	default:
	}

	p.Append(rec("x"))
	select {
	case <-wake:
	case <-time.After(time.Second):
		t.Fatal("notify channel did not fire on append")
	}

	// It must be single-use: a fresh channel each time.
	next := p.NotifyChan()
	select {
	case <-next:
		t.Fatal("a freshly acquired notify channel was already closed")
	default:
	}
}

func TestNotifyChanFiresOnceForBatch(t *testing.T) {
	p, _ := tempPartition(t, PartitionConfig{})
	wake := p.NotifyChan()

	batch := make([]*Record, 50)
	for i := range batch {
		batch[i] = rec("x")
	}
	p.AppendBatch(batch)

	select {
	case <-wake:
	case <-time.After(time.Second):
		t.Fatal("batch append did not broadcast")
	}
}

func TestAppendsCounter(t *testing.T) {
	p, _ := tempPartition(t, PartitionConfig{})
	if p.Appends() != 0 {
		t.Errorf("fresh partition reports %d appends", p.Appends())
	}
	for i := 0; i < 7; i++ {
		p.Append(rec("x"))
	}
	p.AppendBatch([]*Record{rec("a"), rec("b"), rec("c")})
	if got := p.Appends(); got != 10 {
		t.Errorf("Appends() = %d, want 10", got)
	}
}

func TestPartitionSync(t *testing.T) {
	p, _ := tempPartition(t, PartitionConfig{})
	p.Append(rec("x"))
	if err := p.Sync(); err != nil {
		t.Errorf("sync: %v", err)
	}
}

func TestSyncOnAppendPersistsImmediately(t *testing.T) {
	dir := t.TempDir()
	p, err := OpenPartition("t", 0, dir, PartitionConfig{SyncOnAppend: true})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	for i := 0; i < 10; i++ {
		if _, err := p.Append(rec(fmt.Sprintf("m%d", i))); err != nil {
			t.Fatalf("append: %v", err)
		}
	}

	// Reopen without closing — simulating a crash. With fsync per append,
	// everything acknowledged must be present.
	p2, err := OpenPartition("t", 0, dir, PartitionConfig{})
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer p2.Close()

	got, _ := p2.Read(1, 100, 0)
	if len(got) != 10 {
		t.Errorf("recovered %d of 10 fsynced records", len(got))
	}
	p.Close()
}

func TestConcurrentReadDuringWrite(t *testing.T) {
	p, _ := tempPartition(t, PartitionConfig{SegmentBytes: 2048, IndexInterval: 128})

	var stop atomic.Bool
	var wg sync.WaitGroup

	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < 2000; i++ {
			p.Append(rec(fmt.Sprintf("m%06d", i)))
		}
		stop.Store(true)
	}()

	// Readers must never observe a gap or an out-of-order sequence, even
	// while segments are rolling underneath them.
	for r := 0; r < 4; r++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			var from uint64 = 1
			for !stop.Load() {
				recs, err := p.Read(from, 50, 0)
				if err != nil {
					t.Errorf("concurrent read: %v", err)
					return
				}
				for i, rec := range recs {
					if rec.Seq != from+uint64(i) {
						t.Errorf("gap during concurrent read: expected %d, got %d",
							from+uint64(i), rec.Seq)
						return
					}
				}
				if len(recs) > 0 {
					from = recs[len(recs)-1].Seq + 1
				}
			}
		}()
	}
	wg.Wait()

	final, err := p.Read(1, 5000, 0)
	if err != nil {
		t.Fatalf("final read: %v", err)
	}
	if len(final) != 2000 {
		t.Errorf("final read got %d records, want 2000", len(final))
	}
}

// --- Retention ---

func TestTimeBasedRetention(t *testing.T) {
	dir := t.TempDir()
	p, err := OpenPartition("t", 0, dir, PartitionConfig{
		SegmentBytes: 512,
		RetentionAge: 50 * time.Millisecond,
	})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer p.Close()

	for i := 0; i < 200; i++ {
		p.Append(rec(fmt.Sprintf("old-%d", i)))
	}
	time.Sleep(100 * time.Millisecond)
	// Write more so there is a newer segment whose first timestamp is past
	// the cutoff, which is what marks the older ones as expired.
	for i := 0; i < 200; i++ {
		p.Append(rec(fmt.Sprintf("new-%d", i)))
	}

	removed, err := p.EnforceRetention()
	if err != nil {
		t.Fatalf("retention: %v", err)
	}
	if removed == 0 {
		t.Error("time-based retention removed nothing")
	}
	if p.FirstSeq() == 1 {
		t.Error("FirstSeq did not advance")
	}
}

func TestRetentionDisabledByDefault(t *testing.T) {
	p, _ := tempPartition(t, PartitionConfig{SegmentBytes: 256})
	for i := 0; i < 500; i++ {
		p.Append(rec("x"))
	}
	removed, err := p.EnforceRetention()
	if err != nil {
		t.Fatalf("retention: %v", err)
	}
	if removed != 0 {
		t.Errorf("retention removed %d segments with no policy configured", removed)
	}
	if p.FirstSeq() != 1 {
		t.Error("FirstSeq moved with retention disabled")
	}
}

func TestRetentionOnClosedPartitionIsSafe(t *testing.T) {
	dir := t.TempDir()
	p, _ := OpenPartition("t", 0, dir, PartitionConfig{SegmentBytes: 256, RetentionBytes: 128})
	for i := 0; i < 100; i++ {
		p.Append(rec("x"))
	}
	p.Close()

	if _, err := p.EnforceRetention(); err != nil {
		t.Errorf("retention after close: %v", err)
	}
}

func TestReadAfterRetentionReportsHorizon(t *testing.T) {
	dir := t.TempDir()
	p, _ := OpenPartition("t", 0, dir, PartitionConfig{SegmentBytes: 512, RetentionBytes: 1024})
	defer p.Close()

	for i := 0; i < 400; i++ {
		p.Append(rec(fmt.Sprintf("m%d", i)))
	}
	p.EnforceRetention()

	first := p.FirstSeq()
	if first <= 1 {
		t.Skip("retention did not remove anything on this run")
	}

	_, err := p.Read(first-1, 10, 0)
	if !errors.Is(err, ErrSeqTruncated) {
		t.Errorf("reading below the horizon: got %v, want ErrSeqTruncated", err)
	}
	// The error must name both the request and what is available, so a client
	// can resynchronise without a second round trip.
	if err != nil && !strings.Contains(err.Error(), fmt.Sprint(first)) {
		t.Errorf("error does not report the oldest available sequence: %v", err)
	}
}

// --- Segment internals ---

func TestSegmentContainsAndTimestamps(t *testing.T) {
	dir := t.TempDir()
	seg, err := createSegment(dir, 1, 64)
	if err != nil {
		t.Fatalf("create segment: %v", err)
	}
	defer seg.close()

	if !seg.isEmpty() {
		t.Error("a fresh segment is not empty")
	}
	if seg.contains(1) {
		t.Error("an empty segment claims to contain sequence 1")
	}
	if ts := seg.firstTimestamp(); ts != 0 {
		t.Errorf("empty segment firstTimestamp = %d, want 0", ts)
	}

	for i := uint64(1); i <= 5; i++ {
		r := &Record{Seq: i, Timestamp: int64(1000 + i), Payload: []byte("x")}
		encoded, _ := r.encode()
		if err := seg.append(encoded, i); err != nil {
			t.Fatalf("append: %v", err)
		}
	}

	if seg.isEmpty() {
		t.Error("a segment with records reports empty")
	}
	for _, s := range []uint64{1, 3, 5} {
		if !seg.contains(s) {
			t.Errorf("segment does not contain %d", s)
		}
	}
	for _, s := range []uint64{0, 6, 100} {
		if seg.contains(s) {
			t.Errorf("segment claims to contain %d", s)
		}
	}
	if ts := seg.firstTimestamp(); ts != 1001 {
		t.Errorf("firstTimestamp = %d, want 1001", ts)
	}
}

func TestSegmentAppendToReadOnlyFails(t *testing.T) {
	dir := t.TempDir()
	seg, _ := createSegment(dir, 1, 64)
	r := &Record{Seq: 1, Payload: []byte("x")}
	encoded, _ := r.encode()
	seg.append(encoded, 1)
	seg.close()

	ro, err := openSegment(dir, 1, 64, true)
	if err != nil {
		t.Fatalf("open read-only: %v", err)
	}
	defer ro.close()

	if err := ro.append(encoded, 2); err == nil {
		t.Error("appending to a read-only segment succeeded")
	}
}

func TestParseSegmentBase(t *testing.T) {
	if base, ok := parseSegmentBase("00000000000000004096.log"); !ok || base != 4096 {
		t.Errorf("parse = %d, %v", base, ok)
	}
	for _, name := range []string{"00000000000000004096.index", "notanumber.log", "4096", ""} {
		if _, ok := parseSegmentBase(name); ok {
			t.Errorf("%q was parsed as a segment name", name)
		}
	}
}

func TestRecoveryDiscardsIndexEntriesPastEOF(t *testing.T) {
	dir := t.TempDir()
	p, _ := OpenPartition("t", 0, dir, PartitionConfig{IndexInterval: 32})
	for i := 0; i < 50; i++ {
		p.Append(rec(fmt.Sprintf("m%d", i)))
	}
	p.Close()

	// Simulate the index being fsynced while the log was not: append a bogus
	// index entry pointing far past the end of the log.
	_, idxPath := segmentPaths(dir, 1)
	f, err := os.OpenFile(idxPath, os.O_WRONLY|os.O_APPEND, 0644)
	if err != nil {
		t.Fatalf("open index: %v", err)
	}
	var e [indexEntrySize]byte
	binary.LittleEndian.PutUint64(e[0:8], 9999)
	binary.LittleEndian.PutUint64(e[8:16], 1<<30)
	f.Write(e[:])
	f.Close()

	p2, err := OpenPartition("t", 0, dir, PartitionConfig{IndexInterval: 32})
	if err != nil {
		t.Fatalf("reopen with a bad index: %v", err)
	}
	defer p2.Close()

	got, err := p2.Read(1, 100, 0)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if len(got) != 50 {
		t.Errorf("recovered %d records, want 50", len(got))
	}
	if p2.NextSeq() != 51 {
		t.Errorf("NextSeq = %d, want 51", p2.NextSeq())
	}
}

func TestRecoveryWithMissingIndex(t *testing.T) {
	dir := t.TempDir()
	p, _ := OpenPartition("t", 0, dir, PartitionConfig{IndexInterval: 32})
	for i := 0; i < 30; i++ {
		p.Append(rec(fmt.Sprintf("m%d", i)))
	}
	p.Close()

	// Delete the index entirely; recovery must rebuild state from the log.
	_, idxPath := segmentPaths(dir, 1)
	os.Remove(idxPath)

	p2, err := OpenPartition("t", 0, dir, PartitionConfig{IndexInterval: 32})
	if err != nil {
		t.Fatalf("reopen without an index: %v", err)
	}
	defer p2.Close()

	got, _ := p2.Read(1, 100, 0)
	if len(got) != 30 {
		t.Errorf("recovered %d records without an index, want 30", len(got))
	}
}

// --- Log-level API ---

func TestLogPartitionsAccessor(t *testing.T) {
	l, _ := OpenLog(DefaultLogConfig(t.TempDir()))
	defer l.Close()

	topic, _ := l.CreateTopic("chat", TopicConfig{Partitions: 4})
	parts := topic.Partitions()
	if len(parts) != 4 {
		t.Fatalf("got %d partitions", len(parts))
	}
	for i, p := range parts {
		if p.ID != int32(i) {
			t.Errorf("partition at index %d has ID %d", i, p.ID)
		}
	}
}

func TestLogReadAndTail(t *testing.T) {
	l, _ := OpenLog(DefaultLogConfig(t.TempDir()))
	defer l.Close()

	l.CreateTopic("chat", TopicConfig{Partitions: 2})
	res, err := l.Append("chat", &Record{Key: []byte("k"), Payload: []byte("hello")})
	if err != nil {
		t.Fatalf("append: %v", err)
	}

	got, err := l.Read("chat", res.Partition, 1, 10)
	if err != nil || len(got) != 1 {
		t.Fatalf("Log.Read: %v, %v", got, err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	received := make(chan uint64, 4)
	go l.Tail(ctx, "chat", res.Partition, 1, func(recs []*Record) error {
		for _, r := range recs {
			received <- r.Seq
		}
		return nil
	})

	select {
	case seq := <-received:
		if seq != 1 {
			t.Errorf("Log.Tail delivered seq %d", seq)
		}
	case <-ctx.Done():
		t.Fatal("Log.Tail delivered nothing")
	}
}

func TestLogReadErrorsOnUnknownTopicAndPartition(t *testing.T) {
	l, _ := OpenLog(DefaultLogConfig(t.TempDir()))
	defer l.Close()
	l.CreateTopic("chat", TopicConfig{Partitions: 2})

	if _, err := l.Read("nope", 0, 1, 10); !errors.Is(err, ErrNoSuchTopic) {
		t.Errorf("unknown topic: %v", err)
	}
	if _, err := l.Read("chat", 99, 1, 10); !errors.Is(err, ErrNoSuchPartition) {
		t.Errorf("out-of-range partition: %v", err)
	}
	if _, err := l.Read("chat", -1, 1, 10); !errors.Is(err, ErrNoSuchPartition) {
		t.Errorf("negative partition: %v", err)
	}
	if _, _, err := l.ReadByKey("nope", []byte("k"), 1, 10); !errors.Is(err, ErrNoSuchTopic) {
		t.Errorf("ReadByKey unknown topic: %v", err)
	}
	if err := l.Tail(context.Background(), "nope", 0, 1, nil); !errors.Is(err, ErrNoSuchTopic) {
		t.Errorf("Tail unknown topic: %v", err)
	}
}

func TestLogEnforceRetentionAndSync(t *testing.T) {
	l, err := OpenLog(LogConfig{
		Dir: t.TempDir(),
		DefaultTopic: TopicConfig{
			Partitions: 2,
			Partition:  PartitionConfig{SegmentBytes: 512, RetentionBytes: 1024},
		},
	})
	if err != nil {
		t.Fatalf("open log: %v", err)
	}
	defer l.Close()

	for i := 0; i < 400; i++ {
		l.Append("chat", &Record{Key: []byte("k"), Payload: []byte(fmt.Sprintf("m%d", i))})
	}

	if err := l.Sync(); err != nil {
		t.Errorf("Log.Sync: %v", err)
	}
	removed, err := l.EnforceRetention()
	if err != nil {
		t.Fatalf("Log.EnforceRetention: %v", err)
	}
	if removed == 0 {
		t.Error("log-wide retention removed nothing despite exceeding the cap")
	}
}

func TestLogStartMaintenance(t *testing.T) {
	l, _ := OpenLog(LogConfig{
		Dir: t.TempDir(),
		DefaultTopic: TopicConfig{
			Partitions: 1,
			Partition:  PartitionConfig{SegmentBytes: 512, RetentionBytes: 1024},
		},
	})
	defer l.Close()

	for i := 0; i < 400; i++ {
		l.Append("chat", &Record{Key: []byte("k"), Payload: []byte(fmt.Sprintf("m%d", i))})
	}

	stop := l.StartMaintenance(30 * time.Millisecond)
	defer stop()

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		topic, _ := l.Topic("chat")
		p, _ := topic.Partition(0)
		if p.FirstSeq() > 1 {
			return // maintenance ran retention
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Error("background maintenance never enforced retention")
}

func TestLogStopMaintenanceIsIdempotent(t *testing.T) {
	l, _ := OpenLog(DefaultLogConfig(t.TempDir()))
	defer l.Close()
	stop := l.StartMaintenance(0) // zero interval falls back to a default
	stop()
	stop()
}

func TestLogOperationsAfterClose(t *testing.T) {
	l, _ := OpenLog(DefaultLogConfig(t.TempDir()))
	l.CreateTopic("chat", TopicConfig{Partitions: 1})
	l.Close()

	if err := l.Close(); err != nil {
		t.Errorf("double close: %v", err)
	}
	if _, err := l.CreateTopic("new", TopicConfig{Partitions: 1}); !errors.Is(err, ErrClosed) {
		t.Errorf("CreateTopic after close: %v", err)
	}
	if names := l.TopicNames(); len(names) != 0 {
		t.Errorf("TopicNames after close: %v", names)
	}
	if stats := l.Stats(); len(stats) != 0 {
		t.Errorf("Stats after close: %v", stats)
	}
}

func TestOpenLogRequiresDir(t *testing.T) {
	if _, err := OpenLog(LogConfig{}); err == nil {
		t.Error("OpenLog with no directory succeeded")
	}
}

func TestOpenLogZeroPartitionsFallsBackToDefault(t *testing.T) {
	l, err := OpenLog(LogConfig{Dir: t.TempDir(), DefaultTopic: TopicConfig{Partitions: 0}})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer l.Close()

	topic, err := l.GetOrCreateTopic("chat")
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if topic.PartitionCount() != 16 {
		t.Errorf("partition count = %d, want the default 16", topic.PartitionCount())
	}
}

func TestValidTopicName(t *testing.T) {
	if err := ValidTopicName(""); err == nil {
		t.Error("an empty topic name was accepted")
	}
	if err := ValidTopicName(strings.Repeat("x", 500)); err == nil {
		t.Error("an over-long topic name was accepted")
	}
	if err := ValidTopicName("chat.direct.alice:bob"); err != nil {
		t.Errorf("a normal topic name was rejected: %v", err)
	}
}

func TestCreateTopicRejectsInvalidName(t *testing.T) {
	l, _ := OpenLog(DefaultLogConfig(t.TempDir()))
	defer l.Close()

	if _, err := l.CreateTopic("", TopicConfig{Partitions: 1}); !errors.Is(err, ErrInvalidTopic) {
		t.Errorf("empty name: %v", err)
	}
}

func TestDeleteUnknownTopic(t *testing.T) {
	l, _ := OpenLog(DefaultLogConfig(t.TempDir()))
	defer l.Close()
	if err := l.DeleteTopic("ghost"); !errors.Is(err, ErrNoSuchTopic) {
		t.Errorf("deleting an unknown topic: %v", err)
	}
}

func TestReopenDetectsPartitionGap(t *testing.T) {
	dir := t.TempDir()
	l, _ := OpenLog(DefaultLogConfig(dir))
	l.CreateTopic("chat", TopicConfig{Partitions: 4})
	l.Close()

	// Removing a partition directory must be a loud failure. Silently
	// renumbering would remap every key to a different partition.
	os.RemoveAll(filepath.Join(dir, "chat", "1"))

	if _, err := OpenLog(DefaultLogConfig(dir)); err == nil {
		t.Fatal("a topic with a missing partition opened successfully")
	} else if !strings.Contains(err.Error(), "gap") {
		t.Errorf("error does not describe the gap: %v", err)
	}
}

func TestPartitionForKeyIsStableAcrossReopen(t *testing.T) {
	dir := t.TempDir()
	keys := [][]byte{[]byte("conv-a"), []byte("conv-b"), []byte("alice:bob"), []byte("")}

	l, _ := OpenLog(DefaultLogConfig(dir))
	topic, _ := l.CreateTopic("chat", TopicConfig{Partitions: 8})
	before := make([]int32, len(keys))
	for i, k := range keys {
		before[i] = topic.PartitionForKey(k)
	}
	l.Close()

	l2, _ := OpenLog(DefaultLogConfig(dir))
	defer l2.Close()
	topic2, _ := l2.Topic("chat")
	for i, k := range keys {
		if got := topic2.PartitionForKey(k); got != before[i] {
			t.Errorf("key %q moved from partition %d to %d across a restart",
				k, before[i], got)
		}
	}
}

func TestPartitionForKeyEmptyKeyAndSinglePartition(t *testing.T) {
	l, _ := OpenLog(DefaultLogConfig(t.TempDir()))
	defer l.Close()

	multi, _ := l.CreateTopic("multi", TopicConfig{Partitions: 8})
	if got := multi.PartitionForKey(nil); got != 0 {
		t.Errorf("an empty key routed to partition %d, want 0", got)
	}

	single, _ := l.CreateTopic("single", TopicConfig{Partitions: 1})
	if got := single.PartitionForKey([]byte("anything")); got != 0 {
		t.Errorf("single-partition topic routed to %d", got)
	}
}

// --- Cursor store ---

func TestCursorFlushAndSync(t *testing.T) {
	cs, _ := OpenCursorStore(t.TempDir())
	defer cs.Close()

	cs.Commit(CursorKey{Topic: "chat", Group: "g", Member: "m"}, 10)
	if err := cs.Flush(); err != nil {
		t.Errorf("Flush: %v", err)
	}
	if err := cs.Sync(); err != nil {
		t.Errorf("Sync: %v", err)
	}
}

func TestCursorFlushSurvivesUngracefulExit(t *testing.T) {
	dir := t.TempDir()
	cs, _ := OpenCursorStore(dir)

	key := CursorKey{Topic: "chat", Partition: 0, Group: "user:1", Member: "phone"}
	cs.Commit(key, 500)
	// Flush without closing — the guarantee the auto-flusher provides.
	if err := cs.Flush(); err != nil {
		t.Fatalf("flush: %v", err)
	}

	cs2, err := OpenCursorStore(dir)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer cs2.Close()

	if seq, ok := cs2.Position(key); !ok || seq != 500 {
		t.Errorf("a flushed commit did not survive: %d, %v", seq, ok)
	}
}

func TestCursorAutoFlush(t *testing.T) {
	dir := t.TempDir()
	cs, _ := OpenCursorStore(dir)
	defer cs.Close()

	stop := cs.StartAutoFlush(20 * time.Millisecond)
	defer stop()

	key := CursorKey{Topic: "chat", Group: "g", Member: "m"}
	cs.Commit(key, 42)

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		other, err := OpenCursorStore(dir)
		if err == nil {
			seq, ok := other.Position(key)
			other.Close()
			if ok && seq == 42 {
				return
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Error("auto-flush never pushed the commit to disk")
}

func TestCursorAutoFlushZeroIntervalDefaults(t *testing.T) {
	cs, _ := OpenCursorStore(t.TempDir())
	defer cs.Close()
	stop := cs.StartAutoFlush(0)
	stop()
}

func TestCursorOperationsAfterClose(t *testing.T) {
	cs, _ := OpenCursorStore(t.TempDir())
	key := CursorKey{Topic: "chat", Group: "g", Member: "m"}
	cs.Commit(key, 5)
	cs.Close()

	if err := cs.Commit(key, 10); !errors.Is(err, ErrClosed) {
		t.Errorf("commit after close: %v", err)
	}
	if err := cs.Delete(key); !errors.Is(err, ErrClosed) {
		t.Errorf("delete after close: %v", err)
	}
	if err := cs.Compact(); !errors.Is(err, ErrClosed) {
		t.Errorf("compact after close: %v", err)
	}
	if err := cs.Flush(); err != nil {
		t.Errorf("flush after close should be a no-op: %v", err)
	}
	if err := cs.Close(); err != nil {
		t.Errorf("double close: %v", err)
	}
}

func TestCursorAutoCompactionTriggers(t *testing.T) {
	dir := t.TempDir()
	cs, err := OpenCursorStore(dir)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer cs.Close()

	// Lower the threshold so the background compaction fires quickly.
	cs.mu.Lock()
	cs.compactBytes = 4096
	cs.mu.Unlock()

	key := CursorKey{Topic: "chat", Partition: 0, Group: "user:1", Member: "phone"}
	for i := uint64(1); i <= 5000; i++ {
		if err := cs.Commit(key, i); err != nil {
			t.Fatalf("commit %d: %v", i, err)
		}
	}

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		cs.mu.RLock()
		size := cs.size
		cs.mu.RUnlock()
		if size < 4096 {
			// Compaction ran; the final value must be intact.
			if seq, ok := cs.Position(key); !ok || seq != 5000 {
				t.Errorf("after auto-compaction the cursor is %d, want 5000", seq)
			}
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Error("auto-compaction never ran despite crossing the threshold")
}

func TestCursorCompactionWithConcurrentCommits(t *testing.T) {
	dir := t.TempDir()
	cs, _ := OpenCursorStore(dir)

	var wg sync.WaitGroup
	var stop atomic.Bool

	// Commits landing during a rewrite must be re-appended after the swap,
	// not lost.
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := uint64(1); !stop.Load(); i++ {
			cs.Commit(CursorKey{Topic: "chat", Group: "user:1", Member: "phone"}, i)
			if i%100 == 0 {
				time.Sleep(time.Millisecond)
			}
		}
	}()

	for i := 0; i < 5; i++ {
		if err := cs.Compact(); err != nil {
			t.Errorf("compact: %v", err)
		}
		time.Sleep(20 * time.Millisecond)
	}
	stop.Store(true)
	wg.Wait()

	want, _ := cs.Position(CursorKey{Topic: "chat", Group: "user:1", Member: "phone"})
	cs.Close()

	cs2, err := OpenCursorStore(dir)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer cs2.Close()

	got, ok := cs2.Position(CursorKey{Topic: "chat", Group: "user:1", Member: "phone"})
	if !ok {
		t.Fatal("the cursor vanished across compaction and reopen")
	}
	if got != want {
		t.Errorf("cursor is %d after reopen, was %d before close", got, want)
	}
}

func TestCursorLoadStopsAtCorruption(t *testing.T) {
	dir := t.TempDir()
	cs, _ := OpenCursorStore(dir)
	for i := uint64(1); i <= 10; i++ {
		cs.Commit(CursorKey{Topic: "chat", Group: "g", Member: fmt.Sprintf("m%d", i)}, i)
	}
	cs.Close()

	// Append garbage; the healthy prefix must still load.
	f, _ := os.OpenFile(filepath.Join(dir, cursorFileName), os.O_WRONLY|os.O_APPEND, 0644)
	f.Write([]byte{0xDE, 0xAD, 0xBE, 0xEF, 0x40, 0x00, 0x00, 0x00, 0x01})
	f.Close()

	cs2, err := OpenCursorStore(dir)
	if err != nil {
		t.Fatalf("reopen after corruption: %v", err)
	}
	defer cs2.Close()

	if cs2.Count() != 10 {
		t.Errorf("recovered %d cursors, want the 10 intact ones", cs2.Count())
	}
}

func TestCursorDeleteUnknownKeyIsNoOp(t *testing.T) {
	cs, _ := OpenCursorStore(t.TempDir())
	defer cs.Close()

	if err := cs.Delete(CursorKey{Topic: "chat", Group: "g", Member: "ghost"}); err != nil {
		t.Errorf("deleting an unknown cursor: %v", err)
	}
}

func TestParseCursorKeyRejectsMalformed(t *testing.T) {
	for _, s := range []string{"", "a", "a\x00b", "a\x00b\x00c", "a\x00notanumber\x00c\x00d"} {
		if _, err := parseCursorKey(s); err == nil {
			t.Errorf("malformed cursor key %q was parsed", s)
		}
	}
}

func TestCursorKeyWithEmptyMember(t *testing.T) {
	cs, _ := OpenCursorStore(t.TempDir())
	defer cs.Close()

	// The push dispatcher uses an empty member.
	key := CursorKey{Topic: "chat.inbox.bob", Partition: 0, Group: "push-dispatcher"}
	cs.Commit(key, 88)

	if seq, ok := cs.Position(key); !ok || seq != 88 {
		t.Errorf("empty-member cursor: %d, %v", seq, ok)
	}
	members := cs.GroupMembers("chat.inbox.bob", 0, "push-dispatcher")
	if len(members) != 1 || members[""] != 88 {
		t.Errorf("GroupMembers with an empty member = %v", members)
	}
}

func TestCursorGroupPrefixIsNotAmbiguous(t *testing.T) {
	cs, _ := OpenCursorStore(t.TempDir())
	defer cs.Close()

	// "user:1" must not match "user:10" — a naive prefix scan would.
	cs.Commit(CursorKey{Topic: "chat", Partition: 0, Group: "user:1", Member: "m"}, 10)
	cs.Commit(CursorKey{Topic: "chat", Partition: 0, Group: "user:10", Member: "m"}, 20)

	if m := cs.GroupMembers("chat", 0, "user:1"); len(m) != 1 || m["m"] != 10 {
		t.Errorf("group user:1 = %v — prefix matching leaked", m)
	}
	if m := cs.GroupMembers("chat", 0, "user:10"); len(m) != 1 || m["m"] != 20 {
		t.Errorf("group user:10 = %v", m)
	}
}
