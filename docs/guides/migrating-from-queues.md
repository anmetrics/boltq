# Migrating from queues

For deployments already running BoltQ as a work queue.

## The short answer

**Nothing changes.** The messaging subsystem is off by default. If you do not
set `messaging.stream.enabled`, BoltQ behaves exactly as it did before — same
ports, same wire protocol, same broker, same Raft cluster, and it does not even
create the streams directory.

You can stop reading here if you only run jobs.

## What was added

Eight new packages, all inert unless enabled:

| Package | Purpose |
|---|---|
| `internal/stream` | Partitioned append-only log with cursors |
| `internal/identity` | Per-user tokens and topic ACL |
| `internal/presence` | Which device is connected where |
| `internal/dedup` | Idempotency for message submission |
| `internal/fanout` | Conversation delivery |
| `internal/ephemeral` | Typing and presence signals |
| `internal/outbox` | Offline push dispatch |
| `internal/gateway` | WebSocket edge |

One new dependency: `github.com/gorilla/websocket`.

## What was not touched

- `internal/broker` — the queue broker, unchanged.
- `internal/queue` — the ring buffers, unchanged.
- `internal/wal` — the queue's write-ahead log, unchanged.
- `internal/cluster` — Raft, unchanged.
- `pkg/protocol` — the TCP wire protocol, unchanged. No new command bytes.
- The TCP server, the Go and Node SDKs, the web dashboard.

The stream engine is a **separate** storage system under
`<data_dir>/streams`. It does not read or write the queue's WAL, and the
queue's compaction does not touch stream segments.

## Additions to existing files

Three, all additive:

1. **`internal/config`** — a `messaging` block, defaulting to disabled.
2. **`internal/api/http.go`** — one field on `HTTPServer`, plus a `Handle`
   method and `SetMessagingStats`. Existing routes are untouched.
3. **`cmd/server/main.go`** — builds the messaging stack if enabled, mounts the
   gateway, shuts it down on signal.

## Should you use both?

Yes, if the workloads are genuinely different — and they usually are.

```
Work queue                          Stream
──────────                          ──────
image resizing                      chat messages
email sending                       conversation history
webhook delivery                    activity feeds
payment reconciliation              audit logs
```

The rule of thumb: **if a consumer should process it once and forget it, use
the queue. If a reader might need it again — for history, for a second device,
for a new consumer — use a stream.**

The two subsystems share nothing but the process. A stream backlog does not slow
job processing, and a job spike does not delay messages.

## Do not port queues to streams reflexively

A work queue is the *right* model for jobs. Streams do not delete on read, so a
queue converted to a stream grows forever, and competing consumers become
something you have to coordinate yourself with cursor groups.

Convert only when you actually want replay.

## Enabling incrementally

### Stage 1 — the log only

```json
{ "messaging": { "stream": { "enabled": true } } }
```

Now you have a partitioned log usable from Go. Nothing is exposed over the
network — no gateway, no auth changes. A good way to try the storage model.

### Stage 2 — authentication

```json
{ "messaging": {
    "stream":   { "enabled": true },
    "identity": { "enabled": true, "allow_anonymous": true,
                  "keys": [{ "id": "k1", "secret_env": "BOLTQ_KEY" }] }
} }
```

`allow_anonymous: true` keeps your existing shared-API-key backends working
exactly as before while user tokens become available. This is the important
compatibility switch — turning it off would break every backend client at once.

### Stage 3 — the gateway

```json
{ "messaging": {
    "stream":   { "enabled": true },
    "identity": { "enabled": true, … },
    "gateway":  { "enabled": true, "port": 9095 }
} }
```

A new port for end-user clients. The TCP and admin ports are unaffected.

### Stage 4 — the rest

Membership service, push webhook, retention policy. See the
[chat app guide](building-a-chat-app.md).

## Resource impact when enabled

| Resource | Impact |
|---|---|
| Disk | New. Grows with message volume; unlimited by default |
| Memory | ~1GB at 100k users — see [Capacity](../operations/capacity.md) |
| File handles | Two per segment plus one per connection. Raise `ulimit -n` |
| Goroutines | 2–3 per connection, one per push-watched partition |
| CPU | Negligible when idle |

The one to plan for is **file handles**. An inbox topic per user means a lot of
files at scale, and hitting the limit presents as confusing failures rather than
a clear error.

## Rolling back

Set `messaging.stream.enabled: false` and restart. The queue subsystem is
unaffected. Stream data stays on disk under `<data_dir>/streams` and is
recovered intact if you re-enable.

To remove it entirely, delete that directory while the server is stopped.

## Known issues in the existing suite

Three test failures predate this work and are unrelated to it. They are recorded
here so nobody attributes them to the messaging subsystem:

- `internal/api` — `TestMessagingEndpointsRemoved` hangs. It asserts `/consume`
  returns 404, but the route is still registered and the handler blocks.
- `internal/cluster` — times out on Raft transport.
- `test/integration` — `TestSDKEndToEnd` fails on a port collision (`9095`).

All three reproduce at commit `1cb48ce`, before any messaging code existed.

Note that `9095` is also the gateway port suggested in these docs — pick a
different one if you plan to fix that integration test.

## Further reading

- [Why a log](../architecture/why-a-log.md) — the reasoning.
- [Building a chat app](building-a-chat-app.md).
- [Configuration](../reference/configuration.md).
