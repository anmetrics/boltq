# Monitoring

What to watch, what to alert on, and what each signal actually means.

## Endpoints

```
GET /health                  liveness
GET /metrics                 Prometheus (queue subsystem)
GET /messaging/overview      everything below, in one call
GET /streams                 per-topic log statistics
GET /streams/topic?name=     per-partition detail for one topic
GET /streams/cursors?topic=&partition=&group=   cursor positions and lag
GET /presence                connection counts by node, region, state
GET /gateway/stats           WebSocket edge counters
```

All require the admin API key when one is configured. **Do not expose the admin
port publicly.**

## The four alerts that matter

If you set up nothing else, set up these.

### 1. Disk usage on the stream volume

**Why:** retention defaults to unlimited. Disk grows forever until you make a
decision, and the failure mode when it fills is that appends fail and messages
stop being accepted.

```
GET /streams  →  topics[].total_bytes
```

Alert at 70% of volume, page at 85%. Also alert on *rate of growth* — a sudden
change usually means an abuse pattern or a bug, and you want to know before the
disk tells you.

### 2. Push dispatcher lag

**Why:** it is the difference between "notifications are working" and "users
are silently not being told about messages". It fails quietly.

```
GET /streams/cursors?topic=chat.inbox.<user>&group=push-dispatcher
→ { "lag": 3, "next_seq": 1100, "watermark": 1097 }
```

Sustained lag means the webhook is slow or failing. Cross-reference:

```
GET /messaging/overview  →  push.failed, push.dropped
```

`dropped` is the serious one — it means batches exceeded `max_attempts` and
notifications were **abandoned**. Any non-zero rate of `dropped` warrants
investigation.

### 3. Replication health — separately from `/cluster/status`

**Why:** the Raft cluster and stream replication are independent. A healthy Raft
quorum says nothing about whether chat is being replicated. If the stream leader
dies, `/cluster/status` still reports green while chat is down.

```
GET /replication
```

```json
{ "role": "leader", "followers_connected": 1, "followers": ["follower-1"],
  "min_in_sync": 2, "followers_needed": 1, "quorum_available": true,
  "ack_timeouts": 0, "partitions": [ … ] }
```

Alert on:

- **`quorum_available: false`** — writes are failing right now.
- **`ack_timeouts` rising** — publishers are seeing durability errors.
- **`role: "standalone"`** on a node you believe is replicated.
- A follower's **`connected: false`** or rising **`gaps`**.

### 4. Node liveness

**Why:** there is **no automatic failover** for the messaging plane. A node
that is down means its conversations are unavailable, and nothing recovers them
for you.

```
GET /health
```

Page on failure. Your runbook should say what to do, because BoltQ will not do
it. See [Global HA](global-ha.md).

### 5. Backup age and restore-test recency

**Why:** the stream log is not replicated. Backups are your only durability
story for node loss, and an untested backup is not a backup.

Track this outside BoltQ. Alert if the newest snapshot is older than your RPO,
and if the last successful restore test is older than a month.

## Gateway health

```
GET /gateway/stats
```

```json
{
  "connections": 1284392,      cumulative since start
  "resumed": 84021,            successful session resumes
  "auth_failures": 1204,       rejected before upgrade
  "forbidden": 88,             ACL denials
  "frames_in": 98234012,
  "frames_out": 201938441,
  "records_out": 88203441,
  "slow_client_drops": 12,     clients disconnected for not reading
  "sessions": 84102,           tracked, attached + detached
  "attached": 82911            currently holding a socket
}
```

**`slow_client_drops`** — clients disconnected because their send queue filled.
A trickle is normal (bad networks). A spike means either a client bug or that
`send_buffer` is too small for your record sizes. These clients reconnect and
resume from their cursor, so nothing is lost, but it is a real degradation.

**`auth_failures`** — a background rate is normal (expired tokens on reconnect).
A spike means either your auth service is minting bad tokens or someone is
probing. Alert on the rate, not the total.

