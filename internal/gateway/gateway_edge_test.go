package gateway

import (
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"

	"github.com/boltq/boltq/internal/ephemeral"
	"github.com/boltq/boltq/internal/identity"
	"github.com/boltq/boltq/internal/stream"
)

// Harness variants used by the edge cases below.

func newHarnessWith(t *testing.T, cfg Config) *harness {
	t.Helper()
	return newHarnessOpts(t, harnessOpts{cfg: cfg})
}

func newHarnessWithAPIKey(t *testing.T, key string, allowAnonymous bool) *harness {
	t.Helper()
	return newHarnessOpts(t, harnessOpts{
		cfg:            Config{PongTimeout: 5 * time.Second, ResumeWindow: time.Minute},
		apiKey:         key,
		allowAnonymous: allowAnonymous,
	})
}

func newHarnessWithPresenceTTL(t *testing.T, ttl time.Duration) *harness {
	t.Helper()
	return newHarnessOpts(t, harnessOpts{
		cfg:         Config{PongTimeout: 5 * time.Second, ResumeWindow: time.Minute},
		presenceTTL: ttl,
	})
}

// newTightHub is a signal hub with a rate limit low enough to trip in a test.
func newTightHub(t *testing.T) *ephemeral.Hub {
	t.Helper()
	h := ephemeral.New(ephemeral.Config{RatePerSecond: 1, Burst: 2})
	t.Cleanup(h.Close)
	return h
}

// --- Construction ---

func TestNewValidatesDependencies(t *testing.T) {
	l, _ := stream.OpenLog(stream.LogConfig{Dir: t.TempDir()})
	defer l.Close()
	cursors, _ := stream.OpenCursorStore(t.TempDir())
	defer cursors.Close()
	policy := identity.NewPolicy(identity.PolicyConfig{})
	verifier, _ := identity.NewVerifier(identity.VerifierConfig{Keys: []identity.SigningKey{signKey}})

	cases := []struct {
		name string
		opts Options
	}{
		{"no log", Options{Cursors: cursors, Policy: policy, Verifier: verifier}},
		{"no cursors", Options{Log: l, Policy: policy, Verifier: verifier}},
		{"no policy", Options{Log: l, Cursors: cursors, Verifier: verifier}},
		{"no verifier and no api key", Options{Log: l, Cursors: cursors, Policy: policy}},
	}
	for _, c := range cases {
		if _, err := New(c.opts); err == nil {
			t.Errorf("%s: New succeeded", c.name)
		}
	}

	// An API key alone is enough — that is the trusted-backend path.
	if _, err := New(Options{Log: l, Cursors: cursors, Policy: policy, APIKey: "k"}); err != nil {
		t.Errorf("API-key-only gateway rejected: %v", err)
	}
}

func TestConfigDefaultsApplied(t *testing.T) {
	l, _ := stream.OpenLog(stream.LogConfig{Dir: t.TempDir()})
	defer l.Close()
	cursors, _ := stream.OpenCursorStore(t.TempDir())
	defer cursors.Close()
	verifier, _ := identity.NewVerifier(identity.VerifierConfig{Keys: []identity.SigningKey{signKey}})

	g, err := New(Options{
		Log: l, Cursors: cursors, Verifier: verifier,
		Policy: identity.NewPolicy(identity.PolicyConfig{}),
	})
	if err != nil {
		t.Fatalf("new: %v", err)
	}
	defer g.Close()

	if g.cfg.ReadLimit == 0 || g.cfg.WriteTimeout == 0 || g.cfg.PongTimeout == 0 ||
		g.cfg.SendBuffer == 0 || g.cfg.MaxSubscriptions == 0 || g.cfg.HistoryLimit == 0 {
		t.Errorf("defaults not applied: %+v", g.cfg)
	}
	if g.cfg.PingInterval >= g.cfg.PongTimeout {
		t.Errorf("ping interval %v must be well under pong timeout %v",
			g.cfg.PingInterval, g.cfg.PongTimeout)
	}
}

// --- Origin checking ---

func TestOriginAllowedWhenUnconfigured(t *testing.T) {
	h := newHarness(t)
	url := "ws" + strings.TrimPrefix(h.server.URL, "http") + "?token=" + h.token(t, "alice", "phone")

	hdr := http.Header{"Origin": []string{"https://evil.example.com"}}
	conn, _, err := websocket.DefaultDialer.Dial(url, hdr)
	if err != nil {
		t.Fatalf("empty allowlist should permit any origin: %v", err)
	}
	conn.Close()
}

