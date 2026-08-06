package fanout

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/boltq/boltq/internal/dedup"
	"github.com/boltq/boltq/internal/presence"
	"github.com/boltq/boltq/internal/stream"
)

// --- Construction ---

func TestNewValidatesDependencies(t *testing.T) {
	l, _ := stream.OpenLog(stream.LogConfig{Dir: t.TempDir()})
	defer l.Close()

	if _, err := New(Options{Members: staticMembers{}}); err == nil {
		t.Error("New succeeded without a log")
	}
	if _, err := New(Options{Log: l}); err == nil {
		t.Error("New succeeded without a member lister")
	}
	if _, err := New(Options{Log: l, Members: staticMembers{}}); err != nil {
		t.Errorf("minimal valid options rejected: %v", err)
	}
}

func TestConfigDefaults(t *testing.T) {
	l, _ := stream.OpenLog(stream.LogConfig{Dir: t.TempDir()})
	defer l.Close()

	d, _ := New(Options{Log: l, Members: staticMembers{}})
	if d.cfg.FanoutOnWriteLimit != 256 || d.cfg.ConversationPartitions != 16 || d.cfg.InboxPartitions != 1 {
		t.Errorf("defaults not applied: %+v", d.cfg)
	}
}

// --- Working without optional dependencies ---

func TestSendWithoutDedup(t *testing.T) {
	l, _ := stream.OpenLog(stream.LogConfig{Dir: t.TempDir(), DefaultTopic: stream.TopicConfig{Partitions: 2}})
	defer l.Close()

	d, _ := New(Options{Log: l, Members: staticMembers{"g1": {"alice"}}})
	ctx := context.Background()

	req := SendRequest{Kind: KindGroup, ConversationID: "g1", SenderID: "alice",
		ClientMsgID: "same", Payload: []byte("x")}

	// With no dedup table, an identical client_msg_id must still be accepted —
	// it simply is not collapsed.
	first, err := d.Send(ctx, req)
	if err != nil {
		t.Fatalf("first: %v", err)
	}
	second, err := d.Send(ctx, req)
	if err != nil {
		t.Fatalf("second: %v", err)
	}
	if second.Duplicate {
		t.Error("a send was flagged duplicate with no dedup table configured")
	}
	if second.Seq == first.Seq {
		t.Error("two sends got the same sequence")
	}
}

func TestSendWithoutPresenceReportsNoOfflineUsers(t *testing.T) {
	l, _ := stream.OpenLog(stream.LogConfig{Dir: t.TempDir(), DefaultTopic: stream.TopicConfig{Partitions: 2}})
	defer l.Close()

	d, _ := New(Options{Log: l, Members: staticMembers{"g1": {"alice", "bob"}}})
	res, err := d.Send(context.Background(), SendRequest{
		Kind: KindGroup, ConversationID: "g1", SenderID: "alice", Payload: []byte("x"),
	})
	if err != nil {
		t.Fatalf("send: %v", err)
	}
	// Without a presence registry there is nothing to report; the push
	// dispatcher decides on its own from its cursor.
	if len(res.OfflineUsers) != 0 {
		t.Errorf("OfflineUsers = %v with no presence registry", res.OfflineUsers)
	}
}

func TestOnlineRecipientsAreExcludedFromOfflineList(t *testing.T) {
	members := staticMembers{"g1": {"alice", "bob", "carol"}}
	d, _ := newDeliverer(t, members, Config{})

	// Bob is connected; only carol should be listed for a push.
	d.presence.(presence.LocalLookup).Registry.Bind(presence.Session{UserID: "bob", DeviceID: "phone", ConnID: "c1"})

	res, err := d.Send(context.Background(), SendRequest{
		Kind: KindGroup, ConversationID: "g1", SenderID: "alice", Payload: []byte("x"),
	})
	if err != nil {
		t.Fatalf("send: %v", err)
	}
	if len(res.OfflineUsers) != 1 || res.OfflineUsers[0] != "carol" {
		t.Errorf("OfflineUsers = %v, want just carol", res.OfflineUsers)
	}
}

// --- Defaults and normalisation ---

