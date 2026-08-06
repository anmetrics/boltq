# Gateway protocol

The WebSocket protocol clients use. Version 1.

## Connecting

```
wss://boltq.example.com/ws?token=<jwt>
```

or, preferably for native clients:

```
GET /ws
Authorization: Bearer <jwt>
```

Authentication happens **before** the upgrade. A bad token gets `401` and no
WebSocket handshake, so an unauthenticated peer never holds a socket.

Browsers cannot set headers on a WebSocket, which is why the query parameter is
supported. Be aware that a URL query string is more likely to appear in proxy
logs than a header; use short-lived tokens.

## Frame envelope

Every message in both directions is a JSON object:

```json
{
  "op": "send",
  "id": "req-42",
  "conversation": "alice:bob",
  "payload": "aGVsbG8="
}
```

- `op` — the operation. Required.
- `id` — correlates a response with its request. Echoed back on responses.
  Server-initiated frames (`record`, `signal`, `presence_event`) carry no `id`.
- `payload` — opaque bytes, base64-encoded by JSON. The server never parses it.

Unknown fields are ignored, so the protocol can grow without breaking clients.

## Handshake

### `hello` → `welcome`

The first frame **must** be `hello`. Anything else closes the connection with
`bad_request`.

```json
→ { "op": "hello", "id": "h1", "version": 1,
    "device_id": "phone-a1b2", "user_agent": "MyApp/2.1 iOS" }

← { "op": "welcome", "id": "h1", "version": 1,
    "session": "kJ2n…", "token": "kJ2n….7fQx…", "resumed": false }
```

