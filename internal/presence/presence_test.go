package presence

import (
	"fmt"
	"sync"
	"testing"
	"time"
)

func newTestRegistry(t *testing.T, cfg Config) *Registry {
	t.Helper()
	if cfg.NodeID == "" {
		cfg.NodeID = "node-a"
	}
	// A long sweep interval keeps the background sweeper out of tests that
	// drive expiry explicitly.
	if cfg.SweepInterval == 0 {
		cfg.SweepInterval = time.Hour
	}
	r := New(cfg)
	t.Cleanup(r.Close)
	return r
}

func bind(t *testing.T, r *Registry, user, device string) *Session {
	t.Helper()
	s, err := r.Bind(Session{UserID: user, DeviceID: device, ConnID: device + "-conn"})
	if err != nil {
		t.Fatalf("bind %s/%s: %v", user, device, err)
	}
	return s
}

func TestBindAndLookup(t *testing.T) {
	r := newTestRegistry(t, Config{})

	if r.Online("alice") {
		t.Error("alice online before binding")
	}
	bind(t, r, "alice", "phone")

	if !r.Online("alice") {
		t.Error("alice offline after binding")
	}
	s, ok := r.Session("alice", "phone")
	if !ok {
		t.Fatal("session not found")
	}
	if s.NodeID != "node-a" || s.State != StateOnline {
		t.Errorf("session defaults not applied: %+v", s)
	}
}

func TestBindRequiresIdentity(t *testing.T) {
	r := newTestRegistry(t, Config{})
	if _, err := r.Bind(Session{DeviceID: "phone"}); err == nil {
		t.Error("bind without user_id was accepted")
	}
	if _, err := r.Bind(Session{UserID: "alice"}); err == nil {
		t.Error("bind without device_id was accepted")
	}
}

func TestMultiDeviceSessions(t *testing.T) {
	r := newTestRegistry(t, Config{})
	for _, d := range []string{"phone", "laptop", "tablet"} {
		bind(t, r, "alice", d)
	}

	sessions := r.Sessions("alice")
	if len(sessions) != 3 {
		t.Fatalf("got %d sessions, want 3", len(sessions))
	}

	// Losing one device must not mark the user offline — the whole point of
	// tracking devices separately.
	r.Unbind("alice", "phone", "")
	if !r.Online("alice") {
		t.Error("alice went offline after one of three devices disconnected")
	}
	r.Unbind("alice", "laptop", "")
	r.Unbind("alice", "tablet", "")
	if r.Online("alice") {
		t.Error("alice still online after all devices disconnected")
	}
}

func TestRebindReplacesSession(t *testing.T) {
	r := newTestRegistry(t, Config{})

	r.Bind(Session{UserID: "alice", DeviceID: "phone", ConnID: "conn-1"})
	r.Bind(Session{UserID: "alice", DeviceID: "phone", ConnID: "conn-2"})

	if n := len(r.Sessions("alice")); n != 1 {
		t.Fatalf("reconnect created %d sessions, want 1", n)
	}
	s, _ := r.Session("alice", "phone")
	if s.ConnID != "conn-2" {
		t.Errorf("newest connection did not win: %s", s.ConnID)
	}
}

func TestUnbindGuardsAgainstStaleConnID(t *testing.T) {
	r := newTestRegistry(t, Config{})

	r.Bind(Session{UserID: "alice", DeviceID: "phone", ConnID: "conn-1"})
	r.Bind(Session{UserID: "alice", DeviceID: "phone", ConnID: "conn-2"})

	// The old connection's close arrives late. It must not evict the new one.
	if r.Unbind("alice", "phone", "conn-1") {
		t.Error("stale unbind evicted the current session")
	}
	if !r.Online("alice") {
		t.Fatal("alice was knocked offline by a stale disconnect")
	}

	if !r.Unbind("alice", "phone", "conn-2") {
		t.Error("current connection could not unbind itself")
	}
	if r.Online("alice") {
		t.Error("alice still online after the current connection closed")
	}
}

func TestTouchAndExpiry(t *testing.T) {
	r := newTestRegistry(t, Config{TTL: 50 * time.Millisecond})
	bind(t, r, "alice", "phone")

	if !r.Touch("alice", "phone") {
		t.Error("touch on a live session failed")
	}
	if r.Touch("alice", "unknown-device") {
		t.Error("touch on an unknown session succeeded")
	}

	time.Sleep(80 * time.Millisecond)
	if n := r.Sweep(); n != 1 {
		t.Errorf("sweep removed %d sessions, want 1", n)
	}
	if r.Online("alice") {
		t.Error("lapsed session survived the sweep")
	}
}

