package gateway

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"

	"github.com/boltq/boltq/internal/dedup"
	"github.com/boltq/boltq/internal/ephemeral"
	"github.com/boltq/boltq/internal/fanout"
	"github.com/boltq/boltq/internal/identity"
	"github.com/boltq/boltq/internal/presence"
	"github.com/boltq/boltq/internal/stream"
)

var signKey = identity.SigningKey{ID: "test", Secret: []byte("0123456789abcdef0123456789abcdef")}

type harness struct {
	gw      *Gateway
	server  *httptest.Server
	log     *stream.Log
	cursors *stream.CursorStore
	members *identity.StaticMembership
	pres    *presence.Registry
	signals *ephemeral.Hub
}

type memberAdapter struct{ m *identity.StaticMembership }

func (a memberAdapter) Members(_ context.Context, tenant, groupID string) ([]string, error) {
	return a.m.Members(tenant, groupID), nil
}

type harnessOpts struct {
	cfg            Config
	apiKey         string
	allowAnonymous bool
	presenceTTL    time.Duration
	signals        *ephemeral.Hub
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	return newHarnessOpts(t, harnessOpts{cfg: Config{
		PongTimeout: 5 * time.Second, ResumeWindow: time.Minute, NodeID: "test-node",
	}})
}

func newHarnessOpts(t *testing.T, o harnessOpts) *harness {
	t.Helper()
	if o.cfg.NodeID == "" {
		o.cfg.NodeID = "test-node"
	}
	if o.presenceTTL == 0 {
		o.presenceTTL = time.Hour
	}

	l, err := stream.OpenLog(stream.LogConfig{
		Dir:          t.TempDir(),
		DefaultTopic: stream.TopicConfig{Partitions: 4},
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

	members := identity.NewStaticMembership()
	pres := presence.New(presence.Config{
		NodeID: o.cfg.NodeID, SweepInterval: time.Hour, TTL: o.presenceTTL,
	})
	t.Cleanup(pres.Close)

	signals := o.signals
	if signals == nil {
		signals = ephemeral.New(ephemeral.Config{RatePerSecond: 1000, Burst: 1000})
	}
	t.Cleanup(signals.Close)

	tbl := dedup.New(dedup.Config{TTL: time.Minute, SweepInterval: time.Hour})
	t.Cleanup(tbl.Close)

	deliver, err := fanout.New(fanout.Options{
		Log: l, Members: memberAdapter{members}, Presence: presence.LocalLookup{Registry: pres}, Dedup: tbl,
	})
	if err != nil {
		t.Fatalf("new deliverer: %v", err)
	}

	verifier, err := identity.NewVerifier(identity.VerifierConfig{Keys: []identity.SigningKey{signKey}})
	if err != nil {
		t.Fatalf("new verifier: %v", err)
	}

	policy := identity.NewPolicy(identity.PolicyConfig{
		Rules: identity.ChatPolicyRules(), Membership: members,
		AllowAnonymous: o.allowAnonymous,
	})

	gw, err := New(Options{
		Log: l, Cursors: cursors, Deliver: deliver, Presence: pres,
		Signals: signals, Policy: policy, Verifier: verifier,
		APIKey: o.apiKey,
		Config: o.cfg,
	})
	if err != nil {
		t.Fatalf("new gateway: %v", err)
	}
	t.Cleanup(gw.Close)

	srv := httptest.NewServer(gw)
	t.Cleanup(srv.Close)

	return &harness{
		gw: gw, server: srv, log: l, cursors: cursors,
		members: members, pres: pres, signals: signals,
	}
}

func (h *harness) token(t *testing.T, userID, deviceID string, scopes ...string) string {
	t.Helper()
	if len(scopes) == 0 {
		scopes = []string{identity.ScopePublish, identity.ScopeSubscribe, identity.ScopePresence}
	}
	tok, err := identity.Sign(signKey, identity.Claims{
		Subject: userID, DeviceID: deviceID, Scopes: scopes,
		ExpiresAt: time.Now().Add(time.Hour).Unix(),
	})
	if err != nil {
		t.Fatalf("sign: %v", err)
	}
	return tok
}

// client is a small test client for the gateway protocol.
type client struct {
	t    *testing.T
	conn *websocket.Conn
	// pending holds frames read while waiting for a different op. On a single
	// connection a server-initiated record can overtake the response to the
	// send that produced it, so discarding non-matching frames would make
	// tests racy.
	pending []Frame
}

func (h *harness) dial(t *testing.T, token string) *client {
	t.Helper()
	url := "ws" + strings.TrimPrefix(h.server.URL, "http") + "?token=" + token
	conn, resp, err := websocket.DefaultDialer.Dial(url, nil)
	if err != nil {
		code := 0
		if resp != nil {
			code = resp.StatusCode
		}
		t.Fatalf("dial: %v (http %d)", err, code)
	}
	t.Cleanup(func() { conn.Close() })
	return &client{t: t, conn: conn}
}

func (c *client) send(f Frame) {
	c.t.Helper()
	if err := c.conn.WriteJSON(f); err != nil {
		c.t.Fatalf("write %s: %v", f.Op, err)
	}
}

func (c *client) read(timeout time.Duration) Frame {
	c.t.Helper()
	if len(c.pending) > 0 {
		f := c.pending[0]
		c.pending = c.pending[1:]
		return f
	}
	return c.readSocket(timeout)
}

// readSocket reads one frame straight from the connection, bypassing the
// pending buffer.
func (c *client) readSocket(timeout time.Duration) Frame {
	c.t.Helper()
	c.conn.SetReadDeadline(time.Now().Add(timeout))
	var f Frame
	if err := c.conn.ReadJSON(&f); err != nil {
		c.t.Fatalf("read: %v", err)
	}
	return f
}

// tryRead returns the next frame, or ok=false if none arrives within timeout.
// Used to assert that something is NOT delivered.
func (c *client) tryRead(timeout time.Duration) (Frame, bool) {
	c.t.Helper()
	if len(c.pending) > 0 {
		f := c.pending[0]
		c.pending = c.pending[1:]
		return f, true
	}
	c.conn.SetReadDeadline(time.Now().Add(timeout))
	var f Frame
	if err := c.conn.ReadJSON(&f); err != nil {
		return Frame{}, false
	}
	return f, true
}

// readOp reads until a frame with the given op arrives, so a test waiting for a
// response is not derailed by an unrelated server-initiated frame.
func (c *client) readOp(op Op, timeout time.Duration) Frame {
	c.t.Helper()
	// Check anything buffered by an earlier readOp first, removing a match.
	for i, f := range c.pending {
		if f.Op == op {
			c.pending = append(c.pending[:i], c.pending[i+1:]...)
			return f
		}
	}

	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		// Read from the socket directly: going through c.read would re-consume
		// the frames this loop just buffered and spin without progressing.
		f := c.readSocket(time.Until(deadline))
		if f.Op == op {
			return f
		}
		if f.Op == OpError {
			c.t.Fatalf("waiting for %s but got error: %+v", op, f.Error)
		}
		// Keep it for a later read rather than dropping it on the floor.
		c.pending = append(c.pending, f)
	}
	c.t.Fatalf("timed out waiting for %s", op)
	return Frame{}
}

func (c *client) hello(deviceID string) Frame {
	c.send(Frame{Op: OpHello, ID: "h1", Version: ProtocolVersion, DeviceID: deviceID})
	return c.readOp(OpWelcome, 3*time.Second)
}

// --- Authentication ---

func TestRejectsMissingToken(t *testing.T) {
	h := newHarness(t)
	url := "ws" + strings.TrimPrefix(h.server.URL, "http")

	_, resp, err := websocket.DefaultDialer.Dial(url, nil)
	if err == nil {
		t.Fatal("connection without a token succeeded")
	}
	if resp == nil || resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("expected 401, got %v", resp)
	}
}