func TestSendDefaultsToDirectKind(t *testing.T) {
	d, _ := newDeliverer(t, staticMembers{"c1": {"alice", "bob"}}, Config{})

	res, err := d.Send(context.Background(), SendRequest{
		ConversationID: "c1", SenderID: "alice", Payload: []byte("x"),
	})
	if err != nil {
		t.Fatalf("send: %v", err)
	}
	if res.Topic != "chat.direct.c1" {
		t.Errorf("topic = %q, want the direct topic", res.Topic)
	}
}

func TestSendUsesConfiguredTenant(t *testing.T) {
	var gotTenant string
	lister := MemberListerFunc(func(_ context.Context, tenant, group string) ([]string, error) {
		gotTenant = tenant
		return []string{"alice"}, nil
	})
	d, _ := newDeliverer(t, lister, Config{Tenant: "default-tenant"})

	d.Send(context.Background(), SendRequest{
		Kind: KindGroup, ConversationID: "g1", SenderID: "alice", Payload: []byte("x"),
	})
	if gotTenant != "default-tenant" {
		t.Errorf("tenant = %q, want the configured default", gotTenant)
	}

	d.Send(context.Background(), SendRequest{
		Tenant: "explicit", Kind: KindGroup, ConversationID: "g1",
		SenderID: "alice", Payload: []byte("x"),
	})
	if gotTenant != "explicit" {
		t.Errorf("tenant = %q, want the explicit value to win", gotTenant)
	}
}

// --- Headers ---

func TestUserHeadersArePreservedAndSystemHeadersWin(t *testing.T) {
	d, _ := newDeliverer(t, staticMembers{"g1": {"alice"}}, Config{})

	res, err := d.Send(context.Background(), SendRequest{
		Kind: KindGroup, ConversationID: "g1", SenderID: "alice",
		Payload: []byte("x"),
		Headers: map[string]string{
			"content_type":  "text/plain",
			HeaderSender:    "mallory", // an attempt to forge the sender
			HeaderMessageID: "forged",
		},
	})
	if err != nil {
		t.Fatalf("send: %v", err)
	}

	recs, _ := d.History(KindGroup, "g1", 1, 10)
	h := recs[0].Headers

	if h["content_type"] != "text/plain" {
		t.Error("a user header was dropped")
	}
	// The server's values must overwrite any client-supplied ones, or a client
	// could attribute its message to someone else.
	if h[HeaderSender] != "alice" {
		t.Errorf("sender header = %q — a client forged it", h[HeaderSender])
	}
	if h[HeaderMessageID] != res.MessageID {
		t.Errorf("message id header = %q, want %q", h[HeaderMessageID], res.MessageID)
	}
}

func TestInboxPointersDoNotShareHeaderMaps(t *testing.T) {
	members := staticMembers{"g1": {"alice", "bob", "carol"}}
	d, _ := newDeliverer(t, members, Config{})

	d.Send(context.Background(), SendRequest{
		Kind: KindGroup, ConversationID: "g1", SenderID: "alice", Payload: []byte("x"),
	})

	// Mutating one recipient's headers must not affect another's — records
	// outlive the send call inside tailers.
	aliceRecs, _ := d.Backlog("alice", 1, 10)
	bobRecs, _ := d.Backlog("bob", 1, 10)
	if len(aliceRecs) == 0 || len(bobRecs) == 0 {
		t.Fatal("inbox pointers missing")
	}

	aliceRecs[0].Headers[HeaderSender] = "mutated"
	if bobRecs[0].Headers[HeaderSender] != "alice" {
		t.Error("two recipients' pointers share one header map")
	}
}

func TestConversationSeqHeaderMatchesActualSeq(t *testing.T) {
	d, _ := newDeliverer(t, staticMembers{"g1": {"alice", "bob"}}, Config{})
	ctx := context.Background()

	for i := 0; i < 5; i++ {
		res, err := d.Send(ctx, SendRequest{
			Kind: KindGroup, ConversationID: "g1", SenderID: "alice",
			ClientMsgID: fmt.Sprint(i), Payload: []byte("x"),
		})
		if err != nil {
			t.Fatalf("send %d: %v", i, err)
		}

		recs, _ := d.Backlog("bob", uint64(i+1), 1)
		if len(recs) != 1 {
			t.Fatalf("inbox pointer %d missing", i)
		}
		seq, _ := strconv.ParseUint(recs[0].Headers[HeaderConvSeq], 10, 64)
		part, _ := strconv.ParseInt(recs[0].Headers[HeaderConvPart], 10, 32)

		if seq != res.Seq {
			t.Errorf("pointer %d references seq %d, message is at %d", i, seq, res.Seq)
		}
		if int32(part) != res.Partition {
			t.Errorf("pointer %d references partition %d, message is in %d", i, part, res.Partition)
		}
	}
}