func TestOriginAllowlist(t *testing.T) {
	h := newHarnessWith(t, Config{
		PongTimeout:    5 * time.Second,
		ResumeWindow:   time.Minute,
		AllowedOrigins: []string{"https://app.example.com"},
	})
	url := "ws" + strings.TrimPrefix(h.server.URL, "http") + "?token=" + h.token(t, "alice", "phone")

	// Allowed, and case-insensitive.
	for _, origin := range []string{"https://app.example.com", "HTTPS://APP.EXAMPLE.COM"} {
		conn, _, err := websocket.DefaultDialer.Dial(url, http.Header{"Origin": []string{origin}})
		if err != nil {
			t.Errorf("origin %q rejected: %v", origin, err)
			continue
		}
		conn.Close()
	}

	// Denied.
	if _, _, err := websocket.DefaultDialer.Dial(url,
		http.Header{"Origin": []string{"https://evil.example.com"}}); err == nil {
		t.Error("a disallowed origin was accepted")
	}

	// A native client sends no Origin at all and must still connect.
	conn, _, err := websocket.DefaultDialer.Dial(url, nil)
	if err != nil {
		t.Errorf("a client with no Origin header was rejected: %v", err)
	} else {
		conn.Close()
	}
}

// --- Shared API key path ---

func TestSharedAPIKeyAuthenticatesAsAnonymous(t *testing.T) {
	h := newHarnessWithAPIKey(t, "backend-secret", true)
	url := "ws" + strings.TrimPrefix(h.server.URL, "http") + "?token=backend-secret"

	conn, _, err := websocket.DefaultDialer.Dial(url, nil)
	if err != nil {
		t.Fatalf("shared API key rejected: %v", err)
	}
	defer conn.Close()

	c := &client{t: t, conn: conn}
	w := c.hello("backend-1")
	if w.Session == "" {
		t.Fatal("no session for the shared-key connection")
	}

	// With AllowAnonymous on, a backend may touch any inbox.
	c.send(Frame{Op: OpSubscribe, ID: "s1", Topic: "chat.inbox.anyone"})
	c.readOp(OpAck, 3*time.Second)
}

