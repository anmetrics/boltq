# Presence and typing

Two features that look small and are not: they generate more traffic than
messages do, and getting their reliability model wrong is how a chat backend
falls over.

## The volume problem

A user types for ten seconds and sends one message. If the client emits a
typing signal every second, that is **ten signals per message**. Add presence
heartbeats, read receipts and "viewing profile" markers and ephemeral traffic
routinely runs an order of magnitude above message traffic.

None of it is worth a disk write. A typing indicator is worthless one second
after it is sent, and one recovered from before a process crash is not
information — it is noise that would make the UI lie.

So ephemeral signals take a completely separate path: **memory only, best
effort, rate limited, dropped without ceremony when a subscriber is slow**.
Routing them through the durable log would mean the least valuable traffic in
the system consumed most of its write capacity.

## Typing indicators

### Sending

```json
→ { "op": "typing", "id": "t1", "conversation": "alice:bob", "typing": true }
← { "op": "ack", "id": "t1" }
```

### Receiving

```json
← { "op": "signal",
    "signal": { "topic": "typing.alice:bob", "sender": "alice",
                "kind": "typing", "at": 1730000000000000000 } }
```

`kind` is `typing` or `stop_typing`. You never receive your own signals echoed
back.

### Subscribing

You are subscribed to a conversation's typing topic **automatically on your
first `typing` frame for it**. To receive others' indicators without claiming to
type, send a `stop_typing` when opening the conversation:

```javascript
async openConversation(convID) {
  await this.request({ op: 'subscribe', topic: `chat.direct.${convID}` });
  await this.request({ op: 'typing', conversation: convID, typing: false });
}
```

### Client throttling

The server rate-limits at 5 signals/second per user with a burst of 20. Your
client should be well under that:

```javascript
class TypingSignal {
  constructor(client, convID) {
    this.client = client;
    this.convID = convID;
    this.active = false;
  }

  onKeystroke() {
    if (!this.active) {
      this.active = true;
      this.client.request({ op: 'typing', conversation: this.convID, typing: true });
    }
    // Refresh the stop timer on every keystroke; only send "typing" once.
    clearTimeout(this.stopTimer);
    this.stopTimer = setTimeout(() => this.stop(), 3000);
  }

  stop() {
    if (!this.active) return;
    this.active = false;
    clearTimeout(this.stopTimer);
    this.client.request({ op: 'typing', conversation: this.convID, typing: false });
  }
}
```

One signal on the first keystroke, one when typing stops. That is two signals
per message instead of ten.

Getting `rate_limited` back means "you are sending too often" — treat it as a
signal to throttle, not as an error to surface to the user.

### What you must accept

- **Signals are dropped** when a subscriber's buffer is full. One stalled phone
  must never be able to slow down everyone else in a conversation.
- **Nothing survives a restart.**
- **There is no delivery confirmation.** The `ack` means the server accepted the
  signal, not that anyone received it.

Design the UI so a missing "stopped typing" is harmless. Always run a local
timeout that clears the indicator after a few seconds regardless of what the
server says:

```javascript
onTypingSignal(sig) {
  if (sig.kind === 'typing') {
    this.ui.showTyping(sig.sender);
    clearTimeout(this.clearTimers[sig.sender]);
    // Never rely on receiving the stop signal — it may have been dropped.
    this.clearTimers[sig.sender] = setTimeout(
      () => this.ui.hideTyping(sig.sender), 5000);
  } else {
    this.ui.hideTyping(sig.sender);
  }
}
```

## Presence

Presence is more structured than typing — it is tracked in a registry rather
than being pure fire-and-forget — but it is still in memory only.

### The model

```
user → device → session { node, region, conn_id, state, last_seen }
```

A user is **online while any device is connected**. That aggregate is what a
contact list wants; individual device state is available but rarely what you
display.

### Automatic binding

Connecting binds presence. Disconnecting unbinds it. You do not need to do
anything for basic online/offline to work.

### Reporting state

```json
→ { "op": "presence", "id": "p1", "state": "away" }
```

| State | Meaning | Push behaviour |
|---|---|---|
| `online` | Connected, foreground | No push |
| `away` | Connected, backgrounded | Push still warranted for mentions |
| `offline` | Not connected | Everything goes to the outbox |

Send `away` when your app backgrounds. It is what lets the push dispatcher
distinguish "they are looking at this" from "they have a socket open but are
not looking".

### Watching contacts

```json
→ { "op": "watch_presence", "id": "w1", "users": ["bob", "carol"] }
← { "op": "ack", "id": "w1" }

← { "op": "presence_event",
    "presence": { "user_id": "bob", "device_id": "phone",
                  "state": "online", "online": true, "at": … } }
```

`online` is the aggregate. `state` and `device_id` describe the device that
changed.

Calling `watch_presence` again **replaces** the entire watch set — send the full
list each time, not a delta.

Each user is authorised as a read of `presence.<user>`, so watching someone you
may not observe returns `forbidden`.

### Heartbeats and expiry

A session expires after `presence.ttl` (default 90s) without a heartbeat. Two
things refresh it:

- The gateway's WebSocket ping, every `pong_timeout / 3`.
- An explicit `{ "op": "ping" }` frame.

You normally do not need to send anything — the transport keeps presence alive.

Expiry exists because mobile clients vanish without closing. Without a TTL, a
crashed client would appear online forever and every message to them would route
to a dead socket instead of a push notification.

### Reconnect races

When a phone changes network it reconnects before the old socket times out. The
registry replaces the session for that device — newest connection wins — and a
late-arriving close for the *old* connection is ignored, because it carries the
old `conn_id`.

Without that guard, a delayed disconnect would knock a healthy new connection
offline. It is tested by `TestUnbindGuardsAgainstStaleConnID`.

## The presence ACL is asymmetric

By default a user writes only their own presence but may read **anyone's**:

```go
{ Pattern: "presence.${user}.#", Effect: Allow, Actions: []Action{ActionWrite} },
{ Pattern: "presence.#",         Effect: Allow, Actions: []Action{ActionRead}  },
```

This is intentional — being able to observe everyone's presence is what a
contact list is. But for a dating app it is very likely **wrong**: users should
not be able to see the online status of people they have not matched with, since
that leaks activity patterns to strangers.

Restrict it:

```go
rules := identity.ChatPolicyRules()
for i := range rules {
    if rules[i].Pattern == "presence.#" {
        rules[i].RequireMembership = true
        rules[i].MembershipSegment = 1   // presence.<userID>
    }
}
policy.SetRules(rules)
```

Then your membership service answers "may user A observe user B" by treating the
observed user's ID as the group.

## Limits

- **Presence is per node.** A sharded deployment needs an external presence
  store — the registry does not gossip. See [Global HA](../operations/global-ha.md).
- **No last-seen timestamp is exposed** beyond what a watcher observes live.
  Store it yourself from presence events if your product shows "last seen 2h
  ago".
- **No custom status text.** Presence carries a state, not a message. Put a
  status message in your own profile store.
- **Watchers are dropped silently** if they fall behind. The drop count is
  tracked internally but not exposed per watcher.

## Further reading

- [Gateway protocol](../reference/gateway-protocol.md)
- [Authentication](auth.md) — customising the presence ACL
- [Offline push](offline-push.md) — how presence drives push decisions
