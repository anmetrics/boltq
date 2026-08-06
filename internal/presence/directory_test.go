package presence

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeResolver places users on nodes the way the control plane would.
type fakeResolver struct {
	self  string
	owner map[string]string // userID -> nodeID
	addr  map[string]string // nodeID -> admin address
}

func (f *fakeResolver) OwnerOf(userID string) (string, string, bool) {
	node, ok := f.owner[userID]
	if !ok {
		return "", "", false // unowned shard
	}
	if node == f.self {
		return node, "", true
	}
	return node, f.addr[node], false
}

// peerNode is another node's presence shard, reachable over HTTP.
type peerNode struct {
	*httptest.Server
	reg *Registry
}

func newPeerNode(t *testing.T, nodeID string) *peerNode {
	t.Helper()
	reg := New(Config{NodeID: nodeID, TTL: time.Minute})
	t.Cleanup(reg.Close)

	p := &peerNode{reg: reg}
	mux := http.NewServeMux()
	mux.HandleFunc("/internal/presence", func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet:
			json.NewEncoder(w).Encode(map[string]any{
				"sessions": reg.Sessions(r.URL.Query().Get("user")),
			})
		case http.MethodPost:
			var s Session
			json.NewDecoder(r.Body).Decode(&s)
			reg.Bind(s)
			w.WriteHeader(http.StatusOK)
		}
	})
	p.Server = httptest.NewServer(mux)
	t.Cleanup(p.Close)
	return p
}

func (p *peerNode) addr() string { return strings.TrimPrefix(p.URL, "http://") }

// TestPresenceIsVisibleAcrossNodes is the gap this closes. Before sharding,
// a node knew only the sessions it held, so fan-out and push decisions on every
// other node were made on a view that was missing most of the cluster.
func TestPresenceIsVisibleAcrossNodes(t *testing.T) {
	peer := newPeerNode(t, "n2")

	local := New(Config{NodeID: "n1", TTL: time.Minute})
	defer local.Close()

	d := NewDirectory(DirectoryOptions{
		Registry: local,
		Resolver: &fakeResolver{
			self:  "n1",
			owner: map[string]string{"alice": "n2"},
			addr:  map[string]string{"n2": peer.addr()},
		},
	})

	// Alice's socket is held by n1, but her shard belongs to n2.
	err := d.Report(context.Background(), Session{
		UserID: "alice", DeviceID: "phone", ConnID: "c1", NodeID: "n1",
	})
	if err != nil {
		t.Fatalf("report: %v", err)
	}

	// n1 must now be able to see her, via n2.
	if !d.Online(context.Background(), "alice") {
		t.Error("alice reported but not visible through her shard owner")
	}

	sessions, err := d.Sessions(context.Background(), "alice")
	if err != nil {
		t.Fatalf("sessions: %v", err)
	}
	if len(sessions) != 1 {
		t.Fatalf("got %d sessions, want 1", len(sessions))
	}
	// The delivery route must point at the node holding the socket, not the
	// node holding the shard. Overwriting NodeID on the owner would send every
	// message to a node with no connection to deliver on.
	if sessions[0].NodeID != "n1" {
		t.Errorf("session NodeID = %q, want n1 which holds the socket", sessions[0].NodeID)
	}
}

// TestLocalShardSkipsTheNetwork: routing must be the exception. A lookup for a
// user this node owns has no business making an HTTP call.
func TestLocalShardSkipsTheNetwork(t *testing.T) {
	peer := newPeerNode(t, "n2")
	peer.Close() // any network call now fails outright

	local := New(Config{NodeID: "n1", TTL: time.Minute})
	defer local.Close()

	d := NewDirectory(DirectoryOptions{
		Registry: local,
		Resolver: &fakeResolver{
			self:  "n1",
			owner: map[string]string{"bob": "n1"},
			addr:  map[string]string{"n2": peer.addr()},
		},
	})

	if err := d.Report(context.Background(), Session{
		UserID: "bob", DeviceID: "laptop", ConnID: "c1", NodeID: "n1",
	}); err != nil {
		t.Fatalf("local report failed: %v", err)
	}
	if !d.Online(context.Background(), "bob") {
		t.Error("locally-owned user not visible")
	}
	if d.Stats().RemoteHits != 0 {
		t.Errorf("RemoteHits = %d, want 0 for a locally-owned shard", d.Stats().RemoteHits)
	}
}