func TestSharedAPIKeyDeniedWhenAnonymousDisallowed(t *testing.T) {
	h := newHarnessWithAPIKey(t, "backend-secret", false)
	url := "ws" + strings.TrimPrefix(h.server.URL, "http") + "?token=backend-secret"

	conn, _, err := websocket.DefaultDialer.Dial(url, nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()

	c := &client{t: t, conn: conn}
	c.hello("backend-1")
	c.send(Frame{Op: OpSubscribe, ID: "s1", Topic: "chat.inbox.anyone"})

	f := c.read(3 * time.Second)
	if f.Op != OpError || f.Error.Code != CodeForbidden {
		t.Fatalf("anonymous access allowed while AllowAnonymous is off: %+v", f)
	}
}

// --- Subscribe / unsubscribe ---

func TestUnsubscribeStopsDelivery(t *testing.T) {
	h := newHarness(t)
	h.members.Add("", "g1", "alice")

	c := h.dial(t, h.token(t, "alice", "phone"))
	c.hello("phone")

	topic, _ := h.log.GetOrCreateTopic("chat.group.g1")
	partition := topic.PartitionForKey([]byte("g1"))

	c.send(Frame{Op: OpSubscribe, ID: "s1", Topic: "chat.group.g1", Partition: &partition, FromSeq: 1})
	c.readOp(OpAck, 3*time.Second)

	c.send(Frame{Op: OpSend, ID: "m1", Kind: "group", Conversation: "g1",
		ClientMsgID: "c1", Payload: []byte("first")})
	c.readOp(OpSent, 3*time.Second)
	c.readOp(OpRecord, 3*time.Second)

	c.send(Frame{Op: OpUnsubscribe, ID: "u1", Topic: "chat.group.g1", Partition: &partition})
	c.readOp(OpAck, 3*time.Second)

	c.send(Frame{Op: OpSend, ID: "m2", Kind: "group", Conversation: "g1",
		ClientMsgID: "c2", Payload: []byte("second")})
	c.readOp(OpSent, 3*time.Second)

	// No record frame should follow the unsubscribe.
	if f, ok := c.tryRead(500 * time.Millisecond); ok && f.Op == OpRecord {
		t.Errorf("records still delivered after unsubscribe: %+v", f.Records)
	}
}

func TestUnsubscribeUnknownIsAcked(t *testing.T) {
	h := newHarness(t)
	c := h.dial(t, h.token(t, "alice", "phone"))
	c.hello("phone")

	// Idempotent: unsubscribing from something you never had is not an error.
	c.send(Frame{Op: OpUnsubscribe, ID: "u1", Topic: "chat.inbox.alice"})
	c.readOp(OpAck, 3*time.Second)
}

func TestResubscribeReplacesExisting(t *testing.T) {
	h := newHarness(t)
	h.members.Add("", "g1", "alice")

	c := h.dial(t, h.token(t, "alice", "phone"))
	c.hello("phone")

	topic, _ := h.log.GetOrCreateTopic("chat.group.g1")
	partition := topic.PartitionForKey([]byte("g1"))

	for i := 0; i < 3; i++ {
		c.send(Frame{Op: OpSubscribe, ID: "s", Topic: "chat.group.g1",
			Partition: &partition, FromSeq: 1})
		c.readOp(OpAck, 3*time.Second)
	}

	c.send(Frame{Op: OpSend, ID: "m1", Kind: "group", Conversation: "g1",
		ClientMsgID: "c1", Payload: []byte("once")})
	c.readOp(OpSent, 3*time.Second)

	rec := c.readOp(OpRecord, 3*time.Second)
	if len(rec.Records) != 1 {
		t.Fatalf("got %d records", len(rec.Records))
	}

	// Three subscribes must not mean three copies of every message.
	if f, ok := c.tryRead(500 * time.Millisecond); ok && f.Op == OpRecord {
		t.Error("a duplicate record was delivered — the old subscription survived")
	}
}

func TestSubscribeMissingTopic(t *testing.T) {
	h := newHarness(t)
	c := h.dial(t, h.token(t, "alice", "phone"))
	c.hello("phone")

	c.send(Frame{Op: OpSubscribe, ID: "s1"})
	f := c.read(3 * time.Second)
	if f.Op != OpError || f.Error.Code != CodeBadRequest {
		t.Fatalf("subscribe with no topic: %+v", f)
	}
}

func TestSubscribeOutOfRangePartition(t *testing.T) {
	h := newHarness(t)
	c := h.dial(t, h.token(t, "alice", "phone"))
	c.hello("phone")

	h.log.CreateTopic("chat.inbox.alice", stream.TopicConfig{Partitions: 2})
	p := int32(99)
	c.send(Frame{Op: OpSubscribe, ID: "s1", Topic: "chat.inbox.alice", Partition: &p})

	f := c.read(3 * time.Second)
	if f.Op != OpError || f.Error.Code != CodeNotFound {
		t.Fatalf("out-of-range partition: %+v", f)
	}
}

func TestSubscribeReportsGapAfterRetention(t *testing.T) {
	h := newHarness(t)

	// Build a topic whose history has been trimmed.
	topic, _ := h.log.CreateTopic("chat.inbox.alice", stream.TopicConfig{
		Partitions: 1,
		Partition:  stream.PartitionConfig{SegmentBytes: 512, RetentionBytes: 1024},
	})
	p, _ := topic.Partition(0)
	for i := 0; i < 400; i++ {
		p.Append(&stream.Record{Key: []byte("alice"), Payload: []byte("old message body")})
	}
	if _, err := p.EnforceRetention(); err != nil {
		t.Fatalf("retention: %v", err)
	}
	if p.FirstSeq() == 1 {
		t.Skip("retention removed nothing on this run")
	}

	c := h.dial(t, h.token(t, "alice", "phone"))
	c.hello("phone")

	pid := int32(0)
	c.send(Frame{Op: OpSubscribe, ID: "s1", Topic: "chat.inbox.alice", Partition: &pid, FromSeq: 1})

	f := c.readOp(OpGap, 3*time.Second)
	if f.Error == nil || f.Error.Code != CodeGap {
		t.Fatalf("gap frame missing its error detail: %+v", f)
	}
	if f.FirstSeq != p.FirstSeq() {
		t.Errorf("gap reports first_seq %d, partition says %d", f.FirstSeq, p.FirstSeq())
	}
}

// --- History ---

func TestHistoryMissingTopic(t *testing.T) {
	h := newHarness(t)
	c := h.dial(t, h.token(t, "alice", "phone"))
	c.hello("phone")

	c.send(Frame{Op: OpHistory, ID: "h1"})
	if f := c.read(3 * time.Second); f.Op != OpError || f.Error.Code != CodeBadRequest {
		t.Fatalf("history with no topic: %+v", f)
	}

	c.send(Frame{Op: OpHistory, ID: "h2", Topic: "chat.inbox.alice"})
	if f := c.read(3 * time.Second); f.Op != OpError || f.Error.Code != CodeNotFound {
		t.Fatalf("history for a nonexistent topic: %+v", f)
	}
}

func TestHistoryLimitIsCapped(t *testing.T) {
	h := newHarness(t)
	h.gw.cfg.HistoryLimit = 5
	h.members.Add("", "g1", "alice")

	c := h.dial(t, h.token(t, "alice", "phone"))
	c.hello("phone")

	for i := 0; i < 20; i++ {
		c.send(Frame{Op: OpSend, ID: "m", Kind: "group", Conversation: "g1",
			ClientMsgID: string(rune('a' + i)), Payload: []byte("x")})
		c.readOp(OpSent, 3*time.Second)
	}

	// A client asking for 1000 must not be able to make the server read 1000.
	c.send(Frame{Op: OpHistory, ID: "h1", Topic: "chat.group.g1", FromSeq: 1, Limit: 1000})
	f := c.readOp(OpRecord, 3*time.Second)
	if len(f.Records) > 5 {
		t.Errorf("history returned %d records against a cap of 5", len(f.Records))
	}
}

func TestHistoryPagination(t *testing.T) {
	h := newHarness(t)
	h.members.Add("", "g1", "alice")

	c := h.dial(t, h.token(t, "alice", "phone"))
	c.hello("phone")

	for i := 0; i < 25; i++ {
		c.send(Frame{Op: OpSend, ID: "m", Kind: "group", Conversation: "g1",
			ClientMsgID: string(rune('a' + i)), Payload: []byte{byte('0' + i%10)}})
		c.readOp(OpSent, 3*time.Second)
	}

	var seen []uint64
	from := uint64(1)
	for page := 0; page < 5; page++ {
		c.send(Frame{Op: OpHistory, ID: "h", Topic: "chat.group.g1", FromSeq: from, Limit: 10})
		f := c.readOp(OpRecord, 3*time.Second)
		if len(f.Records) == 0 {
			break
		}
		for _, r := range f.Records {
			seen = append(seen, r.Seq)
		}
		from = f.Records[len(f.Records)-1].Seq + 1
	}

	if len(seen) != 25 {
		t.Fatalf("pagination yielded %d records, want 25", len(seen))
	}
	for i, s := range seen {
		if s != uint64(i+1) {
			t.Fatalf("page boundary lost or repeated a record at index %d: seq %d", i, s)
		}
	}
}

func TestHistoryDeniedForOtherUser(t *testing.T) {
	h := newHarness(t)
	h.log.GetOrCreateTopic("chat.inbox.bob")

	c := h.dial(t, h.token(t, "alice", "phone"))
	c.hello("phone")

	c.send(Frame{Op: OpHistory, ID: "h1", Topic: "chat.inbox.bob", FromSeq: 1})
	f := c.read(3 * time.Second)
	if f.Op != OpError || f.Error.Code != CodeForbidden {
		t.Fatalf("alice read bob's inbox history: %+v", f)
	}
}

// --- Commit ---

func TestCommitValidation(t *testing.T) {
	h := newHarness(t)
	c := h.dial(t, h.token(t, "alice", "phone"))
	c.hello("phone")

	for _, f := range []Frame{
		{Op: OpCommit, ID: "c1", Seq: 10},                   // no topic
		{Op: OpCommit, ID: "c2", Topic: "chat.inbox.alice"}, // no seq
		{Op: OpCommit, ID: "c3", Topic: "chat.inbox.alice", Seq: 0},
	} {
		c.send(f)
		got := c.read(3 * time.Second)
		if got.Op != OpError || got.Error.Code != CodeBadRequest {
			t.Errorf("commit %s was accepted: %+v", f.ID, got)
		}
	}
}

func TestCommitDeniedForOtherUser(t *testing.T) {
	h := newHarness(t)
	c := h.dial(t, h.token(t, "alice", "phone"))
	c.hello("phone")

	c.send(Frame{Op: OpCommit, ID: "c1", Topic: "chat.inbox.bob", Seq: 10})
	f := c.read(3 * time.Second)
	if f.Op != OpError || f.Error.Code != CodeForbidden {
		t.Fatalf("alice committed a cursor on bob's inbox: %+v", f)
	}
}

func TestCommitIsMonotonicOverTheWire(t *testing.T) {
	h := newHarness(t)
	c := h.dial(t, h.token(t, "alice", "phone"))
	c.hello("phone")

	key := stream.CursorKey{Topic: "chat.inbox.alice", Partition: 0,
		Group: "user:alice", Member: "phone"}

	c.send(Frame{Op: OpCommit, ID: "c1", Topic: "chat.inbox.alice", Seq: 100})
	c.readOp(OpAck, 3*time.Second)

	// A stale in-flight commit arriving late must be acked but not applied.
	c.send(Frame{Op: OpCommit, ID: "c2", Topic: "chat.inbox.alice", Seq: 50})
	c.readOp(OpAck, 3*time.Second)

	if seq, _ := h.cursors.Position(key); seq != 100 {
		t.Errorf("cursor rewound to %d — a stale commit was applied", seq)
	}
}

// --- Send ---

func TestSendValidation(t *testing.T) {
	h := newHarness(t)
	c := h.dial(t, h.token(t, "alice", "phone"))
	c.hello("phone")

	c.send(Frame{Op: OpSend, ID: "m1", Payload: []byte("x")})
	f := c.read(3 * time.Second)
	if f.Op != OpError || f.Error.Code != CodeBadRequest {
		t.Fatalf("send with no conversation: %+v", f)
	}
}

func TestSendToEmptyConversationReturnsNotFound(t *testing.T) {
	h := newHarness(t)
	// Alice is authorised (direct IDs derive membership) but the conversation
	// resolves to no members through the group lister.
	c := h.dial(t, h.token(t, "alice", "phone"))
	c.hello("phone")

	c.send(Frame{Op: OpSend, ID: "m1", Kind: "group", Conversation: "ghost", Payload: []byte("x")})
	f := c.read(3 * time.Second)
	if f.Op != OpError {
		t.Fatalf("send to an empty conversation succeeded: %+v", f)
	}
	if f.Error.Code != CodeForbidden && f.Error.Code != CodeNotFound {
		t.Errorf("unexpected code %s", f.Error.Code)
	}
}

func TestSendBinaryPayloadIsPreserved(t *testing.T) {
	h := newHarness(t)
	h.members.Add("", "g1", "alice", "bob")

	bob := h.dial(t, h.token(t, "bob", "bob-phone"))
	bob.hello("bob-phone")
	bob.send(Frame{Op: OpSubscribe, ID: "s1", Topic: "chat.group.g1", FromSeq: 1})
	bob.readOp(OpAck, 3*time.Second)

	// Ciphertext: arbitrary bytes including nulls and invalid UTF-8.
	payload := []byte{0x00, 0xFF, 0xFE, 0x01, 0x80, 0x7F, 0x00}

	alice := h.dial(t, h.token(t, "alice", "alice-phone"))
	alice.hello("alice-phone")
	alice.send(Frame{Op: OpSend, ID: "m1", Kind: "group", Conversation: "g1",
		ClientMsgID: "c1", Payload: payload})
	alice.readOp(OpSent, 3*time.Second)

	rec := bob.readOp(OpRecord, 3*time.Second)
	got := rec.Records[0].Payload
	if len(got) != len(payload) {
		t.Fatalf("payload length changed: %d -> %d", len(payload), len(got))
	}
	for i := range payload {
		if got[i] != payload[i] {
			t.Fatalf("payload byte %d changed: %#x -> %#x", i, payload[i], got[i])
		}
	}
}

func TestConcurrentSendsFromOneConnection(t *testing.T) {
	h := newHarness(t)
	h.members.Add("", "g1", "alice")

	c := h.dial(t, h.token(t, "alice", "phone"))
	c.hello("phone")

	// The gateway reads frames serially, so pipelining must still produce a
	// response per request and a total order in the log.
	for i := 0; i < 30; i++ {
		c.send(Frame{Op: OpSend, ID: "m", Kind: "group", Conversation: "g1",
			ClientMsgID: strings.Repeat("x", i+1), Payload: []byte("m")})
	}
	seqs := make(map[uint64]bool)
	for i := 0; i < 30; i++ {
		f := c.readOp(OpSent, 5*time.Second)
		if seqs[f.Sent.Seq] {
			t.Fatalf("sequence %d handed out twice", f.Sent.Seq)
		}
		seqs[f.Sent.Seq] = true
	}
	if len(seqs) != 30 {
		t.Errorf("got %d distinct sequences, want 30", len(seqs))
	}
}

// --- Typing ---

func TestTypingValidation(t *testing.T) {
	h := newHarness(t)
	c := h.dial(t, h.token(t, "alice", "phone"))
	c.hello("phone")

	c.send(Frame{Op: OpTyping, ID: "t1"})
	f := c.read(3 * time.Second)
	if f.Op != OpError || f.Error.Code != CodeBadRequest {
		t.Fatalf("typing with no conversation: %+v", f)
	}
}

func TestTypingRateLimitSurfacesAsRetryable(t *testing.T) {
	h := newHarnessOpts(t, harnessOpts{
		cfg:     Config{PongTimeout: 5 * time.Second, ResumeWindow: time.Minute},
		signals: newTightHub(t),
	})
	h.members.Add("", "g1", "alice")
	c := h.dial(t, h.token(t, "alice", "phone"))
	c.hello("phone")

	typing := true
	limited := false
	for i := 0; i < 30; i++ {
		c.send(Frame{Op: OpTyping, ID: "t", Conversation: "g1", Typing: &typing})
		f := c.read(3 * time.Second)
		if f.Op == OpError && f.Error.Code == CodeRateLimited {
			if !f.Error.Retryable {
				t.Error("a rate-limit error was not marked retryable")
			}
			limited = true
			break
		}
	}
	if !limited {
		t.Error("30 rapid typing signals were never rate limited")
	}
}

// --- Presence ---

func TestWatchPresenceDeniedIsEnforced(t *testing.T) {
	h := newHarness(t)

	// Tighten the policy so presence reads require membership.
	rules := identity.ChatPolicyRules()
	for i := range rules {
		if rules[i].Pattern == "presence.#" {
			rules[i].RequireMembership = true
			rules[i].MembershipSegment = 1
		}
	}
	h.gw.policy.SetRules(rules)

	c := h.dial(t, h.token(t, "alice", "phone"))
	c.hello("phone")

	c.send(Frame{Op: OpWatchPresence, ID: "w1", Users: []string{"stranger"}})
	f := c.read(3 * time.Second)
	if f.Op != OpError || f.Error.Code != CodeForbidden {
		t.Fatalf("alice watched a stranger's presence: %+v", f)
	}
}

func TestWatchPresenceReplacesSet(t *testing.T) {
	h := newHarness(t)

	alice := h.dial(t, h.token(t, "alice", "alice-phone"))
	alice.hello("alice-phone")

	alice.send(Frame{Op: OpWatchPresence, ID: "w1", Users: []string{"bob"}})
	alice.readOp(OpAck, 3*time.Second)
	alice.send(Frame{Op: OpWatchPresence, ID: "w2", Users: []string{"carol"}})
	alice.readOp(OpAck, 3*time.Second)

	// Bob is no longer watched; carol is.
	bob := h.dial(t, h.token(t, "bob", "bob-phone"))
	bob.hello("bob-phone")
	carol := h.dial(t, h.token(t, "carol", "carol-phone"))
	carol.hello("carol-phone")

	ev := alice.readOp(OpPresenceEvent, 3*time.Second)
	if ev.Presence.UserID != "carol" {
		t.Errorf("received a presence event for %q — the replaced watch set is still active",
			ev.Presence.UserID)
	}
}

func TestWatchPresenceEmptySetIsAllowed(t *testing.T) {
	h := newHarness(t)
	c := h.dial(t, h.token(t, "alice", "phone"))
	c.hello("phone")

	c.send(Frame{Op: OpWatchPresence, ID: "w1", Users: nil})
	c.readOp(OpAck, 3*time.Second)
}

// --- Session store ---

func TestSessionStoreDropAndGet(t *testing.T) {
	st := NewSessionStore(time.Minute)
	defer st.Close()

	p := &identity.Principal{UserID: "alice"}
	sess, _ := st.Create(p, "phone", "MyApp/1.0")

	got, ok := st.Get(sess.ID)
	if !ok || got.ID != sess.ID || got.UserAgent != "MyApp/1.0" {
		t.Fatalf("Get returned %+v, %v", got, ok)
	}
	if _, ok := st.Get("nonexistent"); ok {
		t.Error("Get found a session that does not exist")
	}

	st.Drop(sess.ID)
	if _, ok := st.Get(sess.ID); ok {
		t.Error("session survived Drop")
	}
	if st.Len() != 0 {
		t.Errorf("Len = %d after Drop", st.Len())
	}
	st.Drop("nonexistent") // must not panic
}

func TestSessionStoreAttachedCount(t *testing.T) {
	st := NewSessionStore(time.Minute)
	defer st.Close()

	p := &identity.Principal{UserID: "alice"}
	a, _ := st.Create(p, "d1", "")
	b, _ := st.Create(p, "d2", "")

	if st.Attached() != 2 {
		t.Errorf("Attached = %d, want 2", st.Attached())
	}
	st.Detach(a)
	if st.Attached() != 1 {
		t.Errorf("Attached after one detach = %d", st.Attached())
	}
	if st.Len() != 2 {
		t.Errorf("Len = %d — a detached session should still be tracked", st.Len())
	}
	st.Detach(b)
	if st.Attached() != 0 {
		t.Errorf("Attached = %d after detaching both", st.Attached())
	}
}

func TestSessionStoreResumeUnknownSession(t *testing.T) {
	st := NewSessionStore(time.Minute)
	defer st.Close()

	p := &identity.Principal{UserID: "alice"}
	if _, ok := st.Resume("no-such-session", "token", p); ok {
		t.Error("resumed a session that does not exist")
	}
}

func TestSessionStoreResumeRefreshesPrincipal(t *testing.T) {
	st := NewSessionStore(time.Minute)
	defer st.Close()

	old := &identity.Principal{UserID: "alice", ExpiresAt: time.Now().Add(time.Minute)}
	sess, token := st.Create(old, "phone", "")
	st.Detach(sess)

	// A reconnect with a newer token must refresh the session's principal,
	// or the session would die on the old expiry.
	fresh := &identity.Principal{UserID: "alice", ExpiresAt: time.Now().Add(time.Hour)}
	got, ok := st.Resume(sess.ID, token, fresh)
	if !ok {
		t.Fatal("resume failed")
	}
	if !got.Principal.ExpiresAt.Equal(fresh.ExpiresAt) {
		t.Error("resume did not adopt the fresher principal")
	}
}

func TestSessionSubscriptionTracking(t *testing.T) {
	st := NewSessionStore(time.Minute)
	defer st.Close()

	sess, _ := st.Create(&identity.Principal{UserID: "alice"}, "phone", "")

	sess.addSubscription("chat.group.g1", 3, 100)
	sess.addSubscription("chat.inbox.alice", 0, 50)
	if got := sess.snapshotSubscriptions(); len(got) != 2 {
		t.Fatalf("got %d subscriptions", len(got))
	}

	// advance must move forward only.
	sess.advance("chat.group.g1", 3, 200)
	sess.advance("chat.group.g1", 3, 150)
	for _, s := range sess.snapshotSubscriptions() {
		if s.Topic == "chat.group.g1" && s.FromSeq != 200 {
			t.Errorf("subscription rewound to %d", s.FromSeq)
		}
	}

	// advance on an unknown subscription must not create one.
	sess.advance("chat.group.unknown", 0, 999)
	if len(sess.snapshotSubscriptions()) != 2 {
		t.Error("advance created a subscription")
	}

	sess.removeSubscription("chat.group.g1", 3)
	if got := sess.snapshotSubscriptions(); len(got) != 1 {
		t.Errorf("removeSubscription left %d", len(got))
	}
}

func TestSubKeyDisambiguatesPartitions(t *testing.T) {
	// A naive rune-based key would collide for large partition numbers.
	seen := map[string]bool{}
	for p := int32(0); p < 1000; p++ {
		k := subKey("chat", p)
		if seen[k] {
			t.Fatalf("subKey collision at partition %d", p)
		}
		seen[k] = true
	}
	if subKey("a.b", 1) == subKey("a", 0) {
		t.Error("subKey confuses topic and partition boundaries")
	}
}

func TestSessionWatchedUsers(t *testing.T) {
	st := NewSessionStore(time.Minute)
	defer st.Close()

	sess, _ := st.Create(&identity.Principal{UserID: "alice"}, "phone", "")
	sess.setWatchedUsers([]string{"bob", "carol"})
	if got := sess.watchedSnapshot(); len(got) != 2 {
		t.Errorf("watched = %v", got)
	}
	sess.setWatchedUsers(nil)
	if got := sess.watchedSnapshot(); len(got) != 0 {
		t.Errorf("setWatchedUsers(nil) left %v", got)
	}
}

func TestRandomTokenIsUnique(t *testing.T) {
	seen := map[string]bool{}
	for i := 0; i < 10000; i++ {
		tok := randomToken(16)
		if seen[tok] {
			t.Fatal("randomToken produced a duplicate")
		}
		seen[tok] = true
	}
}

// --- Connection lifecycle ---

func TestGatewayCloseRejectsNewConnections(t *testing.T) {
	h := newHarness(t)
	token := h.token(t, "alice", "phone")

	h.gw.Close()

	url := "ws" + strings.TrimPrefix(h.server.URL, "http") + "?token=" + token
	if _, resp, err := websocket.DefaultDialer.Dial(url, nil); err == nil {
		t.Error("a connection was accepted after Close")
	} else if resp != nil && resp.StatusCode != http.StatusServiceUnavailable {
		t.Errorf("expected 503, got %d", resp.StatusCode)
	}
	h.gw.Close() // idempotent
}

func TestConcurrentConnectionsAndClose(t *testing.T) {
	h := newHarness(t)

	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			url := "ws" + strings.TrimPrefix(h.server.URL, "http") +
				"?token=" + h.token(t, "alice", "d"+string(rune('a'+i)))
			conn, _, err := websocket.DefaultDialer.Dial(url, nil)
			if err != nil {
				return // acceptable once shutdown begins
			}
			defer conn.Close()
			conn.WriteJSON(Frame{Op: OpHello, ID: "h", Version: ProtocolVersion,
				DeviceID: "d" + string(rune('a'+i))})
			conn.SetReadDeadline(time.Now().Add(time.Second))
			var f Frame
			conn.ReadJSON(&f)
		}(i)
	}

	time.Sleep(100 * time.Millisecond)
	// Close racing accepts must not trip the WaitGroup misuse panic.
	h.gw.Close()
	wg.Wait()
}