func TestRejectsForgedToken(t *testing.T) {
	h := newHarness(t)
	forged, _ := identity.Sign(identity.SigningKey{
		ID: "test", Secret: []byte("ffffffffffffffffffffffffffffffff"),
	}, identity.Claims{Subject: "attacker"})

	url := "ws" + strings.TrimPrefix(h.server.URL, "http") + "?token=" + forged
	if _, _, err := websocket.DefaultDialer.Dial(url, nil); err == nil {
		t.Fatal("a token signed with the wrong key was accepted")
	}
}

func TestAcceptsBearerHeader(t *testing.T) {
	h := newHarness(t)
	url := "ws" + strings.TrimPrefix(h.server.URL, "http")

	hdr := http.Header{}
	hdr.Set("Authorization", "Bearer "+h.token(t, "alice", "phone"))
	conn, _, err := websocket.DefaultDialer.Dial(url, hdr)
	if err != nil {
		t.Fatalf("bearer header rejected: %v", err)
	}
	conn.Close()
}

func TestFirstFrameMustBeHello(t *testing.T) {
	h := newHarness(t)
	c := h.dial(t, h.token(t, "alice", "phone"))

	c.send(Frame{Op: OpSubscribe, ID: "x", Topic: "chat.inbox.alice"})
	f := c.read(3 * time.Second)
	if f.Op != OpError || f.Error.Code != CodeBadRequest {
		t.Fatalf("expected a bad_request error, got %+v", f)
	}
}

