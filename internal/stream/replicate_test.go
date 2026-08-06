package stream

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"
)

// --- AppendReplicated ---

func TestAppendReplicatedAppliesInOrder(t *testing.T) {
	p, _ := tempPartition(t, PartitionConfig{})

	for i := uint64(1); i <= 20; i++ {
		rec := &Record{Seq: i, Timestamp: int64(1000 + i), Key: []byte("k"),
			Payload: []byte(fmt.Sprintf("m%d", i))}
		if err := p.AppendReplicated(rec); err != nil {
			t.Fatalf("apply %d: %v", i, err)
		}
	}

	if p.NextSeq() != 21 {
		t.Errorf("NextSeq = %d, want 21", p.NextSeq())
	}
	got, err := p.Read(1, 100, 0)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if len(got) != 20 {
		t.Fatalf("read %d records, want 20", len(got))
	}
	for i, r := range got {
		if r.Seq != uint64(i+1) {
			t.Fatalf("record %d has seq %d", i, r.Seq)
		}
		// The leader's timestamp must be preserved, not re-stamped locally —
		// otherwise the replica shows different times than the leader.
		if r.Timestamp != int64(1001+i) {
			t.Fatalf("record %d timestamp = %d, want %d", i, r.Timestamp, 1001+i)
		}
	}
}

func TestAppendReplicatedIsIdempotent(t *testing.T) {
	p, _ := tempPartition(t, PartitionConfig{})

	for i := uint64(1); i <= 5; i++ {
		p.AppendReplicated(&Record{Seq: i, Timestamp: 1, Payload: []byte("x")})
	}

	// A replay after a reconnect must be silently absorbed, not duplicated.
	for i := uint64(1); i <= 5; i++ {
		if err := p.AppendReplicated(&Record{Seq: i, Timestamp: 1, Payload: []byte("x")}); err != nil {
			t.Fatalf("replay of %d: %v", i, err)
		}
	}

	if p.NextSeq() != 6 {
		t.Errorf("NextSeq = %d after replay, want 6", p.NextSeq())
	}
	got, _ := p.Read(1, 100, 0)
	if len(got) != 5 {
		t.Errorf("replay duplicated records: %d present, want 5", len(got))
	}
}

func TestAppendReplicatedRejectsGap(t *testing.T) {
	p, _ := tempPartition(t, PartitionConfig{})
	p.AppendReplicated(&Record{Seq: 1, Timestamp: 1, Payload: []byte("x")})

	// Skipping 2 would write a hole into the log; refusing lets the follower
	// re-fetch from its true position instead.
	err := p.AppendReplicated(&Record{Seq: 5, Timestamp: 1, Payload: []byte("x")})
	if !errors.Is(err, ErrReplicationGap) {
		t.Fatalf("got %v, want ErrReplicationGap", err)
	}
	if !containsAll(err.Error(), "5", "2") {
		t.Errorf("error should name both the received and expected sequence: %v", err)
	}
	if p.NextSeq() != 2 {
		t.Errorf("a rejected record advanced NextSeq to %d", p.NextSeq())
	}
}

func containsAll(s string, subs ...string) bool {
	for _, sub := range subs {
		found := false
		for i := 0; i+len(sub) <= len(s); i++ {
			if s[i:i+len(sub)] == sub {
				found = true
				break
			}
		}
		if !found {
			return false
		}
	}
	return true
}

func TestAppendReplicatedValidation(t *testing.T) {
	p, _ := tempPartition(t, PartitionConfig{})

	if err := p.AppendReplicated(&Record{Payload: []byte("x")}); err == nil {
		t.Error("a record with no sequence was accepted")
	}
	if err := p.AppendReplicated(&Record{Seq: 1, Flags: FlagEphemeral}); err == nil {
		t.Error("an ephemeral record was replicated")
	}
}

func TestAppendReplicatedAfterClose(t *testing.T) {
	dir := t.TempDir()
	p, _ := OpenPartition("t", 0, dir, PartitionConfig{})
	p.Close()

	if err := p.AppendReplicated(&Record{Seq: 1, Payload: []byte("x")}); !errors.Is(err, ErrClosed) {
		t.Errorf("got %v, want ErrClosed", err)
	}
}

func TestAppendReplicatedRollsSegments(t *testing.T) {
	p, _ := tempPartition(t, PartitionConfig{SegmentBytes: 512, IndexInterval: 64})

	for i := uint64(1); i <= 300; i++ {
		rec := &Record{Seq: i, Timestamp: int64(i), Key: []byte("k"),
			Payload: []byte(fmt.Sprintf("payload-%04d", i))}
		if err := p.AppendReplicated(rec); err != nil {
			t.Fatalf("apply %d: %v", i, err)
		}
	}

	got, err := p.Read(1, 1000, 0)
	if err != nil {
		t.Fatalf("read across segments: %v", err)
	}
	if len(got) != 300 {
		t.Fatalf("read %d records, want 300", len(got))
	}
}

