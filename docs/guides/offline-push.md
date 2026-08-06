# Offline push

Delivering to users who are not connected.

## Why this is a separate subsystem

A message for an online user needs nothing extra — their device is tailing the
log and wakes on the append. A message for an offline user needs a push
notification, and that is a fundamentally different kind of delivery:

- It leaves the system, to a third party you do not control.
- It costs money per send.
- It can fail for hours.
- It must not be retried forever.

Putting any of that on the message send path would mean Alice's message to Bob
waits on APNs. So the dispatcher is a **normal log consumer with its own
cursor**, running behind the write path. If the push provider is down, the
cursor stops advancing and resumes when it recovers. No queue to overflow, no
messages lost, and no coupling between "the message was stored" and "the
notification was sent".

## How it works

```
chat.inbox.bob  ──▶  dispatcher (cursor group: "push-dispatcher")
                          │
                          ├─ presence.Online("bob")?  ──▶ yes: skip
                          │
                          ├─ wait grace_delay
                          │
                          ├─ batch notifications
                          │
                          ├─ POST your webhook  ──▶  you  ──▶ APNs/FCM
                          │
                          └─ commit cursor
```

The dispatcher tails inbox topics, checks presence, batches, calls your webhook,
then advances its cursor. Because the cursor is durable, a restart resumes
exactly where it left off.

## BoltQ does not speak APNs or FCM

Deliberately. Push credentials rotate, payload formats are platform-specific
and change, and what a notification should *say* is product logic, not broker
logic. BoltQ hands you a batch; you decide.

## Configuration

```json
{
  "messaging": {
    "push": {
      "enabled": true,
      "webhook_url": "https://api.example.com/boltq/push",
      "auth_header": "Bearer <service-token>",
      "timeout": "10s",
      "max_attempts": 5,
      "grace_delay": "3s",
      "scan_interval": "30s"
    }
  }
}
```

**`grace_delay`** — a user who opens the app within a couple of seconds should
read the message in the app, not receive a notification for something they are
already looking at. Three seconds is a reasonable default; raise it if your
clients are slow to report presence.

**`max_attempts`** — bounded on purpose. Without a bound, one permanently bad
batch stalls every notification behind it. After the limit the batch is dropped
and the cursor advances. Watch `push.dropped`.

**`scan_interval`** — how often new inbox topics are discovered. Inbox topics
are created lazily on a user's first message, so this only affects how quickly a
brand-new user's first notification goes out.

## The webhook

```http
POST /boltq/push
Authorization: Bearer <service-token>
Content-Type: application/json

{
  "notifications": [
    {
      "user_id": "bob",
      "message_id": "9f3a…",
      "conversation_id": "alice:bob",
      "kind": "direct",
      "sender_id": "alice",
      "conv_topic": "chat.direct.alice:bob",
      "conv_partition": 7,
      "conv_seq": 1042,
      "headers": { "sender": "alice", "message_id": "9f3a…", … },
      "at": 1730000000000000000,
      "attempt": 1
    }
  ],
  "count": 1,
  "sent_at": 1730000003000000000
}
```

`conv_topic`, `conv_partition` and `conv_seq` let the receiving client jump
straight to the message once it opens the app.

**There is no payload.** The message body is not included, because for an
end-to-end encrypted app the server cannot read it anyway. If you want the
message text in the notification, either fetch it yourself or — for an
encrypted app — send a contentless alert and let the client decrypt after
launch.

Respond `2xx` for success. Anything else triggers a retry with exponential
backoff.

### Implementation

```go
func pushWebhook(w http.ResponseWriter, r *http.Request) {
    var payload struct {
        Notifications []Notification `json:"notifications"`
    }
    if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
        w.WriteHeader(http.StatusBadRequest)
        return
    }

    // Group by user so one person receiving five messages gets one
    // notification, not five.
    byUser := map[string][]Notification{}
    for _, n := range payload.Notifications {
        byUser[n.UserID] = append(byUser[n.UserID], n)
    }

    for userID, ns := range byUser {
        // Dispatch is at-least-once: a crash between sending and committing
        // the cursor re-presents a batch. Deduplicate on your side.
        newest := ns[len(ns)-1]
        if alreadyNotified(userID, newest.ConversationID, newest.ConvSeq) {
            continue
        }

        tokens, err := deviceTokens(userID)
        if err != nil {
            // Returning an error retries the WHOLE batch, including
            // notifications that already succeeded. Only do it for failures
            // that are genuinely transient and batch-wide.
            w.WriteHeader(http.StatusInternalServerError)
            return
        }

        alert := buildAlert(ns)   // "alice: 3 new messages"
        for _, tok := range tokens {
            sendAPNs(tok, alert)
        }
        markNotified(userID, newest.ConversationID, newest.ConvSeq)
    }

    w.WriteHeader(http.StatusOK)
}
```

Two things worth care:

**Idempotency.** At-least-once means you will see a batch twice eventually.
Deduplicate on `(user_id, conversation_id, conv_seq)`.

**Partial failure.** Returning non-2xx retries the entire batch. If one user's
push fails, prefer to log it and return 200 rather than re-notifying everyone
else in the batch.

## Suppression

A notification is skipped when `presence.Online(user)` is true — the user has at
least one connected device.

This is why clients should report `away` when backgrounded rather than staying
`online`: an `away` device is still connected, so BoltQ suppresses the push,
which is right for a foregrounded app and wrong for a backgrounded one.

**If you want pushes to backgrounded devices**, the suppression check needs to
consider state, not just connectedness. That is not currently configurable —
`Online()` returns true for any live session regardless of state. Work around it
by having the client disconnect when backgrounded, or by treating `away` as
disconnected in your own layer.

Check suppression is working:

```
GET /messaging/overview  →  push.suppressed
```

## Monitoring

```
GET /streams/cursors?topic=chat.inbox.bob&group=push-dispatcher
```

```json
{ "watermark": 1097, "next_seq": 1100, "lag": 3 }
```

Sustained lag means the webhook is slow or failing.

```
GET /messaging/overview  →  push
```

```json
{ "scanned": 90211, "suppressed": 62104, "sent": 28100, "failed": 7, "dropped": 0 }
```

**`dropped` is the one to alert on.** It means batches exceeded `max_attempts`
and notifications were abandoned. Any non-zero rate warrants investigation.

## Backfill behaviour

A **fresh dispatcher starts at the head**, not at the beginning of each inbox.

This matters enormously: without it, turning on push notifications on an
existing deployment would notify every user about every message they had ever
received. Tested by `TestFreshDispatcherStartsAtHead`.

The consequence is that messages that arrived while push was disabled are never
notified. That is the correct trade.

## Limits

- **No per-user preferences.** BoltQ notifies for everything; muting,
  do-not-disturb and per-conversation settings belong in your webhook handler.
- **No notification content.** The payload is not included.
- **No delivery receipts.** BoltQ does not know whether APNs delivered.
- **`Online()` ignores state.** See suppression above.
- **One goroutine per watched inbox partition.** At one partition per user
  inbox, that is one goroutine per user with an inbox.

## Further reading

- [Message lifecycle](../architecture/message-lifecycle.md) — where push sits.
- [Presence and typing](presence-and-typing.md) — what drives suppression.
- [Monitoring](../operations/monitoring.md).