// TestOnlineAssumesReachableWhenTheShardCannotAnswer.
//
// This call decides whether to send a push notification, and the two wrong
// answers are not equal. A false "offline" wakes someone for a message they are
// already reading; a false "online" silently drops a notification they needed.
func TestOnlineAssumesReachableWhenTheShardCannotAnswer(t *testing.T) {
	peer := newPeerNode(t, "n2")
	peer.Close() // the owner is unreachable

	local := New(Config{NodeID: "n1", TTL: time.Minute})
	defer local.Close()

	d := NewDirectory(DirectoryOptions{
		Registry: local,
		Resolver: &fakeResolver{
			self:  "n1",
			owner: map[string]string{"carol": "n2"},
			addr:  map[string]string{"n2": peer.addr()},
		},
		Timeout: 200 * time.Millisecond,
	})

	if !d.Online(context.Background(), "carol") {
		t.Error("an unreachable shard reported the user offline; that drops their notification")
	}
	if d.Stats().Failures == 0 {
		t.Error("failure was not counted, so the condition would be invisible")
	}

	// Sessions must NOT invent an answer: the caller is routing a delivery, and
	// a made-up empty list sends the message nowhere while looking successful.
	if _, err := d.Sessions(context.Background(), "carol"); err == nil {
		t.Error("Sessions returned success for an unreachable shard")
	}
}

// TestUnownedShardIsDistinctFromEmpty: a shard whose owner was fenced and not
// yet reassigned is "unknown", not "nobody is online". Conflating them makes a
// failover look like every user on that shard went offline at once.
func TestUnownedShardIsDistinctFromEmpty(t *testing.T) {
	local := New(Config{NodeID: "n1", TTL: time.Minute})
	defer local.Close()

	d := NewDirectory(DirectoryOptions{
		Registry: local,
		Resolver: &fakeResolver{self: "n1", owner: map[string]string{}},
	})

	if !d.Online(context.Background(), "dave") {
		t.Error("unowned shard reported the user offline")
	}
	if _, err := d.Sessions(context.Background(), "dave"); err == nil {
		t.Error("Sessions on an unowned shard returned success")
	}
	if d.Stats().Unowned == 0 {
		t.Error("unowned shards are not counted")
	}
}

// TestShardForUserIsStable: the shard function is part of the cluster's wire
// contract. Changing it moves every user's presence at once, which during a
// rolling upgrade means two halves of the cluster disagreeing about where every
// user lives.
func TestShardForUserIsStable(t *testing.T) {
	const shards = 16
	first := map[string]int32{}
	for _, u := range []string{"alice", "bob", "carol", "user-12345", ""} {
		first[u] = ShardForUser(u, shards)
	}
	for i := 0; i < 100; i++ {
		for u, want := range first {
			if got := ShardForUser(u, shards); got != want {
				t.Fatalf("%q hashed to %d then %d", u, want, got)
			}
		}
	}

	// And it must actually spread.
	seen := map[int32]bool{}
	for i := 0; i < 500; i++ {
		seen[ShardForUser(string(rune('a'+i%26))+string(rune('0'+i/26)), shards)] = true
	}
	if len(seen) < shards/2 {
		t.Errorf("500 users landed on only %d of %d shards", len(seen), shards)
	}
}