func TestAppendReplicatedBatch(t *testing.T) {
	p, _ := tempPartition(t, PartitionConfig{})

	batch := make([]*Record, 10)
	for i := range batch {
		batch[i] = &Record{Seq: uint64(i + 1), Timestamp: 1, Payload: []byte("x")}
	}
	applied, err := p.AppendReplicatedBatch(batch)
	if err != nil {
		t.Fatalf("batch: %v", err)
	}
	if applied != 10 {
		t.Errorf("applied %d, want 10", applied)
	}

	// A batch containing a gap must stop at the gap and report how far it got,
	// so the caller can resume precisely.
	gapped := []*Record{
		{Seq: 11, Timestamp: 1, Payload: []byte("x")},
		{Seq: 99, Timestamp: 1, Payload: []byte("x")},
		{Seq: 100, Timestamp: 1, Payload: []byte("x")},
	}
	applied, err = p.AppendReplicatedBatch(gapped)
	if !errors.Is(err, ErrReplicationGap) {
		t.Fatalf("got %v, want ErrReplicationGap", err)
	}
	if applied != 1 {
		t.Errorf("applied %d before the gap, want 1", applied)
	}
	if p.NextSeq() != 12 {
		t.Errorf("NextSeq = %d, want 12", p.NextSeq())
	}
}

func TestAppendReplicatedWakesTailers(t *testing.T) {
	p, _ := tempPartition(t, PartitionConfig{})

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	got := make(chan uint64, 10)
	go p.Tail(ctx, 1, 10, func(recs []*Record) error {
		for _, r := range recs {
			got <- r.Seq
		}
		return nil
	})
	time.Sleep(50 * time.Millisecond)

	// A replica's readers must wake on replicated writes exactly as they do on
	// local ones, or a follower serving reads would go stale.
	p.AppendReplicated(&Record{Seq: 1, Timestamp: 1, Payload: []byte("x")})

	select {
	case seq := <-got:
		if seq != 1 {
			t.Errorf("tailer got seq %d", seq)
		}
	case <-ctx.Done():
		t.Fatal("a replicated append did not wake the tailer")
	}
}

func TestReplicatedAndLocalAppendsInterleaveSafely(t *testing.T) {
	p, _ := tempPartition(t, PartitionConfig{})

	// Mixing the two write paths on one partition is a misconfiguration (a
	// node is either leader or follower for a partition), but it must not
	// corrupt the log — the sequence check makes the loser fail cleanly.
	var wg sync.WaitGroup
	var localOK, replOK int
	var mu sync.Mutex

	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			if i%2 == 0 {
				if _, err := p.Append(&Record{Payload: []byte("local")}); err == nil {
					mu.Lock()
					localOK++
					mu.Unlock()
				}
				return
			}
			if err := p.AppendReplicated(&Record{
				Seq: uint64(i), Timestamp: 1, Payload: []byte("repl"),
			}); err == nil {
				mu.Lock()
				replOK++
				mu.Unlock()
			}
		}(i)
	}
	wg.Wait()

	recs, err := p.Read(1, 1000, 0)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	for i, r := range recs {
		if r.Seq != uint64(i+1) {
			t.Fatalf("log is not gap-free after interleaved writes: index %d has seq %d", i, r.Seq)
		}
	}
	if uint64(len(recs))+1 != p.NextSeq() {
		t.Errorf("%d records but NextSeq is %d", len(recs), p.NextSeq())
	}
}

// --- Log-level replication helpers ---

func TestApplyReplicatedCreatesTopic(t *testing.T) {
	l, _ := OpenLog(DefaultLogConfig(t.TempDir()))
	defer l.Close()

	// A follower may see records for a topic it has never heard of; refusing
	// them would leave it permanently unable to catch up.
	err := l.ApplyReplicated("chat.direct.alice:bob", 4, 2,
		&Record{Seq: 1, Timestamp: 1, Key: []byte("alice:bob"), Payload: []byte("hi")})
	if err != nil {
		t.Fatalf("apply: %v", err)
	}

	topic, err := l.Topic("chat.direct.alice:bob")
	if err != nil {
		t.Fatalf("topic not created: %v", err)
	}
	if topic.PartitionCount() != 4 {
		t.Errorf("partition count = %d, want the leader's 4", topic.PartitionCount())
	}

	got, err := l.Read("chat.direct.alice:bob", 2, 1, 10)
	if err != nil || len(got) != 1 || string(got[0].Payload) != "hi" {
		t.Errorf("record not applied: %v, %v", got, err)
	}
}