- `device_id` — required (falls back to the token's `did` claim). This is the
  cursor member; it must be **stable across reinstalls of the app on the same
  device** or the user's read positions reset.
- `token` in the response is the **resume token**, formatted
  `<sessionID>.<secret>`. Store it; it is a bearer credential.

To resume:

```json
→ { "op": "hello", "id": "h2", "version": 1,
    "device_id": "phone-a1b2", "resume": "kJ2n….7fQx…" }

← { "op": "welcome", "id": "h2", "resumed": true, "session": "kJ2n…" }
```

Resume succeeds only if the token matches (constant-time), the token belongs to
the **same user**, and the session is not already attached. Any failure yields
a fresh session rather than an error — the client's next `subscribe` will
resume from its committed cursor anyway.

A version other than `1` is refused with `unsupported_version`.

## Streaming

### `subscribe` → `ack`, then `record` frames

```json
→ { "op": "subscribe", "id": "s1", "topic": "chat.group.eng-team" }

← { "op": "ack", "id": "s1", "topic": "chat.group.eng-team",
    "partition": 3, "from_seq": 1042, "first_seq": 500, "next_seq": 1100 }

← { "op": "record", "topic": "chat.group.eng-team", "partition": 3,
    "records": [
      { "topic": "chat.group.eng-team", "partition": 3, "seq": 1042,
        "timestamp": 1730000000000000000, "key": "eng-team",
        "headers": { "sender": "alice", "message_id": "9f3a…",
                     "conversation": "eng-team", "kind": "group" },
        "payload": "aGVsbG8=" }
    ] }
```

**Partition resolution.** Omit `partition` and the server resolves it: for a
conversation topic, from the conversation ID; otherwise from your user ID. A
client should not need to know the partition map.

**Position resolution.** Omit `from_seq` and the server uses your committed
cursor, or the partition head if you have none. Set `from_seq: 1` to read from
the beginning.

Records arrive in batches, always in sequence order. `first_seq` and `next_seq`
in the ack let you compute unread count immediately.

Subscriptions are capped per session (`max_subscriptions`, default 200);
exceeding it returns `rate_limited`.

### `unsubscribe` → `ack`

```json
→ { "op": "unsubscribe", "id": "u1", "topic": "chat.group.eng-team", "partition": 3 }
← { "op": "ack", "id": "u1" }
```

### `history` → `record`

```json
→ { "op": "history", "id": "h1", "topic": "chat.direct.alice:bob",
    "from_seq": 1, "limit": 50 }

← { "op": "record", "id": "h1", "records": [ … ],
    "first_seq": 1, "next_seq": 200 }
```

`limit` is capped at `history_limit` (default 200). Paginate with
`from_seq = last.seq + 1`.

### `gap`

Sent when requested records were removed by retention:

```json
← { "op": "gap", "id": "s1", "topic": "chat.group.eng-team", "partition": 3,
    "first_seq": 5000, "next_seq": 9000,
    "error": { "code": "history_gap",
               "message": "requested position was removed by retention" } }
```

You have a genuine hole. Show the user that older messages are unavailable
rather than presenting an incomplete conversation as complete. Streaming
continues from `first_seq`.

## Sending

### `send` → `sent`

```json
→ { "op": "send", "id": "m1",
    "kind": "direct", "conversation": "alice:bob",
    "client_msg_id": "local-8891",
    "payload": "aGVsbG8=",
    "headers": { "content_type": "text/plain" } }

← { "op": "sent", "id": "m1",
    "sent": { "message_id": "9f3a…", "client_msg_id": "local-8891",
              "topic": "chat.direct.alice:bob", "partition": 7,
              "seq": 1042, "timestamp": 1730000000000000000,
              "duplicate": false } }
```

- `kind` — `"direct"` or `"group"`. Defaults to `"direct"`.
- `client_msg_id` — **send this.** It is your idempotency key. Without it, a
  retry over a flaky connection duplicates the message.

`duplicate: true` means this answered a retry; the coordinates are the
original's, so your optimistic local echo resolves to the same message.

Errors:

| Code | Meaning |
|---|---|
| `forbidden` | Not a member of the conversation |
| `not_found` | Conversation has no members |
| `conflict` | An identical send is in flight — retry shortly (`retryable: true`) |

### `commit` → `ack`

```json
→ { "op": "commit", "id": "c1", "topic": "chat.group.eng-team",
    "partition": 3, "seq": 1043 }
← { "op": "ack", "id": "c1" }
```

`seq` is **the next sequence you want**, i.e. `lastProcessed + 1`.

Commits are monotonic: a lower value than stored is silently ignored, so a
late-arriving stale commit cannot rewind your position.

## Ephemeral signals

### `typing` → `ack`, then `signal` frames to others

```json
→ { "op": "typing", "id": "t1", "conversation": "alice:bob", "typing": true }
← { "op": "ack", "id": "t1" }
```

Others in the conversation receive:

```json
← { "op": "signal",
    "signal": { "topic": "typing.alice:bob", "sender": "alice",
                "kind": "typing", "at": 1730000000000000000 } }
```

`kind` is `"typing"` or `"stop_typing"`. You are subscribed to a conversation's
typing topic automatically on your first `typing` frame for it — so send a
`typing: false` on opening a conversation if you want to receive others'
indicators without having typed yourself.

Signals are **best effort**: rate limited (default 5/s per user, burst 20),
dropped when a subscriber is slow, never persisted. Exceeding the rate returns
`rate_limited` with `retryable: true`; treat it as "do not send so often", not
as an error to surface.

You never receive your own signals echoed back.

## Presence

### `presence` → `ack`

```json
→ { "op": "presence", "id": "p1", "state": "away" }
← { "op": "ack", "id": "p1" }
```

`state` is `online`, `away` or `offline`. Send `away` when backgrounded — it is
what lets the push dispatcher decide a notification is still warranted.

### `watch_presence` → `ack`, then `presence_event` frames

```json
→ { "op": "watch_presence", "id": "w1", "users": ["bob", "carol"] }
← { "op": "ack", "id": "w1" }

← { "op": "presence_event",
    "presence": { "user_id": "bob", "device_id": "phone",
                  "state": "online", "online": true,
                  "at": 1730000000000000000 } }
```

`online` is the user's **aggregate** state — true while any device is
connected. That is what a contact list wants; `state` and `device_id` describe
the specific device that changed.

Each user in `users` is authorised as a read of `presence.<user>`, so watching
someone you may not observe returns `forbidden`.

Calling `watch_presence` again **replaces** the whole watch set.

## Keepalive

### `ping` → `pong`

```json
→ { "op": "ping", "id": "k1" }
← { "op": "pong", "id": "k1" }
```

This also refreshes your presence heartbeat. The server independently sends
WebSocket ping control frames every `pong_timeout / 3`; a client that does not
respond to those is disconnected. Most WebSocket libraries answer them
automatically.

## Errors

```json
← { "op": "error", "id": "s1",
    "error": { "code": "forbidden", "message": "access denied",
               "retryable": false } }
```

| Code | Retryable | Meaning |
|---|---|---|
| `bad_request` | no | Malformed or missing fields |
| `unauthenticated` | no | Token invalid or expired — reconnect with a fresh one |
| `forbidden` | no | Policy denied it |
| `not_found` | no | Topic, partition or conversation does not exist |
| `rate_limited` | yes | Back off |
| `conflict` | sometimes | Duplicate in flight, or a second `hello` |
| `internal` | yes | Server-side failure |
| `unsupported_version` | no | Protocol version mismatch |
| `history_gap` | no | Retention removed the requested range |

`forbidden` deliberately carries no detail about *why*. Distinguishing "no such
conversation" from "you are not a member" would be an enumeration oracle.

## Client checklist

1. Reconnect with exponential backoff and jitter.
2. Store the resume token; present it on reconnect.
3. Always send `client_msg_id` on `send`.
4. Sort by `seq`, never by timestamp or arrival order.
5. Deduplicate by `message_id` — delivery is at-least-once.
6. Commit after processing, batched, as `lastSeq + 1`.
7. Handle `gap` by showing a history-unavailable marker.
8. Refresh your token before it expires; expiry is re-checked on every frame.

## Further reading

- [Building a chat app](../guides/building-a-chat-app.md) — worked example.
- [Authentication](../guides/auth.md) — minting tokens.
- [Topic conventions](topics.md).