// --- Message IDs ---

func TestMessageIDsAreUniqueAndOpaque(t *testing.T) {
	seen := make(map[string]bool, 10000)
	for i := 0; i < 10000; i++ {
		id := newMessageID()
		if len(id) != 32 {
			t.Fatalf("message id has length %d, want 32 hex chars", len(id))
		}
		if seen[id] {
			t.Fatal("newMessageID produced a duplicate")
		}
		seen[id] = true
	}
}

func TestMessageIDsAreNotSequential(t *testing.T) {
	// Guessable IDs would let one user probe for another's messages.
	a, b := newMessageID(), newMessageID()
	diff := 0
	for i := range a {
		if a[i] != b[i] {
			diff++
		}
	}
	if diff < 8 {
		t.Errorf("consecutive message IDs differ in only %d of 32 characters", diff)
	}
}

// --- Fan-out boundary ---

func TestExactlyAtFanoutLimitStillWrites(t *testing.T) {
	users := make([]string, 10)
	for i := range users {
		users[i] = fmt.Sprintf("u%d", i)
	}
	d, _ := newDeliverer(t, staticMembers{"g": users}, Config{FanoutOnWriteLimit: 10})

	res, err := d.Send(context.Background(), SendRequest{
		Kind: KindGroup, ConversationID: "g", SenderID: "u0", Payload: []byte("x"),
	})
	if err != nil {
		t.Fatalf("send: %v", err)
	}
	if res.Strategy != StrategyOnWrite || res.InboxWrites != 10 {
		t.Errorf("at the limit: strategy=%s writes=%d", res.Strategy, res.InboxWrites)
	}
}

func TestOneOverFanoutLimitSwitchesToRead(t *testing.T) {
	users := make([]string, 11)
	for i := range users {
		users[i] = fmt.Sprintf("u%d", i)
	}
	d, _ := newDeliverer(t, staticMembers{"g": users}, Config{FanoutOnWriteLimit: 10})

	res, _ := d.Send(context.Background(), SendRequest{
		Kind: KindGroup, ConversationID: "g", SenderID: "u0", Payload: []byte("x"),
	})
	if res.Strategy != StrategyOnRead || res.InboxWrites != 0 {
		t.Errorf("one over the limit: strategy=%s writes=%d", res.Strategy, res.InboxWrites)
	}
}

func TestSingleMemberConversation(t *testing.T) {
	// A note-to-self conversation still works and still gets an inbox pointer.
	d, _ := newDeliverer(t, staticMembers{"self": {"alice"}}, Config{})

	res, err := d.Send(context.Background(), SendRequest{
		Kind: KindGroup, ConversationID: "self", SenderID: "alice", Payload: []byte("note"),
	})
	if err != nil {
		t.Fatalf("send: %v", err)
	}
	if res.Recipients != 1 || res.InboxWrites != 1 {
		t.Errorf("recipients=%d writes=%d", res.Recipients, res.InboxWrites)
	}
	if len(res.OfflineUsers) != 0 {
		t.Errorf("the sender was queued for a push about their own note: %v", res.OfflineUsers)
	}
}

// --- Dedup interaction ---

func TestInFlightDuplicateIsReported(t *testing.T) {
	tbl := dedup.New(dedup.Config{TTL: time.Minute, SweepInterval: time.Hour})
	defer tbl.Close()

	l, _ := stream.OpenLog(stream.LogConfig{Dir: t.TempDir(), DefaultTopic: stream.TopicConfig{Partitions: 2}})
	defer l.Close()

	// Hold the member lookup open so the first send stays in flight.
	release := make(chan struct{})
	lister := MemberListerFunc(func(context.Context, string, string) ([]string, error) {
		<-release
		return []string{"alice"}, nil
	})
	d, _ := New(Options{Log: l, Members: lister, Dedup: tbl})

	req := SendRequest{Kind: KindGroup, ConversationID: "g1", SenderID: "alice",
		ClientMsgID: "contested", Payload: []byte("x")}

	go d.Send(context.Background(), req)
	time.Sleep(50 * time.Millisecond)

	_, err := d.Send(context.Background(), req)
	if !errors.Is(err, ErrInFlight) {
		t.Errorf("a racing retry returned %v, want ErrInFlight", err)
	}
	close(release)
}