// TestSingleNodeNeedsNoResolver: without a control plane every user is local,
// and requiring a resolver would break the standalone deployment.
func TestSingleNodeNeedsNoResolver(t *testing.T) {
	local := New(Config{NodeID: "solo", TTL: time.Minute})
	defer local.Close()

	d := NewDirectory(DirectoryOptions{Registry: local})

	if err := d.Report(context.Background(), Session{
		UserID: "erin", DeviceID: "phone", ConnID: "c1",
	}); err != nil {
		t.Fatalf("report: %v", err)
	}
	if !d.Online(context.Background(), "erin") {
		t.Error("single-node presence lookup failed")
	}
}

// TestOfflineUsersBatchesByOwner is the fan-out hot path. Asking per recipient
// would make a hundred-member group cost a hundred sequential round trips; the
// batch makes it cost one call per owning node, in parallel.
func TestOfflineUsersBatchesByOwner(t *testing.T) {
	var calls int
	var batchSizes []int
	var mu sync.Mutex

	peer := New(Config{NodeID: "n2", TTL: time.Minute})
	defer peer.Close()
	peer.Bind(Session{UserID: "u1", DeviceID: "d", ConnID: "c", NodeID: "n2"})

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Users []string `json:"users"`
		}
		json.NewDecoder(r.Body).Decode(&req)

		mu.Lock()
		calls++
		batchSizes = append(batchSizes, len(req.Users))
		mu.Unlock()

		offline := make([]string, 0, len(req.Users))
		for _, u := range req.Users {
			if !peer.Online(u) {
				offline = append(offline, u)
			}
		}
		json.NewEncoder(w).Encode(map[string][]string{"offline": offline})
	}))
	defer srv.Close()

	local := New(Config{NodeID: "n1", TTL: time.Minute})
	defer local.Close()
	local.Bind(Session{UserID: "u5", DeviceID: "d", ConnID: "c", NodeID: "n1"})

	// u1..u4 belong to n2; u5 and u6 are local.
	d := NewDirectory(DirectoryOptions{
		Registry: local,
		Resolver: &fakeResolver{
			self: "n1",
			owner: map[string]string{
				"u1": "n2", "u2": "n2", "u3": "n2", "u4": "n2",
				"u5": "n1", "u6": "n1",
			},
			addr: map[string]string{"n2": strings.TrimPrefix(srv.URL, "http://")},
		},
	})

	offline := d.OfflineUsers(context.Background(),
		[]string{"u1", "u2", "u3", "u4", "u5", "u6"})

	mu.Lock()
	gotCalls, gotSizes := calls, batchSizes
	mu.Unlock()

	if gotCalls != 1 {
		t.Errorf("made %d calls for one owning node, want 1 (sizes %v)", gotCalls, gotSizes)
	}
	if len(gotSizes) != 1 || gotSizes[0] != 4 {
		t.Errorf("batch sizes = %v, want one batch of 4", gotSizes)
	}

	want := map[string]bool{"u2": true, "u3": true, "u4": true, "u6": true}
	if len(offline) != len(want) {
		t.Fatalf("offline = %v, want %v", offline, want)
	}
	for _, u := range offline {
		if !want[u] {
			t.Errorf("%s reported offline but has a session", u)
		}
	}
}

// TestOfflineUsersAssumesReachableOnFailure: a shard owner that cannot be
// reached must not turn its users into notification targets.
func TestOfflineUsersAssumesReachableOnFailure(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	srv.Close() // unreachable

	local := New(Config{NodeID: "n1", TTL: time.Minute})
	defer local.Close()

	d := NewDirectory(DirectoryOptions{
		Registry: local,
		Resolver: &fakeResolver{
			self:  "n1",
			owner: map[string]string{"u1": "n2", "u2": "n2"},
			addr:  map[string]string{"n2": strings.TrimPrefix(srv.URL, "http://")},
		},
		Timeout: 200 * time.Millisecond,
	})

	if offline := d.OfflineUsers(context.Background(), []string{"u1", "u2"}); len(offline) != 0 {
		t.Errorf("offline = %v; an unreachable owner must not mark its users offline", offline)
	}
	if d.Stats().Failures == 0 {
		t.Error("failure not counted")
	}
}
