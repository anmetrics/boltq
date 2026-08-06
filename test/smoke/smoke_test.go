//go:build smoke

// Package smoke drives a running BoltQ server over the real WebSocket gateway
// and asserts that the admin endpoints backing the web console reflect the
// traffic.
//
// It is excluded from the normal test run by a build tag because it needs a
// server already listening. Run it with:
//
//	go test -tags smoke ./test/smoke/
package smoke

import (
	"encoding/json"
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/gorilla/websocket"

	"github.com/boltq/boltq/internal/identity"
)

const (
	adminURL   = "http://127.0.0.1:19190"
	gatewayURL = "ws://127.0.0.1:19195/ws"
)

var key = identity.SigningKey{ID: "k1", Secret: []byte("0123456789abcdef0123456789abcdef")}

func dial(t *testing.T, user, device string) *websocket.Conn {
	t.Helper()
	tok, err := identity.Sign(key, identity.Claims{
		Subject: user, DeviceID: device,
		ExpiresAt: time.Now().Add(time.Hour).Unix(),
	})
	if err != nil {
		t.Fatalf("sign: %v", err)
	}

	c, _, err := websocket.DefaultDialer.Dial(gatewayURL+"?token="+tok, nil)
	if err != nil {
		t.Fatalf("dial %s: %v", user, err)
	}
	t.Cleanup(func() { c.Close() })

	c.WriteJSON(map[string]any{"op": "hello", "id": "h", "version": 1, "device_id": device})
	c.SetReadDeadline(time.Now().Add(3 * time.Second))
	var f map[string]any
	if err := c.ReadJSON(&f); err != nil {
		t.Fatalf("welcome for %s: %v", user, err)
	}
	if f["op"] != "welcome" {
		t.Fatalf("%s got op=%v, want welcome", user, f["op"])
	}
	return c
}

func getJSON(t *testing.T, path string, out any) {
	t.Helper()
	resp, err := http.Get(adminURL + path)
	if err != nil {
		t.Fatalf("GET %s: %v", path, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET %s returned %d", path, resp.StatusCode)
	}
	if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
		t.Fatalf("decode %s: %v", path, err)
	}
}