func TestReadLimitDisconnectsOversizedFrame(t *testing.T) {
	h := newHarnessWith(t, Config{
		PongTimeout: 5 * time.Second, ResumeWindow: time.Minute, ReadLimit: 4096,
	})
	c := h.dial(t, h.token(t, "alice", "phone"))
	c.hello("phone")

	// A frame past the read limit must close the socket, not be processed.
	c.send(Frame{Op: OpSend, ID: "big", Kind: "direct", Conversation: "alice:bob",
		Payload: make([]byte, 64*1024)})

	if f, ok := c.tryRead(2 * time.Second); ok {
		t.Errorf("an oversized frame was processed: %+v", f)
	}
}

func TestHelloRequiresDeviceID(t *testing.T) {
	h := newHarness(t)

	// A token with no `did` claim, and no device_id in hello.
	tok, _ := identity.Sign(signKey, identity.Claims{
		Subject: "alice", ExpiresAt: time.Now().Add(time.Hour).Unix(),
	})
	c := h.dial(t, tok)
	c.send(Frame{Op: OpHello, ID: "h1", Version: ProtocolVersion})

	f := c.read(3 * time.Second)
	if f.Op != OpError || f.Error.Code != CodeBadRequest {
		t.Fatalf("hello with no device id: %+v", f)
	}
}