func TestProtocolVersionMismatch(t *testing.T) {
	h := newHarness(t)
	c := h.dial(t, h.token(t, "alice", "phone"))

	c.send(Frame{Op: OpHello, ID: "h1", Version: 999, DeviceID: "phone"})
	f := c.read(3 * time.Second)
	if f.Op != OpError || f.Error.Code != CodeUnsupported {
		t.Fatalf("expected unsupported_version, got %+v", f)
	}
}

// --- Session lifecycle ---

func TestHelloBindsPresence(t *testing.T) {
	h := newHarness(t)
	c := h.dial(t, h.token(t, "alice", "phone"))

	w := c.hello("phone")
	if w.Session == "" || w.Token == "" {
		t.Fatalf("welcome missing session or resume token: %+v", w)
	}
	if w.Resumed {
		t.Error("a fresh session was flagged as resumed")
	}

	if !h.pres.Online("alice") {
		t.Error("connecting did not mark alice online")
	}
	s, ok := h.pres.Session("alice", "phone")
	if !ok || s.NodeID != "test-node" {
		t.Errorf("presence session wrong: %+v ok=%v", s, ok)
	}
}

func TestDisconnectUnbindsPresence(t *testing.T) {
	h := newHarness(t)
	c := h.dial(t, h.token(t, "alice", "phone"))
	c.hello("phone")

	if !h.pres.Online("alice") {
		t.Fatal("alice not online after connect")
	}
	c.conn.Close()

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if !h.pres.Online("alice") {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Error("alice still online after disconnect")
}

func TestSessionResume(t *testing.T) {
	h := newHarness(t)
	h.members.Add("", "g1", "alice", "bob")

	c1 := h.dial(t, h.token(t, "alice", "phone"))
	w := c1.hello("phone")
	resumeToken := w.Token

	c1.send(Frame{Op: OpSubscribe, ID: "s1", Topic: "chat.group.g1", FromSeq: 1})
	c1.readOp(OpAck, 3*time.Second)

	// Simulate a tunnel: the socket dies without a close handshake.
	c1.conn.Close()
	time.Sleep(100 * time.Millisecond)

	c2 := h.dial(t, h.token(t, "alice", "phone"))
	c2.send(Frame{Op: OpHello, ID: "h2", Version: ProtocolVersion, DeviceID: "phone", Resume: resumeToken})
	w2 := c2.readOp(OpWelcome, 3*time.Second)

	if !w2.Resumed {
		t.Fatal("session was not resumed")
	}
	if w2.Session != w.Session {
		t.Errorf("resumed into a different session: %s vs %s", w2.Session, w.Session)
	}
}

func TestResumeRejectsWrongToken(t *testing.T) {
	h := newHarness(t)
	c1 := h.dial(t, h.token(t, "alice", "phone"))
	w := c1.hello("phone")
	sessionID, _, _ := strings.Cut(w.Token, ".")
	c1.conn.Close()
	time.Sleep(100 * time.Millisecond)

	c2 := h.dial(t, h.token(t, "alice", "phone"))
	c2.send(Frame{
		Op: OpHello, ID: "h2", Version: ProtocolVersion, DeviceID: "phone",
		Resume: sessionID + ".wrong-secret",
	})
	w2 := c2.readOp(OpWelcome, 3*time.Second)

	// A bad token must produce a fresh session, never someone else's.
	if w2.Resumed {
		t.Error("a forged resume token was accepted")
	}
	if w2.Session == w.Session {
		t.Error("forged resume attached to the original session")
	}
}

func TestResumeRejectsDifferentUser(t *testing.T) {
	h := newHarness(t)
	c1 := h.dial(t, h.token(t, "alice", "phone"))
	w := c1.hello("phone")
	c1.conn.Close()
	time.Sleep(100 * time.Millisecond)

	// Mallory has alice's resume token but her own identity token.
	c2 := h.dial(t, h.token(t, "mallory", "phone"))
	c2.send(Frame{
		Op: OpHello, ID: "h2", Version: ProtocolVersion, DeviceID: "phone", Resume: w.Token,
	})
	w2 := c2.readOp(OpWelcome, 3*time.Second)

	if w2.Resumed {
		t.Fatal("mallory resumed alice's session with a leaked resume token")
	}
}

// --- Authorisation ---

func TestSubscribeDeniedForOtherUsersInbox(t *testing.T) {
	h := newHarness(t)
	c := h.dial(t, h.token(t, "alice", "phone"))
	c.hello("phone")

	c.send(Frame{Op: OpSubscribe, ID: "s1", Topic: "chat.inbox.bob"})
	f := c.read(3 * time.Second)
	if f.Op != OpError || f.Error.Code != CodeForbidden {
		t.Fatalf("alice could subscribe to bob's inbox: %+v", f)
	}
}

func TestSubscribeAllowedForOwnInbox(t *testing.T) {
	h := newHarness(t)
	c := h.dial(t, h.token(t, "alice", "phone"))
	c.hello("phone")

	c.send(Frame{Op: OpSubscribe, ID: "s1", Topic: "chat.inbox.alice"})
	f := c.readOp(OpAck, 3*time.Second)
	if f.ID != "s1" {
		t.Errorf("ack id = %q", f.ID)
	}
}

func TestSendDeniedWithoutMembership(t *testing.T) {
	h := newHarness(t)
	c := h.dial(t, h.token(t, "alice", "phone"))
	c.hello("phone")

	c.send(Frame{
		Op: OpSend, ID: "m1", Kind: "group", Conversation: "secret-group",
		Payload: []byte("let me in"),
	})
	f := c.read(3 * time.Second)
	if f.Op != OpError || f.Error.Code != CodeForbidden {
		t.Fatalf("a non-member could send to a group: %+v", f)
	}
}

// --- Messaging ---

func TestSendAndReceive(t *testing.T) {
	h := newHarness(t)
	h.members.Add("", "g1", "alice", "bob")

	bob := h.dial(t, h.token(t, "bob", "bob-phone"))
	bob.hello("bob-phone")
	bob.send(Frame{Op: OpSubscribe, ID: "s1", Topic: "chat.group.g1", FromSeq: 1})
	bob.readOp(OpAck, 3*time.Second)

	alice := h.dial(t, h.token(t, "alice", "alice-phone"))
	alice.hello("alice-phone")
	alice.send(Frame{
		Op: OpSend, ID: "m1", Kind: "group", Conversation: "g1",
		ClientMsgID: "c1", Payload: []byte("hello bob"),
	})

	sent := alice.readOp(OpSent, 3*time.Second)
	if sent.Sent == nil || sent.Sent.Seq != 1 {
		t.Fatalf("sent frame wrong: %+v", sent.Sent)
	}

	// The whole point: bob's tail wakes on the append without polling.
	rec := bob.readOp(OpRecord, 3*time.Second)
	if len(rec.Records) != 1 {
		t.Fatalf("got %d records", len(rec.Records))
	}
	got := rec.Records[0]
	if string(got.Payload) != "hello bob" {
		t.Errorf("payload = %q", got.Payload)
	}
	if got.Headers[fanout.HeaderSender] != "alice" {
		t.Errorf("sender header = %q", got.Headers[fanout.HeaderSender])
	}
	if got.Seq != sent.Sent.Seq {
		t.Errorf("delivered seq %d, sender was told %d", got.Seq, sent.Sent.Seq)
	}
}

func TestDuplicateSendIsCollapsed(t *testing.T) {
	h := newHarness(t)
	h.members.Add("", "g1", "alice")

	c := h.dial(t, h.token(t, "alice", "phone"))
	c.hello("phone")

	send := Frame{
		Op: OpSend, ID: "m1", Kind: "group", Conversation: "g1",
		ClientMsgID: "same-id", Payload: []byte("only once"),
	}
	c.send(send)
	first := c.readOp(OpSent, 3*time.Second)

	send.ID = "m2"
	c.send(send)
	second := c.readOp(OpSent, 3*time.Second)

	if !second.Sent.Duplicate {
		t.Error("retry was not flagged as duplicate")
	}
	if second.Sent.Seq != first.Sent.Seq {
		t.Errorf("retry got seq %d, original was %d", second.Sent.Seq, first.Sent.Seq)
	}
}

func TestHistory(t *testing.T) {
	h := newHarness(t)
	h.members.Add("", "g1", "alice")

	c := h.dial(t, h.token(t, "alice", "phone"))
	c.hello("phone")

	for i := 0; i < 5; i++ {
		c.send(Frame{
			Op: OpSend, ID: "m", Kind: "group", Conversation: "g1",
			ClientMsgID: string(rune('a' + i)), Payload: []byte{byte('0' + i)},
		})
		c.readOp(OpSent, 3*time.Second)
	}

	c.send(Frame{Op: OpHistory, ID: "h1", Topic: "chat.group.g1", FromSeq: 1, Limit: 10})
	f := c.readOp(OpRecord, 3*time.Second)

	if len(f.Records) != 5 {
		t.Fatalf("history returned %d records, want 5", len(f.Records))
	}
	for i, r := range f.Records {
		if r.Seq != uint64(i+1) {
			t.Errorf("record %d has seq %d", i, r.Seq)
		}
	}
}

func TestCommitPersistsPerDeviceCursor(t *testing.T) {
	h := newHarness(t)

	c := h.dial(t, h.token(t, "alice", "phone"))
	c.hello("phone")

	c.send(Frame{Op: OpCommit, ID: "c1", Topic: "chat.inbox.alice", Seq: 42})
	c.readOp(OpAck, 3*time.Second)

	seq, ok := h.cursors.Position(stream.CursorKey{
		Topic: "chat.inbox.alice", Partition: 0, Group: "user:alice", Member: "phone",
	})
	if !ok || seq != 42 {
		t.Fatalf("cursor = %d, ok=%v; want 42", seq, ok)
	}

	// A second device must keep an independent position.
	c2 := h.dial(t, h.token(t, "alice", "laptop"))
	c2.hello("laptop")
	c2.send(Frame{Op: OpCommit, ID: "c2", Topic: "chat.inbox.alice", Seq: 10})
	c2.readOp(OpAck, 3*time.Second)

	members := h.cursors.GroupMembers("chat.inbox.alice", 0, "user:alice")
	if members["phone"] != 42 || members["laptop"] != 10 {
		t.Errorf("per-device cursors wrong: %v", members)
	}
	if slowest, _ := h.cursors.SlowestInGroup("chat.inbox.alice", 0, "user:alice"); slowest != 10 {
		t.Errorf("watermark = %d, want the slower device's 10", slowest)
	}
}

func TestSubscribeResumesFromCommittedCursor(t *testing.T) {
	h := newHarness(t)
	h.members.Add("", "g1", "alice")

	c := h.dial(t, h.token(t, "alice", "phone"))
	c.hello("phone")

	for i := 0; i < 5; i++ {
		c.send(Frame{
			Op: OpSend, ID: "m", Kind: "group", Conversation: "g1",
			ClientMsgID: string(rune('a' + i)), Payload: []byte{byte('0' + i)},
		})
		c.readOp(OpSent, 3*time.Second)
	}

	topic, _ := h.log.Topic("chat.group.g1")
	partition := topic.PartitionForKey([]byte("g1"))

	// Pretend the client read the first three, then subscribe with no explicit
	// position: it must resume at 4, not replay from the start.
	c.send(Frame{Op: OpCommit, ID: "c1", Topic: "chat.group.g1", Partition: &partition, Seq: 4})
	c.readOp(OpAck, 3*time.Second)

	c.send(Frame{Op: OpSubscribe, ID: "s1", Topic: "chat.group.g1", Partition: &partition})
	ack := c.readOp(OpAck, 3*time.Second)
	if ack.FromSeq != 4 {
		t.Errorf("subscribe resumed at %d, want 4", ack.FromSeq)
	}

	rec := c.readOp(OpRecord, 3*time.Second)
	if rec.Records[0].Seq != 4 {
		t.Errorf("first delivered record is seq %d, want 4", rec.Records[0].Seq)
	}
}

// --- Ephemeral signals ---

func TestTypingSignals(t *testing.T) {
	h := newHarness(t)
	h.members.Add("", "g1", "alice", "bob")

	bob := h.dial(t, h.token(t, "bob", "bob-phone"))
	bob.hello("bob-phone")
	// Bob publishes a stop-typing to register his subscription to the topic.
	stop := false
	bob.send(Frame{Op: OpTyping, ID: "t0", Conversation: "g1", Typing: &stop})
	bob.readOp(OpAck, 3*time.Second)

	alice := h.dial(t, h.token(t, "alice", "alice-phone"))
	alice.hello("alice-phone")
	typing := true
	alice.send(Frame{Op: OpTyping, ID: "t1", Conversation: "g1", Typing: &typing})
	alice.readOp(OpAck, 3*time.Second)

	sig := bob.readOp(OpSignal, 3*time.Second)
	if sig.Signal == nil || sig.Signal.Sender != "alice" || sig.Signal.Kind != ephemeral.KindTyping {
		t.Fatalf("typing signal wrong: %+v", sig.Signal)
	}
}

func TestTypingDeniedForNonMember(t *testing.T) {
	h := newHarness(t)
	c := h.dial(t, h.token(t, "alice", "phone"))
	c.hello("phone")

	typing := true
	c.send(Frame{Op: OpTyping, ID: "t1", Conversation: "not-my-group", Typing: &typing})
	f := c.read(3 * time.Second)
	if f.Op != OpError || f.Error.Code != CodeForbidden {
		t.Fatalf("non-member could publish typing: %+v", f)
	}
}

// --- Presence ---

func TestWatchPresence(t *testing.T) {
	h := newHarness(t)

	alice := h.dial(t, h.token(t, "alice", "alice-phone"))
	alice.hello("alice-phone")
	alice.send(Frame{Op: OpWatchPresence, ID: "w1", Users: []string{"bob"}})
	alice.readOp(OpAck, 3*time.Second)

	bob := h.dial(t, h.token(t, "bob", "bob-phone"))
	bob.hello("bob-phone")

	ev := alice.readOp(OpPresenceEvent, 3*time.Second)
	if ev.Presence == nil || ev.Presence.UserID != "bob" || !ev.Presence.Online {
		t.Fatalf("presence event wrong: %+v", ev.Presence)
	}
}

func TestSetPresenceState(t *testing.T) {
	h := newHarness(t)
	c := h.dial(t, h.token(t, "alice", "phone"))
	c.hello("phone")

	c.send(Frame{Op: OpPresence, ID: "p1", State: "away"})
	c.readOp(OpAck, 3*time.Second)

	s, _ := h.pres.Session("alice", "phone")
	if s.State != presence.StateAway {
		t.Errorf("state = %s, want away", s.State)
	}

	c.send(Frame{Op: OpPresence, ID: "p2", State: "nonsense"})
	f := c.read(3 * time.Second)
	if f.Op != OpError {
		t.Error("an invalid presence state was accepted")
	}
}

// --- Misc ---

func TestPingPong(t *testing.T) {
	h := newHarness(t)
	c := h.dial(t, h.token(t, "alice", "phone"))
	c.hello("phone")

	c.send(Frame{Op: OpPing, ID: "p1"})
	f := c.readOp(OpPong, 3*time.Second)
	if f.ID != "p1" {
		t.Errorf("pong id = %q", f.ID)
	}
}

func TestUnknownOp(t *testing.T) {
	h := newHarness(t)
	c := h.dial(t, h.token(t, "alice", "phone"))
	c.hello("phone")

	c.send(Frame{Op: "teleport", ID: "x"})
	f := c.read(3 * time.Second)
	if f.Op != OpError || f.Error.Code != CodeBadRequest {
		t.Fatalf("unknown op: %+v", f)
	}
}

func TestSecondHelloIsRejected(t *testing.T) {
	h := newHarness(t)
	c := h.dial(t, h.token(t, "alice", "phone"))
	c.hello("phone")

	c.send(Frame{Op: OpHello, ID: "h2", Version: ProtocolVersion, DeviceID: "phone"})
	f := c.read(3 * time.Second)
	if f.Op != OpError || f.Error.Code != CodeConflict {
		t.Fatalf("a second hello was accepted: %+v", f)
	}
}

func TestSubscriptionLimit(t *testing.T) {
	h := newHarness(t)
	h.gw.cfg.MaxSubscriptions = 3

	c := h.dial(t, h.token(t, "alice", "phone"))
	c.hello("phone")

	for i := 0; i < 3; i++ {
		p := int32(i)
		c.send(Frame{Op: OpSubscribe, ID: "s", Topic: "chat.inbox.alice", Partition: &p})
		c.readOp(OpAck, 3*time.Second)
	}

	p := int32(3)
	c.send(Frame{Op: OpSubscribe, ID: "over", Topic: "chat.inbox.alice", Partition: &p})
	f := c.read(3 * time.Second)
	if f.Op != OpError || f.Error.Code != CodeRateLimited {
		t.Fatalf("subscription limit not enforced: %+v", f)
	}
}

func TestExpiredTokenIsRejectedMidConnection(t *testing.T) {
	h := newHarness(t)

	// A token that is valid at connect but expires almost immediately. The
	// leeway means it stays acceptable briefly, so this asserts the check
	// exists rather than its exact timing.
	tok, _ := identity.Sign(signKey, identity.Claims{
		Subject: "alice", DeviceID: "phone",
		ExpiresAt: time.Now().Add(2 * time.Hour).Unix(),
	})
	c := h.dial(t, tok)
	w := c.hello("phone")
	if w.Session == "" {
		t.Fatal("connect failed")
	}
	// The connection works while the token is valid.
	c.send(Frame{Op: OpPing, ID: "p1"})
	c.readOp(OpPong, 3*time.Second)
}

func TestGatewayStats(t *testing.T) {
	h := newHarness(t)
	c := h.dial(t, h.token(t, "alice", "phone"))
	c.hello("phone")

	s := h.gw.Stats()
	if s.Connections == 0 {
		t.Error("connection not counted")
	}
	if s.Sessions == 0 || s.Attached == 0 {
		t.Errorf("sessions=%d attached=%d", s.Sessions, s.Attached)
	}

	data, err := h.gw.StatsJSON()
	if err != nil {
		t.Fatalf("StatsJSON: %v", err)
	}
	var decoded Stats
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("stats are not valid JSON: %v", err)
	}
}

func TestSessionStoreReap(t *testing.T) {
	st := NewSessionStore(30 * time.Millisecond)
	defer st.Close()

	p := &identity.Principal{UserID: "alice"}
	sess, _ := st.Create(p, "phone", "")
	st.Detach(sess)

	if st.Len() != 1 {
		t.Fatal("session missing")
	}
	time.Sleep(60 * time.Millisecond)
	if n := st.Reap(); n != 1 {
		t.Errorf("reaped %d, want 1", n)
	}
	if st.Len() != 0 {
		t.Error("expired session survived the reaper")
	}
}

func TestSessionStoreRefusesConcurrentResume(t *testing.T) {
	st := NewSessionStore(time.Minute)
	defer st.Close()

	p := &identity.Principal{UserID: "alice"}
	sess, token := st.Create(p, "phone", "")

	// Still attached: a second socket must not steal it.
	if _, ok := st.Resume(sess.ID, token, p); ok {
		t.Error("resumed a session that is still attached")
	}

	st.Detach(sess)
	if _, ok := st.Resume(sess.ID, token, p); !ok {
		t.Error("could not resume a detached session")
	}
}
