package outbox

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/boltq/boltq/internal/fanout"
	"github.com/boltq/boltq/internal/presence"
	"github.com/boltq/boltq/internal/stream"
)

type recorder struct {
	mu       sync.Mutex
	batches  [][]Notification
	failures int
	err      error
}

func (r *recorder) Notify(_ context.Context, batch []Notification) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.failures > 0 {
		r.failures--
		return r.err
	}
	cp := make([]Notification, len(batch))
	copy(cp, batch)
	r.batches = append(r.batches, cp)
	return nil
}

func (r *recorder) all() []Notification {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []Notification
	for _, b := range r.batches {
		out = append(out, b...)
	}
	return out
}

func (r *recorder) waitFor(t *testing.T, n int, timeout time.Duration) []Notification {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if got := r.all(); len(got) >= n {
			return got
		}
		time.Sleep(10 * time.Millisecond)
	}
	return r.all()
}

type fixture struct {
	log      *stream.Log
	cursors  *stream.CursorStore
	presence *presence.Registry
	rec      *recorder
	disp     *Dispatcher
}

func newFixture(t *testing.T, cfg Config) *fixture {
	t.Helper()

	l, err := stream.OpenLog(stream.LogConfig{
		Dir:          t.TempDir(),
		DefaultTopic: stream.TopicConfig{Partitions: 1},
	})
	if err != nil {
		t.Fatalf("open log: %v", err)
	}
	t.Cleanup(func() { l.Close() })

	cursors, err := stream.OpenCursorStore(t.TempDir())
	if err != nil {
		t.Fatalf("open cursors: %v", err)
	}
	t.Cleanup(func() { cursors.Close() })

	pres := presence.New(presence.Config{NodeID: "n1", SweepInterval: time.Hour})
	t.Cleanup(pres.Close)

	rec := &recorder{}
	d, err := New(Options{Log: l, Cursors: cursors, Presence: presence.LocalLookup{Registry: pres}, Notifier: rec, Config: cfg})
	if err != nil {
		t.Fatalf("new dispatcher: %v", err)
	}
	t.Cleanup(d.Close)

	return &fixture{log: l, cursors: cursors, presence: pres, rec: rec, disp: d}
}

// appendInboxPointer writes the kind of record fanout produces.
func (f *fixture) appendInboxPointer(t *testing.T, userID, convID, msgID string) {
	t.Helper()
	topic := fanout.InboxTopic(userID)
	if _, err := f.log.GetOrCreateTopic(topic); err != nil {
		t.Fatalf("create inbox topic: %v", err)
	}
	_, err := f.log.Append(topic, &stream.Record{
		Key: []byte(userID),
		Headers: map[string]string{
			fanout.HeaderPointer:   "1",
			fanout.HeaderMessageID: msgID,
			fanout.HeaderConvID:    convID,
			fanout.HeaderSender:    "alice",
			fanout.HeaderKind:      string(fanout.KindDirect),
			fanout.HeaderConvSeq:   "7",
			fanout.HeaderConvPart:  "2",
		},
	})
	if err != nil {
		t.Fatalf("append pointer: %v", err)
	}
}

func TestOfflineUserGetsNotification(t *testing.T) {
	f := newFixture(t, Config{GraceDelay: 0})

	topic := fanout.InboxTopic("bob")
	f.log.GetOrCreateTopic(topic)
	if err := f.disp.Watch(topic); err != nil {
		t.Fatalf("watch: %v", err)
	}
	time.Sleep(50 * time.Millisecond) // let the loop reach the head

	f.appendInboxPointer(t, "bob", "conv-1", "msg-1")

	got := f.rec.waitFor(t, 1, 3*time.Second)
	if len(got) != 1 {
		t.Fatalf("got %d notifications, want 1", len(got))
	}
	n := got[0]
	if n.UserID != "bob" || n.MessageID != "msg-1" || n.ConversationID != "conv-1" {
		t.Errorf("notification wrong: %+v", n)
	}
	if n.ConvSeq != 7 || n.ConvPartition != 2 {
		t.Errorf("conversation coordinates lost: seq=%d partition=%d", n.ConvSeq, n.ConvPartition)
	}
	if n.ConvTopic != "chat.direct.conv-1" {
		t.Errorf("ConvTopic = %q", n.ConvTopic)
	}
}