func TestSmokeEndToEnd(t *testing.T) {
	alice := dial(t, "alice", "alice-phone")
	aliceLaptop := dial(t, "alice", "alice-laptop")
	bob := dial(t, "bob", "bob-phone")

	conv := "alice:bob"
	bob.WriteJSON(map[string]any{"op": "subscribe", "id": "s1", "topic": "chat.direct." + conv})

	const n = 12
	for i := 0; i < n; i++ {
		alice.WriteJSON(map[string]any{
			"op": "send", "id": fmt.Sprintf("m%d", i), "kind": "direct",
			"conversation": conv, "client_msg_id": fmt.Sprintf("c%d", i),
			"payload": []byte(fmt.Sprintf("hello %d", i)),
		})
	}

	alice.WriteJSON(map[string]any{"op": "typing", "id": "t1", "conversation": conv, "typing": true})
	aliceLaptop.WriteJSON(map[string]any{"op": "presence", "id": "p1", "state": "away"})

	received := 0
	deadline := time.Now().Add(5 * time.Second)
	for received < n && time.Now().Before(deadline) {
		bob.SetReadDeadline(time.Now().Add(500 * time.Millisecond))
		var f map[string]any
		if err := bob.ReadJSON(&f); err != nil {
			continue
		}
		if f["op"] == "record" {
			if recs, ok := f["records"].([]any); ok {
				received += len(recs)
			}
		}
	}
	if received < n {
		t.Errorf("bob received %d of %d records over the socket", received, n)
	}
	t.Logf("bob received %d records", received)

	bob.WriteJSON(map[string]any{"op": "commit", "id": "c1", "topic": "chat.inbox.bob", "seq": 5})
	time.Sleep(700 * time.Millisecond)

	// --- The admin endpoints the console reads must reflect all of that ---

	var streams struct {
		Topics []struct {
			Name       string `json:"name"`
			TotalBytes int64  `json:"total_bytes"`
			Partitions []struct {
				ID      int32  `json:"id"`
				NextSeq uint64 `json:"next_seq"`
				Records uint64 `json:"records"`
			} `json:"partitions"`
		} `json:"topics"`
		Cursors struct {
			Tracked int `json:"tracked"`
		} `json:"cursors"`
	}
	getJSON(t, "/streams", &streams)

	byName := map[string]int{}
	for i, tp := range streams.Topics {
		byName[tp.Name] = i
	}
	for _, want := range []string{"chat.direct.alice:bob", "chat.inbox.alice", "chat.inbox.bob"} {
		if _, ok := byName[want]; !ok {
			t.Errorf("topic %q missing from /streams (got %v)", want, byName)
		}
	}
	if streams.Cursors.Tracked == 0 {
		t.Error("/streams reports no cursors after a commit")
	}

	var presence struct {
		Users    int            `json:"users"`
		Sessions int            `json:"sessions"`
		ByState  map[string]int `json:"by_state"`
		ByRegion map[string]int `json:"by_region"`
	}
	getJSON(t, "/presence", &presence)
	if presence.Users != 2 {
		t.Errorf("/presence users = %d, want 2", presence.Users)
	}
	if presence.Sessions != 3 {
		t.Errorf("/presence sessions = %d, want 3", presence.Sessions)
	}
	if presence.ByState["away"] != 1 {
		t.Errorf("/presence by_state = %v, want one away device", presence.ByState)
	}
	// by_region is only populated when messaging.presence.region is configured;
	// its absence is a configuration choice, not a failure.
	if len(presence.ByRegion) > 0 {
		total := 0
		for _, n := range presence.ByRegion {
			total += n
		}
		if total != presence.Sessions {
			t.Errorf("/presence by_region sums to %d but there are %d sessions",
				total, presence.Sessions)
		}
	}

	var gw struct {
		Connections int `json:"connections"`
		Attached    int `json:"attached"`
		Sessions    int `json:"sessions"`
		RecordsOut  int `json:"records_out"`
	}
	getJSON(t, "/gateway/stats", &gw)
	if gw.Connections < 3 || gw.Attached < 3 {
		t.Errorf("/gateway/stats connections=%d attached=%d, want at least 3", gw.Connections, gw.Attached)
	}
	if gw.RecordsOut < n {
		t.Errorf("/gateway/stats records_out = %d, want at least %d", gw.RecordsOut, n)
	}

	var cursors struct {
		Members map[string]uint64 `json:"members"`
		NextSeq uint64            `json:"next_seq"`
		Lag     uint64            `json:"lag"`
	}
	getJSON(t, "/streams/cursors?topic=chat.inbox.bob&partition=0&group=user:bob", &cursors)
	if cursors.Members["bob-phone"] != 5 {
		t.Errorf("cursor for bob-phone = %v, want 5", cursors.Members)
	}

	var overview struct {
		Streams  map[string]any `json:"streams"`
		Presence map[string]any `json:"presence"`
		Gateway  map[string]any `json:"gateway"`
		Signals  map[string]any `json:"signals"`
	}
	getJSON(t, "/messaging/overview", &overview)
	for name, section := range map[string]map[string]any{
		"streams": overview.Streams, "presence": overview.Presence,
		"gateway": overview.Gateway, "signals": overview.Signals,
	} {
		if section == nil {
			t.Errorf("/messaging/overview section %q is null", name)
		}
	}

	t.Logf("streams topics=%d cursors=%d | presence users=%d sessions=%d | gateway records_out=%d",
		len(streams.Topics), streams.Cursors.Tracked,
		presence.Users, presence.Sessions, gw.RecordsOut)
}
