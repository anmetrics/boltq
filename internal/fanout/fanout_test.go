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

type staticMembers map[string][]string

func (s staticMembers) Members(_ context.Context, _, groupID string) ([]string, error) {
	return s[groupID], nil
}

func newDeliverer(t *testing.T, members MemberLister, cfg Config) (*Deliverer, *stream.Log) {
	t.Helper()

	l, err := stream.OpenLog(stream.LogConfig{
		Dir:          t.TempDir(),
		DefaultTopic: stream.TopicConfig{Partitions: 4},
	})
	if err != nil {
		t.Fatalf("open log: %v", err)
	}
	t.Cleanup(func() { l.Close() })

	tbl := dedup.New(dedup.Config{TTL: time.Minute, SweepInterval: time.Hour})
	t.Cleanup(tbl.Close)

	pres := presence.New(presence.Config{NodeID: "n1", SweepInterval: time.Hour})
	t.Cleanup(pres.Close)

	d, err := New(Options{Log: l, Members: members, Presence: presence.LocalLookup{Registry: pres}, Dedup: tbl, Config: cfg})
	if err != nil {
		t.Fatalf("new deliverer: %v", err)
	}
	return d, l
}

func TestTopicNaming(t *testing.T) {
	if got := ConversationTopic(KindDirect, "c1"); got != "chat.direct.c1" {
		t.Errorf("direct topic = %q", got)
	}
	if got := ConversationTopic(KindGroup, "g1"); got != "chat.group.g1" {
		t.Errorf("group topic = %q", got)
	}
	if got := InboxTopic("alice"); got != "chat.inbox.alice" {
		t.Errorf("inbox topic = %q", got)
	}

	kind, id, ok := ConversationFromTopic("chat.group.eng")
	if !ok || kind != KindGroup || id != "eng" {
		t.Errorf("parse group: %v %v %v", kind, id, ok)
	}
	kind, id, ok = ConversationFromTopic("chat.direct.alice:bob")
	if !ok || kind != KindDirect || id != "alice:bob" {
		t.Errorf("parse direct: %v %v %v", kind, id, ok)
	}
	if _, _, ok := ConversationFromTopic("chat.inbox.alice"); ok {
		t.Error("inbox topic parsed as a conversation")
	}
}

func TestSendWritesConversationAndInboxes(t *testing.T) {
	members := staticMembers{"g1": {"alice", "bob", "carol"}}
	d, l := newDeliverer(t, members, Config{})

	res, err := d.Send(context.Background(), SendRequest{
		Kind: KindGroup, ConversationID: "g1", SenderID: "alice",
		ClientMsgID: "c1", Payload: []byte("hello everyone"),
	})
	if err != nil {
		t.Fatalf("send: %v", err)
	}
	if res.Strategy != StrategyOnWrite {
		t.Errorf("strategy = %s, want fan-out on write for 3 members", res.Strategy)
	}
	if res.InboxWrites != 3 {
		t.Errorf("wrote %d inbox pointers, want 3", res.InboxWrites)
	}
	if res.Seq != 1 {
		t.Errorf("first message got seq %d", res.Seq)
	}

	// The conversation log holds the real body.
	convRecs, _, err := l.ReadByKey("chat.group.g1", []byte("g1"), 1, 10)
	if err != nil {
		t.Fatalf("read conversation: %v", err)
	}
	if len(convRecs) != 1 || string(convRecs[0].Payload) != "hello everyone" {
		t.Fatalf("conversation record wrong: %+v", convRecs)
	}
	if convRecs[0].Headers[HeaderSender] != "alice" {
		t.Errorf("sender header missing: %v", convRecs[0].Headers)
	}

	// Each inbox holds a pointer with no body.
	for _, member := range []string{"alice", "bob", "carol"} {
		recs, err := d.Backlog(member, 1, 10)
		if err != nil {
			t.Fatalf("backlog %s: %v", member, err)
		}
		if len(recs) != 1 {
			t.Fatalf("%s has %d inbox records, want 1", member, len(recs))
		}
		p := recs[0]
		if len(p.Payload) != 0 {
			t.Errorf("%s: inbox pointer carries a payload — body was duplicated", member)
		}
		if p.Headers[HeaderPointer] != "1" {
			t.Errorf("%s: pointer flag missing", member)
		}
		if p.Headers[HeaderMessageID] != res.MessageID {
			t.Errorf("%s: pointer references %q, want %q", member, p.Headers[HeaderMessageID], res.MessageID)
		}
		seq, _ := strconv.ParseUint(p.Headers[HeaderConvSeq], 10, 64)
		if seq != res.Seq {
			t.Errorf("%s: pointer seq %d, want %d", member, seq, res.Seq)
		}
	}
}