func TestOnlineUserIsSuppressed(t *testing.T) {
	f := newFixture(t, Config{GraceDelay: 0})

	// Bob's phone is connected, so his device already receives the message
	// over its socket — a push would be a duplicate notification.
	f.presence.Bind(presence.Session{UserID: "bob", DeviceID: "phone", ConnID: "c1"})

	topic := fanout.InboxTopic("bob")
	f.log.GetOrCreateTopic(topic)
	f.disp.Watch(topic)
	time.Sleep(50 * time.Millisecond)

	f.appendInboxPointer(t, "bob", "conv-1", "msg-1")
	time.Sleep(300 * time.Millisecond)

	if got := f.rec.all(); len(got) != 0 {
		t.Errorf("pushed %d notifications to an online user", len(got))
	}
	if s := f.disp.Stats(); s.Suppressed == 0 {
		t.Error("suppression was not recorded")
	}
}

func TestCursorAdvancesAndSurvivesRestart(t *testing.T) {
	f := newFixture(t, Config{GraceDelay: 0})

	topic := fanout.InboxTopic("bob")
	f.log.GetOrCreateTopic(topic)
	f.disp.Watch(topic)
	time.Sleep(50 * time.Millisecond)

	for i := 0; i < 3; i++ {
		f.appendInboxPointer(t, "bob", "conv-1", fmt.Sprintf("msg-%d", i))
	}
	f.rec.waitFor(t, 3, 3*time.Second)

	key := stream.CursorKey{Topic: topic, Partition: 0, Group: DispatcherGroup}
	deadline := time.Now().Add(2 * time.Second)
	var seq uint64
	for time.Now().Before(deadline) {
		if s, ok := f.cursors.Position(key); ok && s >= 4 {
			seq = s
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if seq < 4 {
		t.Fatalf("dispatcher cursor at %d, want 4 after 3 notifications", seq)
	}

	// A restarting dispatcher must resume from the cursor rather than re-push
	// every historical message.
	f.disp.Close()

	rec2 := &recorder{}
	d2, err := New(Options{Log: f.log, Cursors: f.cursors, Presence: presence.LocalLookup{Registry: f.presence}, Notifier: rec2})
	if err != nil {
		t.Fatalf("restart dispatcher: %v", err)
	}
	defer d2.Close()
	d2.Watch(topic)
	time.Sleep(300 * time.Millisecond)

	if got := rec2.all(); len(got) != 0 {
		t.Errorf("restarted dispatcher re-sent %d already-notified messages", len(got))
	}
}

func TestFreshDispatcherStartsAtHead(t *testing.T) {
	f := newFixture(t, Config{GraceDelay: 0})

	// History exists before the dispatcher ever runs.
	topic := fanout.InboxTopic("bob")
	f.log.GetOrCreateTopic(topic)
	for i := 0; i < 10; i++ {
		f.appendInboxPointer(t, "bob", "conv-1", fmt.Sprintf("old-%d", i))
	}

	f.disp.Watch(topic)
	time.Sleep(300 * time.Millisecond)

	// Turning on push notifications must not notify everyone about every
	// message they ever received.
	if got := f.rec.all(); len(got) != 0 {
		t.Fatalf("a fresh dispatcher pushed %d historical messages", len(got))
	}

	// New messages are still picked up.
	f.appendInboxPointer(t, "bob", "conv-1", "new-1")
	got := f.rec.waitFor(t, 1, 3*time.Second)
	if len(got) != 1 || got[0].MessageID != "new-1" {
		t.Errorf("new message not dispatched: %+v", got)
	}
}

func TestRetryThenSucceed(t *testing.T) {
	f := newFixture(t, Config{
		GraceDelay: 0, MaxAttempts: 5, RetryBackoff: 10 * time.Millisecond,
	})
	f.rec.failures = 2
	f.rec.err = errors.New("push provider unavailable")

	topic := fanout.InboxTopic("bob")
	f.log.GetOrCreateTopic(topic)
	f.disp.Watch(topic)
	time.Sleep(50 * time.Millisecond)

	f.appendInboxPointer(t, "bob", "conv-1", "msg-1")

	got := f.rec.waitFor(t, 1, 3*time.Second)
	if len(got) != 1 {
		t.Fatalf("notification never succeeded after transient failures: %d", len(got))
	}
	if got[0].Attempt != 3 {
		t.Errorf("delivered on attempt %d, want 3", got[0].Attempt)
	}
	if s := f.disp.Stats(); s.Failed != 2 {
		t.Errorf("recorded %d failures, want 2", s.Failed)
	}
}

func TestPoisonedBatchIsDroppedNotStalled(t *testing.T) {
	f := newFixture(t, Config{
		GraceDelay: 0, MaxAttempts: 2, RetryBackoff: 5 * time.Millisecond,
	})
	f.rec.failures = 1000 // permanently broken for the first batch
	f.rec.err = errors.New("permanent failure")

	topic := fanout.InboxTopic("bob")
	f.log.GetOrCreateTopic(topic)
	f.disp.Watch(topic)
	time.Sleep(50 * time.Millisecond)

	f.appendInboxPointer(t, "bob", "conv-1", "poison")

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if f.disp.Stats().Dropped > 0 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if s := f.disp.Stats(); s.Dropped == 0 {
		t.Fatal("a permanently failing batch was never dropped — the queue is stalled")
	}

	// And the cursor must have moved past it so later messages flow.
	key := stream.CursorKey{Topic: topic, Partition: 0, Group: DispatcherGroup}
	if seq, ok := f.cursors.Position(key); !ok || seq < 2 {
		t.Errorf("cursor stuck at %d after dropping a poisoned batch", seq)
	}
}

func TestBatchingGroupsMultipleMessages(t *testing.T) {
	f := newFixture(t, Config{GraceDelay: 0, BatchSize: 256})

	topic := fanout.InboxTopic("bob")
	f.log.GetOrCreateTopic(topic)

	// Write before watching so the first read picks them all up at once.
	for i := 0; i < 20; i++ {
		f.appendInboxPointer(t, "bob", "conv-1", fmt.Sprintf("m%d", i))
	}

	key := stream.CursorKey{Topic: topic, Partition: 0, Group: DispatcherGroup}
	f.cursors.Commit(key, 1) // force a replay from the beginning

	f.disp.Watch(topic)
	got := f.rec.waitFor(t, 20, 3*time.Second)
	if len(got) != 20 {
		t.Fatalf("got %d notifications, want 20", len(got))
	}

	f.rec.mu.Lock()
	batches := len(f.rec.batches)
	f.rec.mu.Unlock()
	if batches > 3 {
		t.Errorf("20 messages produced %d webhook calls — batching is not working", batches)
	}
}

func TestWatchIsIdempotent(t *testing.T) {
	f := newFixture(t, Config{})
	topic := fanout.InboxTopic("bob")
	f.log.GetOrCreateTopic(topic)

	for i := 0; i < 3; i++ {
		if err := f.disp.Watch(topic); err != nil {
			t.Fatalf("watch %d: %v", i, err)
		}
	}
	if s := f.disp.Stats(); s.Watching != 1 {
		t.Errorf("watching %d topics after 3 identical calls", s.Watching)
	}
}

func TestWatchUnknownTopicFails(t *testing.T) {
	f := newFixture(t, Config{})
	if err := f.disp.Watch("chat.inbox.ghost"); err == nil {
		t.Error("watching a nonexistent topic succeeded")
	}
}

func TestWatchInboxesDiscoversTopics(t *testing.T) {
	f := newFixture(t, Config{GraceDelay: 0})

	f.log.GetOrCreateTopic(fanout.InboxTopic("bob"))
	f.log.GetOrCreateTopic(fanout.InboxTopic("carol"))
	f.log.GetOrCreateTopic("chat.group.g1") // not an inbox

	f.disp.WatchInboxes(50 * time.Millisecond)
	time.Sleep(150 * time.Millisecond)

	if s := f.disp.Stats(); s.Watching != 2 {
		t.Errorf("watching %d topics, want the 2 inboxes only", s.Watching)
	}

	// A topic created after startup must be picked up by the next scan.
	f.log.GetOrCreateTopic(fanout.InboxTopic("dave"))
	time.Sleep(150 * time.Millisecond)
	if s := f.disp.Stats(); s.Watching != 3 {
		t.Errorf("watching %d topics after a new inbox appeared, want 3", s.Watching)
	}
}

func TestUnwatch(t *testing.T) {
	f := newFixture(t, Config{})
	topic := fanout.InboxTopic("bob")
	f.log.GetOrCreateTopic(topic)

	f.disp.Watch(topic)
	f.disp.Unwatch(topic)
	if s := f.disp.Stats(); s.Watching != 0 {
		t.Errorf("still watching %d topics after Unwatch", s.Watching)
	}
}

func TestDiscardNotifier(t *testing.T) {
	if err := (DiscardNotifier{}).Notify(context.Background(), []Notification{{UserID: "x"}}); err != nil {
		t.Errorf("DiscardNotifier returned %v", err)
	}
}

func TestNotifierFuncAdapter(t *testing.T) {
	var got int
	f := NotifierFunc(func(_ context.Context, b []Notification) error {
		got = len(b)
		return nil
	})
	f.Notify(context.Background(), []Notification{{}, {}})
	if got != 2 {
		t.Errorf("adapter passed %d notifications", got)
	}
}