func TestHeartbeatKeepsSessionAlive(t *testing.T) {
	r := newTestRegistry(t, Config{TTL: 100 * time.Millisecond})
	bind(t, r, "alice", "phone")

	for i := 0; i < 5; i++ {
		time.Sleep(30 * time.Millisecond)
		r.Touch("alice", "phone")
		r.Sweep()
	}
	if !r.Online("alice") {
		t.Error("a heartbeating session was swept")
	}
}

func TestSetState(t *testing.T) {
	r := newTestRegistry(t, Config{})
	bind(t, r, "alice", "phone")

	if !r.SetState("alice", "phone", StateAway) {
		t.Fatal("SetState failed")
	}
	s, _ := r.Session("alice", "phone")
	if s.State != StateAway {
		t.Errorf("state = %s, want away", s.State)
	}
	if r.SetState("alice", "nope", StateAway) {
		t.Error("SetState on an unknown device succeeded")
	}
}

func TestRoutesPreferLocal(t *testing.T) {
	r := newTestRegistry(t, Config{NodeID: "node-a"})

	r.Bind(Session{UserID: "alice", DeviceID: "remote", NodeID: "node-b", ConnID: "c1"})
	r.Bind(Session{UserID: "alice", DeviceID: "local", NodeID: "node-a", ConnID: "c2"})

	routes := r.Routes("alice")
	if len(routes) != 2 {
		t.Fatalf("got %d routes, want 2", len(routes))
	}
	if !routes[0].Local {
		t.Error("local route was not ordered first")
	}
	if routes[1].Local {
		t.Error("remote route was marked local")
	}
}

func TestOfflineUsers(t *testing.T) {
	r := newTestRegistry(t, Config{})
	bind(t, r, "alice", "phone")
	bind(t, r, "bob", "phone")

	offline := r.OfflineUsers([]string{"alice", "bob", "carol", "dave"})
	if len(offline) != 2 {
		t.Fatalf("got %v, want carol and dave", offline)
	}
	got := map[string]bool{offline[0]: true, offline[1]: true}
	if !got["carol"] || !got["dave"] {
		t.Errorf("wrong offline set: %v", offline)
	}
}

func TestRoutesForUsersSkipsOffline(t *testing.T) {
	r := newTestRegistry(t, Config{})
	bind(t, r, "alice", "phone")

	routes := r.RoutesForUsers([]string{"alice", "ghost"})
	if len(routes) != 1 {
		t.Fatalf("got %d entries, want 1", len(routes))
	}
	if _, ok := routes["ghost"]; ok {
		t.Error("offline user included in routes")
	}
}

func TestEvictNode(t *testing.T) {
	r := newTestRegistry(t, Config{NodeID: "node-a"})

	r.Bind(Session{UserID: "alice", DeviceID: "d1", NodeID: "node-a", ConnID: "c1"})
	r.Bind(Session{UserID: "bob", DeviceID: "d1", NodeID: "node-b", ConnID: "c2"})
	r.Bind(Session{UserID: "carol", DeviceID: "d1", NodeID: "node-b", ConnID: "c3"})

	// A node leaving must take its sessions with it immediately, otherwise
	// messages route to a dead process until the TTL lapses.
	if n := r.EvictNode("node-b"); n != 2 {
		t.Errorf("evicted %d sessions, want 2", n)
	}
	if !r.Online("alice") {
		t.Error("evicting node-b removed a node-a session")
	}
	if r.Online("bob") || r.Online("carol") {
		t.Error("node-b sessions survived eviction")
	}
}

func TestWatchEvents(t *testing.T) {
	r := newTestRegistry(t, Config{})

	ch, cancel := r.WatchUsers([]string{"alice"})
	defer cancel()

	bind(t, r, "alice", "phone")
	bind(t, r, "bob", "phone") // filtered out

	select {
	case ev := <-ch:
		if ev.Type != EventBound || ev.UserID != "alice" {
			t.Errorf("unexpected event: %+v", ev)
		}
		if !ev.UserOnline {
			t.Error("bind event did not report the user as online")
		}
	case <-time.After(time.Second):
		t.Fatal("no bind event received")
	}

	r.Unbind("alice", "phone", "")
	select {
	case ev := <-ch:
		if ev.Type != EventUnbound {
			t.Errorf("expected unbound, got %s", ev.Type)
		}
		if ev.UserOnline {
			t.Error("unbind of the last device still reported online")
		}
	case <-time.After(time.Second):
		t.Fatal("no unbind event received")
	}

	// Nothing about bob should ever have arrived.
	select {
	case ev := <-ch:
		if ev.UserID == "bob" {
			t.Error("watch filter leaked another user's events")
		}
	default:
	}
}