func TestDedupIsScopedToSender(t *testing.T) {
	d, _ := newDeliverer(t, staticMembers{"g1": {"alice", "bob"}}, Config{})
	ctx := context.Background()

	// Two users independently picking the same client ID must not collide.
	a, err := d.Send(ctx, SendRequest{Kind: KindGroup, ConversationID: "g1",
		SenderID: "alice", ClientMsgID: "1", Payload: []byte("from alice")})
	if err != nil {
		t.Fatalf("alice: %v", err)
	}
	b, err := d.Send(ctx, SendRequest{Kind: KindGroup, ConversationID: "g1",
		SenderID: "bob", ClientMsgID: "1", Payload: []byte("from bob")})
	if err != nil {
		t.Fatalf("bob: %v", err)
	}

	if b.Duplicate || b.Seq == a.Seq {
		t.Error("bob's message was swallowed by alice's dedup entry")
	}
	recs, _ := d.History(KindGroup, "g1", 1, 10)
	if len(recs) != 2 {
		t.Fatalf("conversation holds %d messages, want 2", len(recs))
	}
}

func TestConcurrentIdenticalSendsProduceOneMessage(t *testing.T) {
	d, _ := newDeliverer(t, staticMembers{"g1": {"alice"}}, Config{})
	ctx := context.Background()

	req := SendRequest{Kind: KindGroup, ConversationID: "g1", SenderID: "alice",
		ClientMsgID: "once-only", Payload: []byte("x")}

	var wg sync.WaitGroup
	var succeeded, inFlight, duplicates int
	var mu sync.Mutex

	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			res, err := d.Send(ctx, req)
			mu.Lock()
			defer mu.Unlock()
			switch {
			case errors.Is(err, ErrInFlight):
				inFlight++
			case err != nil:
				t.Errorf("unexpected error: %v", err)
			case res.Duplicate:
				duplicates++
			default:
				succeeded++
			}
		}()
	}
	wg.Wait()

	if succeeded != 1 {
		t.Errorf("%d sends were treated as original, want exactly 1", succeeded)
	}
	recs, _ := d.History(KindGroup, "g1", 1, 100)
	if len(recs) != 1 {
		t.Fatalf("the conversation holds %d copies of one message", len(recs))
	}
}

// --- Error paths ---

func TestHistoryAndBacklogOnUnknownTopics(t *testing.T) {
	d, _ := newDeliverer(t, staticMembers{}, Config{})

	if _, err := d.History(KindGroup, "never-existed", 1, 10); err == nil {
		t.Error("History on a nonexistent conversation succeeded")
	}
	if _, err := d.Backlog("never-existed", 1, 10); err == nil {
		t.Error("Backlog for a user with no inbox succeeded")
	}
}

func TestMemberListerErrorPropagates(t *testing.T) {
	boom := errors.New("social graph unavailable")
	d, _ := newDeliverer(t, MemberListerFunc(func(context.Context, string, string) ([]string, error) {
		return nil, boom
	}), Config{})

	_, err := d.Send(context.Background(), SendRequest{
		Kind: KindGroup, ConversationID: "g1", SenderID: "alice", Payload: []byte("x"),
	})
	if !errors.Is(err, boom) {
		t.Errorf("got %v, want the lister error wrapped", err)
	}
}

func TestContextCancellationDuringMemberLookup(t *testing.T) {
	d, _ := newDeliverer(t, MemberListerFunc(func(ctx context.Context, _, _ string) ([]string, error) {
		<-ctx.Done()
		return nil, ctx.Err()
	}), Config{})

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	if _, err := d.Send(ctx, SendRequest{
		Kind: KindGroup, ConversationID: "g1", SenderID: "alice", Payload: []byte("x"),
	}); err == nil {
		t.Error("a cancelled context did not abort the send")
	}
}

// --- Payload handling ---