func TestLargeGroupUsesFanoutOnRead(t *testing.T) {
	big := make([]string, 500)
	for i := range big {
		big[i] = fmt.Sprintf("u%d", i)
	}
	d, _ := newDeliverer(t, staticMembers{"big": big}, Config{FanoutOnWriteLimit: 100})

	res, err := d.Send(context.Background(), SendRequest{
		Kind: KindGroup, ConversationID: "big", SenderID: "u0", Payload: []byte("hi"),
	})
	if err != nil {
		t.Fatalf("send: %v", err)
	}

	// The critical property: one "hi" in a big channel must not become 500
	// writes.
	if res.Strategy != StrategyOnRead {
		t.Errorf("strategy = %s, want fan-out on read above the limit", res.Strategy)
	}
	if res.InboxWrites != 0 {
		t.Errorf("wrote %d inbox pointers for a large group, want 0", res.InboxWrites)
	}

	// The conversation log still has the message, so members reading directly
	// see it.
	recs, err := d.History(KindGroup, "big", 1, 10)
	if err != nil || len(recs) != 1 {
		t.Fatalf("conversation history: %v %v", recs, err)
	}
}

func TestStrategyBoundary(t *testing.T) {
	d, _ := newDeliverer(t, staticMembers{}, Config{FanoutOnWriteLimit: 10})
	if got := d.StrategyFor(10); got != StrategyOnWrite {
		t.Errorf("at the limit: %s", got)
	}
	if got := d.StrategyFor(11); got != StrategyOnRead {
		t.Errorf("past the limit: %s", got)
	}
}

func TestPerConversationOrdering(t *testing.T) {
	d, _ := newDeliverer(t, staticMembers{"g1": {"alice", "bob"}}, Config{})
	ctx := context.Background()

	// Two senders hammering the same conversation. Every message must land in
	// one total order, and reading it back must reproduce that order exactly.
	var wg sync.WaitGroup
	for _, sender := range []string{"alice", "bob"} {
		wg.Add(1)
		go func(sender string) {
			defer wg.Done()
			for i := 0; i < 50; i++ {
				_, err := d.Send(ctx, SendRequest{
					Kind: KindGroup, ConversationID: "g1", SenderID: sender,
					ClientMsgID: fmt.Sprintf("%s-%d", sender, i),
					Payload:     []byte(fmt.Sprintf("%s-%d", sender, i)),
				})
				if err != nil {
					t.Errorf("send: %v", err)
					return
				}
			}
		}(sender)
	}
	wg.Wait()

	recs, err := d.History(KindGroup, "g1", 1, 500)
	if err != nil {
		t.Fatalf("history: %v", err)
	}
	if len(recs) != 100 {
		t.Fatalf("got %d messages, want 100", len(recs))
	}
	for i, r := range recs {
		if r.Seq != uint64(i+1) {
			t.Fatalf("record %d has seq %d — the conversation log is not totally ordered", i, r.Seq)
		}
	}
}

func TestDuplicateSendReturnsOriginal(t *testing.T) {
	d, _ := newDeliverer(t, staticMembers{"g1": {"alice", "bob"}}, Config{})
	ctx := context.Background()

	req := SendRequest{
		Kind: KindGroup, ConversationID: "g1", SenderID: "alice",
		ClientMsgID: "retry-me", Payload: []byte("only once"),
	}

	first, err := d.Send(ctx, req)
	if err != nil {
		t.Fatalf("first send: %v", err)
	}
	second, err := d.Send(ctx, req)
	if err != nil {
		t.Fatalf("retry: %v", err)
	}

	if !second.Duplicate {
		t.Error("retry was not flagged as a duplicate")
	}
	if second.Seq != first.Seq || second.MessageID != first.MessageID {
		t.Errorf("retry got different coordinates: %+v vs %+v", second, first)
	}

	// And the conversation must hold exactly one copy.
	recs, _ := d.History(KindGroup, "g1", 1, 10)
	if len(recs) != 1 {
		t.Fatalf("conversation holds %d copies of a retried message", len(recs))
	}
}

func TestSendWithoutClientMsgIDIsNotDeduplicated(t *testing.T) {
	d, _ := newDeliverer(t, staticMembers{"g1": {"alice"}}, Config{})
	ctx := context.Background()

	req := SendRequest{Kind: KindGroup, ConversationID: "g1", SenderID: "alice", Payload: []byte("x")}
	d.Send(ctx, req)
	d.Send(ctx, req)

	// Without an idempotency key the server cannot tell a retry from a genuine
	// second message, and must store both.
	recs, _ := d.History(KindGroup, "g1", 1, 10)
	if len(recs) != 2 {
		t.Errorf("got %d messages, want 2 — a message with no client_msg_id must not be collapsed", len(recs))
	}
}

