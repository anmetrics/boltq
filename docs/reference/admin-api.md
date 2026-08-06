# Admin API

HTTP endpoints for inspecting the messaging subsystem. All are served on the
admin port (default 9090) and require the admin API key when one is configured.

**Do not expose the admin port publicly.** It reveals topic names, user IDs and
connection topology.

## Existing endpoints

These predate the messaging subsystem and are unchanged: `/overview`, `/stats`,
`/metrics`, `/health`, `/queues/purge`, `/dead-letters/purge`, `/publish`,
`/consume`, `/ack`, `/exchange/*`, `/cluster/*`. See the
[main README](../../README.md).

## Messaging endpoints

Registered only when `messaging.stream.enabled` is true.

---

### `GET /messaging/overview`

Everything in one call. Use this for a dashboard.

```json
{
  "streams": {
    "topics": [ { "name": "chat.direct.alice:bob", "partitions": [ … ],
                  "total_bytes": 27063000 } ],
    "cursors": { "tracked": 4102934 }
  },
  "presence": { "users": 84102, "sessions": 91883, … },
  "gateway":  { "connections": 1284392, "attached": 82911, … },
  "signals":  { "published": 8821034, "delivered": 17204188,
                "dropped": 2201, "rate_limited": 8821, … },
  "push":     { "scanned": 90211, "suppressed": 62104, "sent": 28100,
                "failed": 7, "dropped": 0, "watching": 84102 }
}
```

Any section is `null` if that component is not configured.

This walks every topic and partition, so it is O(topics × partitions). Poll it
every 15–30 seconds, not every second.

---

### `GET /streams`

Per-topic log statistics.

```json
{
  "topics": [
    {
      "name": "chat.group.eng-team",
      "partitions": [
        { "id": 0, "first_seq": 1, "next_seq": 90211,
          "records": 90210, "bytes": 27063000 }
      ],
      "total_bytes": 27063000
    }
  ],
  "cursors": { "tracked": 4102934 }
}
```

`first_seq > 1` means retention has removed history from that partition.

---

### `GET /streams/topic?name=<topic>`

Per-partition detail for one topic. `404` if the topic does not exist.

```json
{
  "name": "chat.group.eng-team",
  "partitions": [
    { "id": 0, "first_seq": 1,    "next_seq": 45102, "records": 45101, "bytes": 13530300 },
    { "id": 1, "first_seq": 1,    "next_seq": 44109, "records": 44108, "bytes": 13232700 },
    { "id": 2, "first_seq": 8801, "next_seq": 91002, "records": 82201, "bytes": 24660300 }
  ],
  "total_bytes": 51423300
}
```

Watch for **partition skew** — one partition far larger than the others means
either the partition count is too low or one conversation is dominating.

Partition 2 above has `first_seq: 8801`: retention has removed its early
history. Clients whose cursors are below that will get a `gap` frame.

---

### `GET /streams/cursors?topic=<topic>&partition=<n>&group=<group>`

Cursor positions and lag. `partition` defaults to 0. `group` defaults to
`push-dispatcher`, which is usually the one you want: it answers "are
notifications keeping up?", the question behind most delivery complaints.

```json
{
  "topic": "chat.inbox.bob",
  "partition": 0,
  "group": "push-dispatcher",
  "members": { "": 1097 },
  "watermark": 1097,
  "next_seq": 1100,
  "first_seq": 1,
  "lag": 3
}
```

For a user group, `members` shows per-device positions:

```
GET /streams/cursors?topic=chat.group.eng-team&partition=3&group=user:alice
```

```json
{
  "members": { "phone": 1042, "laptop": 900, "tablet": 1100 },
  "watermark": 900,
  "next_seq": 1100,
  "lag": 200
}
```

`watermark` is the slowest member. `lag` is `next_seq - watermark` — unread
count for the device furthest behind. Per-device unread is
`next_seq - members[device]`.

---

### `GET /presence`

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

`sessions / users` is the average device count. Drift upward over time usually
means unstable device IDs, which also means cursors are accumulating.

`by_node` reflects only this node's registry — presence does not gossip.

This walks every shard and is O(sessions). Do not poll it aggressively.

---

### `GET /gateway/stats`

```json
{
  "connections": 1284392,
  "resumed": 84021,
  "auth_failures": 1204,
  "forbidden": 88,
  "frames_in": 98234012,
  "frames_out": 201938441,
  "records_out": 88203441,
  "slow_client_drops": 12,
  "sessions": 84102,
  "attached": 82911
}
```

| Field | Meaning |
|---|---|
| `connections` | Cumulative accepted, since process start |
| `resumed` | Successful session resumes |
| `auth_failures` | Rejected before the WebSocket upgrade |
| `forbidden` | ACL denials. Should be near zero |
| `slow_client_drops` | Clients disconnected for not reading |
| `sessions` | Tracked, attached plus within the resume window |
| `attached` | Currently holding a socket |

`resumed / connections` is the resume hit rate — low means clients are not
storing resume tokens, and every reconnect pays for a full re-subscribe.

The gap between `sessions` and `attached` is sessions in their resume window. A
large gap means lots of reconnects.

---

## Mounting the gateway on this port

If `messaging.gateway.port` is 0, the WebSocket gateway is mounted on this
server at `messaging.gateway.path` (default `/ws`).

**Do not do this in production.** End-user traffic and operator traffic have
different exposure, different rate limits and different blast radii. Give the
gateway its own port.

## What is not exposed

- **No mutation endpoints.** You cannot create or delete topics, evict
  sessions, force retention, or reset cursors over HTTP. Topics are created
  implicitly on first use.
- **No message content.** You cannot read message payloads through the admin
  API. This is deliberate: an admin endpoint that dumps user messages is a
  liability, and for an encrypted app it would return ciphertext anyway.
- **No Prometheus format.** `/metrics` covers the queue broker only. Messaging
  stats are JSON; scraping them needs a small exporter.

## Example: a monitoring script

```bash
#!/bin/bash
KEY="${BOLTQ_API_KEY}"
BASE="http://localhost:9090"

overview=$(curl -sf -H "X-API-Key: $KEY" "$BASE/messaging/overview")

dropped=$(jq -r '.push.dropped // 0' <<<"$overview")
[ "$dropped" -gt 0 ] && echo "ALERT: $dropped push notifications abandoned"

bytes=$(jq -r '[.streams.topics[].total_bytes] | add // 0' <<<"$overview")
echo "stream bytes: $bytes"

drops=$(jq -r '.gateway.slow_client_drops // 0' <<<"$overview")
[ "$drops" -gt 100 ] && echo "WARN: $drops slow-client disconnects"
```

## Further reading

- [Monitoring](../operations/monitoring.md) — what to alert on.
- [Capacity planning](../operations/capacity.md).
- [Configuration](configuration.md).