func TestHelloFallsBackToTokenDeviceID(t *testing.T) {
	h := newHarness(t)
	c := h.dial(t, h.token(t, "alice", "phone-from-token"))

	c.send(Frame{Op: OpHello, ID: "h1", Version: ProtocolVersion})
	c.readOp(OpWelcome, 3*time.Second)

	if _, ok := h.pres.Session("alice", "phone-from-token"); !ok {
		t.Error("the device ID from the token claim was not used")
	}
}

func TestVersionZeroIsAccepted(t *testing.T) {
	// An older client that omits the version must still connect.
	h := newHarness(t)
	c := h.dial(t, h.token(t, "alice", "phone"))
	c.send(Frame{Op: OpHello, ID: "h1", DeviceID: "phone"})
	c.readOp(OpWelcome, 3*time.Second)
}

func TestPingRefreshesPresenceHeartbeat(t *testing.T) {
	h := newHarnessWithPresenceTTL(t, 150*time.Millisecond)

	c := h.dial(t, h.token(t, "alice", "phone"))
	c.hello("phone")

	for i := 0; i < 6; i++ {
		time.Sleep(50 * time.Millisecond)
		c.send(Frame{Op: OpPing, ID: "k"})
		c.readOp(OpPong, 3*time.Second)
		h.pres.Sweep()
	}
	if !h.pres.Online("alice") {
		t.Error("a heartbeating connection was swept as offline")
	}
}