func TestEmptyPayloadIsAllowed(t *testing.T) {
	// A message can be pure metadata — a reaction, a tombstone, a receipt.
	d, _ := newDeliverer(t, staticMembers{"g1": {"alice"}}, Config{})

	res, err := d.Send(context.Background(), SendRequest{
		Kind: KindGroup, ConversationID: "g1", SenderID: "alice",
		Headers: map[string]string{"type": "reaction", "emoji": "👍"},
	})
	if err != nil {
		t.Fatalf("send: %v", err)
	}
	recs, _ := d.History(KindGroup, "g1", res.Seq, 1)
	if len(recs) != 1 || recs[0].Headers["type"] != "reaction" {
		t.Errorf("metadata-only message not stored correctly: %+v", recs)
	}
}

func TestBinaryPayloadIsOpaque(t *testing.T) {
	d, _ := newDeliverer(t, staticMembers{"g1": {"alice"}}, Config{})

	payload := []byte{0x00, 0xFF, 0x7F, 0x80, 0x00, 0xFE}
	d.Send(context.Background(), SendRequest{
		Kind: KindGroup, ConversationID: "g1", SenderID: "alice", Payload: payload,
	})

	recs, _ := d.History(KindGroup, "g1", 1, 1)
	got := recs[0].Payload
	if len(got) != len(payload) {
		t.Fatalf("payload length %d -> %d", len(payload), len(got))
	}
	for i := range payload {
		if got[i] != payload[i] {
			t.Fatalf("byte %d changed: %#x -> %#x", i, payload[i], got[i])
		}
	}
}

func TestLargePayload(t *testing.T) {
	d, _ := newDeliverer(t, staticMembers{"g1": {"alice"}}, Config{})

	payload := make([]byte, 512<<10) // 512KB
	for i := range payload {
		payload[i] = byte(i % 251)
	}
	res, err := d.Send(context.Background(), SendRequest{
		Kind: KindGroup, ConversationID: "g1", SenderID: "alice", Payload: payload,
	})
	if err != nil {
		t.Fatalf("512KB send: %v", err)
	}

	recs, err := d.History(KindGroup, "g1", res.Seq, 1)
	if err != nil || len(recs) != 1 {
		t.Fatalf("read back: %v, %v", recs, err)
	}
	if len(recs[0].Payload) != len(payload) {
		t.Errorf("payload length %d -> %d", len(payload), len(recs[0].Payload))
	}
	for i := range payload {
		if recs[0].Payload[i] != payload[i] {
			t.Fatalf("byte %d differs", i)
		}
	}
}

// --- Topic helpers ---

func TestConversationFromTopicRejectsEmptyID(t *testing.T) {
	for _, topic := range []string{"chat.group.", "chat.direct.", "chat.group", "chat.direct"} {
		if _, _, ok := ConversationFromTopic(topic); ok {
			t.Errorf("%q was parsed as a conversation topic", topic)
		}
	}
}

func TestConversationIDsWithSpecialCharacters(t *testing.T) {
	// Conversation IDs come from the application and may contain colons,
	// dashes and unicode. They must survive routing and read-back.
	ids := []string{"alice:bob", "user-1:user-2", "conv_abc", "Ξένος:Ünïcode"}
	d, _ := newDeliverer(t, MemberListerFunc(func(_ context.Context, _, id string) ([]string, error) {
		return []string{"alice"}, nil
	}), Config{})

	for _, id := range ids {
		res, err := d.Send(context.Background(), SendRequest{
			Kind: KindDirect, ConversationID: id, SenderID: "alice", Payload: []byte(id),
		})
		if err != nil {
			t.Errorf("send to %q: %v", id, err)
			continue
		}
		recs, err := d.History(KindDirect, id, res.Seq, 1)
		if err != nil || len(recs) != 1 || string(recs[0].Payload) != id {
			t.Errorf("round trip of %q failed: %v, %v", id, recs, err)
		}
	}
}

func TestStrategyForBoundary(t *testing.T) {
	d, _ := newDeliverer(t, staticMembers{}, Config{FanoutOnWriteLimit: 5})
	for n, want := range map[int]Strategy{
		0: StrategyOnWrite, 1: StrategyOnWrite, 5: StrategyOnWrite,
		6: StrategyOnRead, 1000: StrategyOnRead,
	} {
		if got := d.StrategyFor(n); got != want {
			t.Errorf("StrategyFor(%d) = %s, want %s", n, got, want)
		}
	}
}

// --- Replication durability on the send path ---

// blockingWaiter stands in for a follower that never acknowledges.
type blockingWaiter struct {
	consulted chan struct{}
	seqs      chan uint64
}