func TestWatchDoesNotBlockOnSlowWatcher(t *testing.T) {
	r := newTestRegistry(t, Config{WatcherBuffer: 2})

	_, cancel := r.Watch(nil) // never drained
	defer cancel()

	// A watcher that never reads must not be able to stall binds.
	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; i < 200; i++ {
			r.Bind(Session{UserID: fmt.Sprintf("u%d", i), DeviceID: "d", ConnID: "c"})
		}
	}()

	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("binds blocked behind a slow presence watcher")
	}
}

func TestStats(t *testing.T) {
	r := newTestRegistry(t, Config{NodeID: "node-a", Region: "eu"})
	r.Bind(Session{UserID: "alice", DeviceID: "phone", ConnID: "c1"})
	r.Bind(Session{UserID: "alice", DeviceID: "laptop", ConnID: "c2", State: StateAway})
	r.Bind(Session{UserID: "bob", DeviceID: "phone", NodeID: "node-b", Region: "us", ConnID: "c3"})

	s := r.Stats()
	if s.Users != 2 || s.Sessions != 3 {
		t.Errorf("users=%d sessions=%d, want 2 and 3", s.Users, s.Sessions)
	}
	if s.ByNode["node-a"] != 2 || s.ByNode["node-b"] != 1 {
		t.Errorf("ByNode = %v", s.ByNode)
	}
	if s.ByRegion["eu"] != 2 || s.ByRegion["us"] != 1 {
		t.Errorf("ByRegion = %v", s.ByRegion)
	}
	if s.ByState[StateOnline] != 2 || s.ByState[StateAway] != 1 {
		t.Errorf("ByState = %v", s.ByState)
	}
}

func TestConcurrentBindTouchUnbind(t *testing.T) {
	r := newTestRegistry(t, Config{TTL: time.Hour})

	var wg sync.WaitGroup
	for u := 0; u < 20; u++ {
		wg.Add(1)
		go func(u int) {
			defer wg.Done()
			user := fmt.Sprintf("user-%d", u)
			for i := 0; i < 100; i++ {
				device := fmt.Sprintf("d%d", i%3)
				r.Bind(Session{UserID: user, DeviceID: device, ConnID: fmt.Sprint(i)})
				r.Touch(user, device)
				r.Sessions(user)
				r.Online(user)
				if i%7 == 0 {
					r.Unbind(user, device, "")
				}
			}
		}(u)
	}
	wg.Wait()
	r.Stats() // must not panic on a concurrently-mutated table
}

func TestPresenceTopicRoundTrip(t *testing.T) {
	topic := PresenceTopic("alice")
	if topic != "presence.alice" {
		t.Fatalf("PresenceTopic = %q", topic)
	}
	if u, ok := UserFromPresenceTopic(topic); !ok || u != "alice" {
		t.Errorf("UserFromPresenceTopic = %q, %v", u, ok)
	}
	if u, ok := UserFromPresenceTopic("presence.alice.status"); !ok || u != "alice" {
		t.Errorf("sub-topic extraction failed: %q, %v", u, ok)
	}
	if _, ok := UserFromPresenceTopic("chat.inbox.alice"); ok {
		t.Error("non-presence topic parsed as presence")
	}
}

func BenchmarkTouch(b *testing.B) {
	r := New(Config{NodeID: "n", SweepInterval: time.Hour})
	defer r.Close()
	r.Bind(Session{UserID: "alice", DeviceID: "phone", ConnID: "c"})

	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			r.Touch("alice", "phone")
		}
	})
}

func BenchmarkOnline(b *testing.B) {
	r := New(Config{NodeID: "n", SweepInterval: time.Hour})
	defer r.Close()
	for i := 0; i < 10000; i++ {
		r.Bind(Session{UserID: fmt.Sprintf("u%d", i), DeviceID: "d", ConnID: "c"})
	}

	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		i := 0
		for pb.Next() {
			r.Online(fmt.Sprintf("u%d", i%10000))
			i++
		}
	})
}