func TestFailedSendReleasesClaim(t *testing.T) {
	failing := MemberListerFunc(func(context.Context, string, string) ([]string, error) {
		return nil, errors.New("membership service down")
	})
	d, _ := newDeliverer(t, failing, Config{})
	ctx := context.Background()

	req := SendRequest{
		Kind: KindGroup, ConversationID: "g1", SenderID: "alice",
		ClientMsgID: "c1", Payload: []byte("x"),
	}
	if _, err := d.Send(ctx, req); err == nil {
		t.Fatal("send should have failed")
	}

	// The retry must be treated as a fresh attempt: nothing was stored, so
	// deduplicating it would lose the message permanently.
	if _, err := d.Send(ctx, req); err == nil {
		t.Fatal("second attempt should also fail")
	} else if errors.Is(err, ErrInFlight) {
		t.Error("a retry after a failed send was blocked as in-flight — the claim leaked")
	}
}

func TestSendToEmptyConversation(t *testing.T) {
	d, _ := newDeliverer(t, staticMembers{}, Config{})

	_, err := d.Send(context.Background(), SendRequest{
		Kind: KindGroup, ConversationID: "ghost", SenderID: "alice", Payload: []byte("x"),
	})
	if !errors.Is(err, ErrNoMembers) {
		t.Errorf("got %v, want ErrNoMembers", err)
	}
}

func TestSendRequiresConversationID(t *testing.T) {
	d, _ := newDeliverer(t, staticMembers{}, Config{})
	if _, err := d.Send(context.Background(), SendRequest{SenderID: "alice"}); !errors.Is(err, ErrEmptyConversation) {
		t.Errorf("got %v, want ErrEmptyConversation", err)
	}
}

func TestOfflineUsersExcludesSender(t *testing.T) {
	members := staticMembers{"g1": {"alice", "bob", "carol"}}
	d, _ := newDeliverer(t, members, Config{})

	res, err := d.Send(context.Background(), SendRequest{
		Kind: KindGroup, ConversationID: "g1", SenderID: "alice", Payload: []byte("x"),
	})
	if err != nil {
		t.Fatalf("send: %v", err)
	}

	// Nobody is bound, so bob and carol are offline — but alice, the sender,
	// must never be listed for a push about her own message.
	if len(res.OfflineUsers) != 2 {
		t.Fatalf("offline users = %v, want bob and carol", res.OfflineUsers)
	}
	for _, u := range res.OfflineUsers {
		if u == "alice" {
			t.Error("the sender was queued for a push notification about their own message")
		}
	}
}

func TestDirectConversationRouting(t *testing.T) {
	d, _ := newDeliverer(t, staticMembers{"alice:bob": {"alice", "bob"}}, Config{})

	res, err := d.Send(context.Background(), SendRequest{
		Kind: KindDirect, ConversationID: "alice:bob", SenderID: "alice", Payload: []byte("hi"),
	})
	if err != nil {
		t.Fatalf("send: %v", err)
	}
	if res.Topic != "chat.direct.alice:bob" {
		t.Errorf("topic = %q", res.Topic)
	}

	recs, err := d.History(KindDirect, "alice:bob", 1, 10)
	if err != nil || len(recs) != 1 {
		t.Fatalf("history: %v %v", recs, err)
	}
}

func TestBacklogPagination(t *testing.T) {
	d, _ := newDeliverer(t, staticMembers{"g1": {"alice", "bob"}}, Config{})
	ctx := context.Background()

	for i := 0; i < 25; i++ {
		d.Send(ctx, SendRequest{
			Kind: KindGroup, ConversationID: "g1", SenderID: "alice",
			ClientMsgID: fmt.Sprint(i), Payload: []byte(fmt.Sprint(i)),
		})
	}

	page1, err := d.Backlog("bob", 1, 10)
	if err != nil {
		t.Fatalf("backlog: %v", err)
	}
	if len(page1) != 10 || page1[0].Seq != 1 {
		t.Fatalf("page 1: %d records starting at %d", len(page1), page1[0].Seq)
	}

	page2, _ := d.Backlog("bob", page1[9].Seq+1, 10)
	if len(page2) != 10 || page2[0].Seq != 11 {
		t.Fatalf("page 2: %d records starting at %d", len(page2), page2[0].Seq)
	}
}

func BenchmarkSendSmallGroup(b *testing.B) {
	l, _ := stream.OpenLog(stream.LogConfig{Dir: b.TempDir(), DefaultTopic: stream.TopicConfig{Partitions: 4}})
	defer l.Close()

	d, _ := New(Options{Log: l, Members: staticMembers{"g1": {"a", "b", "c", "d"}}})
	ctx := context.Background()

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		d.Send(ctx, SendRequest{
			Kind: KindGroup, ConversationID: "g1", SenderID: "a",
			Payload: []byte("a typical chat message"),
		})
	}
}