func TestApplyReplicatedUsesDefaultPartitionCount(t *testing.T) {
	l, _ := OpenLog(LogConfig{Dir: t.TempDir(), DefaultTopic: TopicConfig{Partitions: 8}})
	defer l.Close()

	if err := l.ApplyReplicated("chat", 0, 0, &Record{Seq: 1, Timestamp: 1, Payload: []byte("x")}); err != nil {
		t.Fatalf("apply: %v", err)
	}
	topic, _ := l.Topic("chat")
	if topic.PartitionCount() != 8 {
		t.Errorf("partition count = %d, want the log default 8", topic.PartitionCount())
	}
}

func TestApplyReplicatedRejectsBadPartition(t *testing.T) {
	l, _ := OpenLog(DefaultLogConfig(t.TempDir()))
	defer l.Close()
	l.CreateTopic("chat", TopicConfig{Partitions: 2})

	err := l.ApplyReplicated("chat", 2, 99, &Record{Seq: 1, Timestamp: 1, Payload: []byte("x")})
	if !errors.Is(err, ErrNoSuchPartition) {
		t.Errorf("got %v, want ErrNoSuchPartition", err)
	}
}

func TestPartitionForResolvesAndErrors(t *testing.T) {
	l, _ := OpenLog(DefaultLogConfig(t.TempDir()))
	defer l.Close()
	l.CreateTopic("chat", TopicConfig{Partitions: 2})

	if _, err := l.PartitionFor("chat", 1); err != nil {
		t.Errorf("PartitionFor: %v", err)
	}
	if _, err := l.PartitionFor("nope", 0); !errors.Is(err, ErrNoSuchTopic) {
		t.Errorf("unknown topic: %v", err)
	}
	if _, err := l.PartitionFor("chat", 9); !errors.Is(err, ErrNoSuchPartition) {
		t.Errorf("bad partition: %v", err)
	}
}

// --- Record codec ---

func TestReadRecordFrame(t *testing.T) {
	var buf frameBuffer
	for i := uint64(1); i <= 3; i++ {
		enc, err := EncodeRecord(&Record{Seq: i, Timestamp: int64(i), Payload: []byte("x")})
		if err != nil {
			t.Fatalf("encode: %v", err)
		}
		buf.data = append(buf.data, enc...)
	}

	for i := uint64(1); i <= 3; i++ {
		frame, err := ReadRecordFrame(&buf)
		if err != nil {
			t.Fatalf("read frame %d: %v", i, err)
		}
		rec, err := DecodeRecord(frame)
		if err != nil {
			t.Fatalf("decode frame %d: %v", i, err)
		}
		if rec.Seq != i {
			t.Errorf("frame %d has seq %d", i, rec.Seq)
		}
	}
	if _, err := ReadRecordFrame(&buf); err == nil {
		t.Error("reading past the end succeeded")
	}
}

func TestReadRecordFrameRejectsAbsurdLength(t *testing.T) {
	buf := frameBuffer{data: []byte{
		0, 0, 0, 0, // crc
		0xFF, 0xFF, 0xFF, 0xFF, // body length: absurd
	}}
	if _, err := ReadRecordFrame(&buf); !errors.Is(err, ErrCorruptRecord) {
		t.Errorf("got %v, want ErrCorruptRecord", err)
	}
}

func TestDecodeRecordRejectsShortAndCorrupt(t *testing.T) {
	if _, err := DecodeRecord([]byte{1, 2, 3}); !errors.Is(err, ErrCorruptRecord) {
		t.Errorf("short frame: %v", err)
	}

	enc, _ := EncodeRecord(&Record{Seq: 1, Timestamp: 1, Payload: []byte("hello")})

	truncated := enc[:len(enc)-2]
	if _, err := DecodeRecord(truncated); !errors.Is(err, ErrCorruptRecord) {
		t.Errorf("truncated frame: %v", err)
	}

	corrupt := append([]byte(nil), enc...)
	corrupt[len(corrupt)-1] ^= 0xFF
	if _, err := DecodeRecord(corrupt); !errors.Is(err, ErrCorruptRecord) {
		t.Errorf("corrupt frame: %v", err)
	}
}

type frameBuffer struct{ data []byte }

func (b *frameBuffer) Read(p []byte) (int, error) {
	if len(b.data) == 0 {
		return 0, errors.New("EOF")
	}
	n := copy(p, b.data)
	b.data = b.data[n:]
	return n, nil
}