**`forbidden`** — should be near zero in a correct client. A sustained rate
means a client is asking for things it cannot have, which is either a bug or an
attack.

**`sessions` vs `attached`** — the gap is sessions in their resume window. A
large gap means lots of clients are dropping and reconnecting, which points at
network problems or a `pong_timeout` that is too aggressive.

**`resumed` / `connections`** — the resume hit rate. Low means clients are not
storing resume tokens correctly, and every reconnect is paying for a full
re-subscribe.

## Presence

```
GET /presence
```

```json
{
  "users": 84102,
  "sessions": 91883,
  "by_node":   { "node-a": 91883 },
  "by_region": { "eu-west-1": 91883 },
  "by_state":  { "online": 88201, "away": 3682 },
  "watchers": 41022
}
```

`sessions / users` is the average device count — a useful sanity check. If it
drifts upward over time, device IDs are probably unstable, which also means
cursors are accumulating.

`by_node` is only meaningful in a sharded deployment where you aggregate across
nodes; the registry itself is per node.

## Stream health

```
GET /streams/topic?name=chat.group.eng-team
```

```json
{
  "name": "chat.group.eng-team",
  "partitions": [
    { "id": 0, "first_seq": 1, "next_seq": 90211, "records": 90210, "bytes": 27063000 }
  ],
  "total_bytes": 27063000
}
```

**Partition skew** is the thing to watch. If one partition holds far more than
the others, one conversation is dominating — check whether the partition count
is too low, or whether a single conversation is genuinely enormous.

`first_seq > 1` means retention has removed history from that partition.

## Cursor lag for any consumer

The same endpoint works for any cursor group, not just the push dispatcher:

```
GET /streams/cursors?topic=chat.group.eng-team&partition=3&group=user:alice
```

```json
{
  "members":   { "phone": 1042, "laptop": 900 },
  "watermark": 900,
  "next_seq":  1100,
  "lag":       200
}
```

`lag` here is unread count for the *slowest* device. Per-device unread is
`next_seq - members[device]`.

If you build your own consumers (search indexer, moderation scanner), give each
its own group and monitor its lag the same way.

## Logs

Filter by prefix:

| Prefix | Source |
|---|---|
| `[messaging]` | Startup, shutdown, subsystem wiring |
| `[gateway]` | Connection lifecycle, read errors |
| `[outbox]` | Push attempts, failures, drops |
| `[stream]` | Cursor compaction problems |

Two lines to alert on specifically:

```
[outbox] dropping N notifications after M attempts
[gateway] partial fan-out for <message-id>: <error>
```

The first means notifications were abandoned. The second means a message is
durable in its conversation log but missing from some inboxes — the message is
not lost, but a recipient's cross-conversation index is incomplete.

## Dashboard

A useful single-screen layout:

```
┌── Traffic ──────────────┬── Health ───────────────┐
│ frames in/out per sec   │ attached sessions       │
│ records out per sec     │ auth failures per sec   │
│ messages sent per sec   │ forbidden per sec       │
├── Delivery ─────────────┼── Storage ──────────────┤
│ push lag (p50, p99)     │ disk used / total       │
│ push dropped per hour   │ growth per day          │
│ slow client drops       │ partition skew          │
└─────────────────────────┴─────────────────────────┘
```

Push lag and disk growth are the two that tell you about tomorrow's problem
rather than today's.

## What is not instrumented

Being explicit, so you do not go looking:

- **No per-message latency histogram.** There is no send-to-delivery timing.
- **No Prometheus metrics for the messaging subsystem.** `/metrics` covers the
  queue broker only; messaging stats are JSON on the admin endpoints. Scraping
  them means a small exporter.
- **No per-conversation metrics.** You can query a topic, but nothing tracks
  hot conversations over time.
- **No distributed tracing.**

## Further reading

- [Production checklist](production-checklist.md)
- [Capacity planning](capacity.md)
- [Global HA](global-ha.md)