func newBlockingWaiter() *blockingWaiter {
	return &blockingWaiter{
		consulted: make(chan struct{}, 16),
		seqs:      make(chan uint64, 16),
	}
}

func (b *blockingWaiter) WaitFor(ctx context.Context, _ string, _ int32, seq uint64) error {
	select {
	case b.consulted <- struct{}{}:
	default:
	}
	select {
	case b.seqs <- seq:
	default:
	}
	<-ctx.Done()
	return ctx.Err()
}

// A send must go through the quorum-aware append path. Regression test for a
// bug where Send used Log.Append instead of Log.AppendContext, which made
// min_in_sync silently ineffective: messages were acknowledged from the
// leader's page cache alone while the configuration and startup logs both
// claimed replicated durability.
func TestSendWaitsForReplication(t *testing.T) {
	l, err := stream.OpenLog(stream.LogConfig{
		Dir: t.TempDir(), DefaultTopic: stream.TopicConfig{Partitions: 1},
	})
	if err != nil {
		t.Fatalf("open log: %v", err)
	}
	defer l.Close()

	w := newBlockingWaiter()
	l.SetAckWaiter(w)

	d, err := New(Options{Log: l, Members: staticMembers{"g1": {"alice"}}})
	if err != nil {
		t.Fatalf("deliverer: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()

	start := time.Now()
	_, err = d.Send(ctx, SendRequest{
		Kind: KindGroup, ConversationID: "g1", SenderID: "alice", Payload: []byte("x"),
	})
	elapsed := time.Since(start)

	select {
	case <-w.consulted:
	default:
		t.Fatalf("send returned in %v without consulting the AckWaiter — "+
			"quorum durability is configured but not enforced", elapsed)
	}
	if err == nil {
		t.Error("a send whose replication never completed reported success")
	}
	if elapsed < 200*time.Millisecond {
		t.Errorf("send returned after %v — it did not actually wait", elapsed)
	}
}

func TestSendWaitsForTheConversationRecordNotThePointers(t *testing.T) {
	l, _ := stream.OpenLog(stream.LogConfig{
		Dir: t.TempDir(), DefaultTopic: stream.TopicConfig{Partitions: 1},
	})
	defer l.Close()

	w := newBlockingWaiter()
	l.SetAckWaiter(w)

	// Ten recipients: if pointer writes also waited for quorum, send latency
	// would be multiplied by the group size.
	members := make([]string, 10)
	for i := range members {
		members[i] = fmt.Sprintf("u%d", i)
	}
	d, _ := New(Options{Log: l, Members: staticMembers{"g1": members}})

	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	d.Send(ctx, SendRequest{
		Kind: KindGroup, ConversationID: "g1", SenderID: "u0", Payload: []byte("x"),
	})

	// Exactly one wait: the conversation record. Pointers are a derived index
	// and are rebuildable, so they are not worth the latency.
	waits := len(w.seqs)
	if waits != 1 {
		t.Errorf("the waiter was consulted %d times, want exactly 1 (the conversation record)", waits)
	}
}

func TestSendSucceedsWhenReplicationSucceeds(t *testing.T) {
	l, _ := stream.OpenLog(stream.LogConfig{
		Dir: t.TempDir(), DefaultTopic: stream.TopicConfig{Partitions: 1},
	})
	defer l.Close()

	var consulted int
	l.SetAckWaiter(ackFunc(func(context.Context, string, int32, uint64) error {
		consulted++
		return nil
	}))

	d, _ := New(Options{Log: l, Members: staticMembers{"g1": {"alice"}}})
	res, err := d.Send(context.Background(), SendRequest{
		Kind: KindGroup, ConversationID: "g1", SenderID: "alice", Payload: []byte("x"),
	})
	if err != nil {
		t.Fatalf("send with a healthy quorum: %v", err)
	}
	if res.Seq != 1 {
		t.Errorf("seq = %d", res.Seq)
	}
	if consulted != 1 {
		t.Errorf("waiter consulted %d times, want 1", consulted)
	}
}

type ackFunc func(ctx context.Context, topic string, partition int32, seq uint64) error

func (f ackFunc) WaitFor(ctx context.Context, topic string, partition int32, seq uint64) error {
	return f(ctx, topic, partition, seq)
}