// --- AckWaiter ---

type fakeWaiter struct {
	mu        sync.Mutex
	calls     int
	lastSeq   uint64
	lastTopic string
	err       error
	delay     time.Duration
}

func (f *fakeWaiter) WaitFor(ctx context.Context, topic string, partition int32, seq uint64) error {
	f.mu.Lock()
	f.calls++
	f.lastSeq = seq
	f.lastTopic = topic
	delay, err := f.delay, f.err
	f.mu.Unlock()

	if delay > 0 {
		select {
		case <-time.After(delay):
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return err
}

func (f *fakeWaiter) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

func TestAppendContextWithoutWaiter(t *testing.T) {
	l, _ := OpenLog(DefaultLogConfig(t.TempDir()))
	defer l.Close()

	res, err := l.AppendContext(context.Background(), "chat", &Record{
		Key: []byte("k"), Payload: []byte("x"),
	})
	if err != nil {
		t.Fatalf("append: %v", err)
	}
	if res.Seq != 1 {
		t.Errorf("seq = %d", res.Seq)
	}
}

func TestAppendContextConsultsWaiter(t *testing.T) {
	l, _ := OpenLog(DefaultLogConfig(t.TempDir()))
	defer l.Close()

	w := &fakeWaiter{}
	l.SetAckWaiter(w)

	res, err := l.AppendContext(context.Background(), "chat", &Record{
		Key: []byte("k"), Payload: []byte("x"),
	})
	if err != nil {
		t.Fatalf("append: %v", err)
	}
	if w.count() != 1 {
		t.Errorf("waiter called %d times", w.count())
	}
	if w.lastSeq != res.Seq || w.lastTopic != "chat" {
		t.Errorf("waiter got %s/%d, append produced %s/%d",
			w.lastTopic, w.lastSeq, "chat", res.Seq)
	}
}

func TestAppendContextReportsReplicationFailureButKeepsData(t *testing.T) {
	l, _ := OpenLog(DefaultLogConfig(t.TempDir()))
	defer l.Close()

	l.SetAckWaiter(&fakeWaiter{err: ErrNotEnoughReplicas})

	res, err := l.AppendContext(context.Background(), "chat", &Record{
		Key: []byte("k"), Payload: []byte("stored anyway"),
	})
	if err == nil {
		t.Fatal("replication failure was not reported")
	}
	if !errors.Is(err, ErrNotEnoughReplicas) {
		t.Errorf("error does not wrap the cause: %v", err)
	}
	// The record is already durable locally and visible to readers; the caller
	// must be able to find it rather than assume the write vanished.
	if res.Seq == 0 {
		t.Fatal("no coordinates returned for a locally-durable record")
	}
	got, err := l.Read("chat", res.Partition, res.Seq, 1)
	if err != nil || len(got) != 1 || string(got[0].Payload) != "stored anyway" {
		t.Errorf("record not readable after a replication failure: %v, %v", got, err)
	}
}

func TestAppendContextRespectsCancellation(t *testing.T) {
	l, _ := OpenLog(DefaultLogConfig(t.TempDir()))
	defer l.Close()

	l.SetAckWaiter(&fakeWaiter{delay: 5 * time.Second})

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()

	start := time.Now()
	_, err := l.AppendContext(ctx, "chat", &Record{Key: []byte("k"), Payload: []byte("x")})
	if err == nil {
		t.Fatal("a cancelled wait returned success")
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Errorf("cancellation took %v to take effect", elapsed)
	}
}

func TestSetAckWaiterNilRestoresLocalOnly(t *testing.T) {
	l, _ := OpenLog(DefaultLogConfig(t.TempDir()))
	defer l.Close()

	w := &fakeWaiter{}
	l.SetAckWaiter(w)
	l.AppendContext(context.Background(), "chat", &Record{Key: []byte("k"), Payload: []byte("x")})

	l.SetAckWaiter(nil)
	l.AppendContext(context.Background(), "chat", &Record{Key: []byte("k"), Payload: []byte("y")})

	if w.count() != 1 {
		t.Errorf("waiter called %d times after being cleared", w.count())
	}
}

func TestAppendContextPropagatesAppendError(t *testing.T) {
	l, _ := OpenLog(DefaultLogConfig(t.TempDir()))
	l.Close()

	w := &fakeWaiter{}
	l.SetAckWaiter(w)
	if _, err := l.AppendContext(context.Background(), "chat", &Record{Payload: []byte("x")}); err == nil {
		t.Fatal("append to a closed log succeeded")
	}
	if w.count() != 0 {
		t.Error("the waiter was consulted despite the append failing")
	}
}
