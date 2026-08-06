# Capacity planning

Where the limits actually are, and how to size for them.

All figures below are order-of-magnitude guidance from the design, not measured
benchmarks on your hardware. Run `go test -bench` in `internal/stream`,
`internal/identity` and `internal/presence` against your own machine before
committing to numbers.

## Storage

### Per message

```
frame header          8 bytes
record header        27 bytes
key (conv ID)     ~  20 bytes
headers           ~ 120 bytes   sender, message_id, kind, conversation, client_msg_id
payload              variable
─────────────────────────────
overhead          ~ 175 bytes + payload
```

A short text message (~100 bytes of payload, or ~150 as ciphertext) costs
roughly **300 bytes** in the conversation log.

### Inbox pointers

Each recipient in a fan-out-on-write conversation adds a pointer record with no
payload:

```
overhead + headers  ~ 180 bytes per recipient
```

So a message to a 3-person group costs `300 + 3×180 ≈ 840` bytes total. A
message to a 500-person channel (above the fan-out limit) costs just the 300.

### Working the numbers

```
1M messages/day, average 2.5 recipients, 100-byte payloads

conversation log:  1M × 300 B          =  300 MB/day
inbox pointers:    1M × 2.5 × 180 B    =  450 MB/day
────────────────────────────────────────────────────
                                       ~ 750 MB/day
                                       ~  22 GB/month
                                       ~ 270 GB/year
```

**Retention defaults to unlimited.** That 270GB/year is a decision you are
making by not making one. For a dating app where conversations go cold after a
match fades, a year of retention is often generous:

```json
{ "messaging": { "stream": { "retention_age": "8760h" } } }
```

Retention removes whole segments, so actual usage sits somewhat above the cap —
budget one extra `segment_bytes` per partition.

### Segment and file-handle count

```
files = topics × partitions × segments × 2   (.log + .index)
```

An inbox topic per user at 1 partition each is one topic per user. 100,000
users with a few segments each is a few hundred thousand file handles. Raise
`ulimit -n` accordingly — this is the limit people hit first and diagnose last.

Larger `segment_bytes` reduces the count at the cost of coarser retention.

## Throughput

### Writes

A partition's append rate is bounded by one lock plus the write path:

| Setting | Approximate ceiling per partition |
|---|---|
| `sync_on_append: false` | high — buffered write, memcpy-bound |
| `sync_on_append: true` | ~1/fsync-latency: roughly 5–10k/s on NVMe, ~1k/s on slower storage |

**This is the single most important capacity trade in the system.** Turning on
fsync for durability costs you two to three orders of magnitude of per-partition
write throughput. It is usually still plenty — 5,000 messages/second in one
conversation is not a thing that happens — but it caps a *busy* partition.

Scale by adding partitions, since each has its own lock. A topic with 32
partitions has 32 independent write paths. Within one conversation you get one
order and one lock; that is the price of ordering.

### Reads

Reads are cheap and mostly sequential. A tailing subscriber reads from the page
cache. History reads seek via the sparse index, costing at most
`index_interval` bytes (4KB by default) of forward scanning.

The read amplification to watch is **fan-out on read**: 50,000 subscribers on
one partition means 50,000 goroutines waking on each append. They read the same
page-cached bytes, so the cost is scheduling and socket writes, not I/O — but it
is real. See [Fan-out](../architecture/fanout.md).

## Connections

Each WebSocket connection costs:

- 2 goroutines (read loop, write drain)
- 1 goroutine per active subscription
- ~8KB of read/write buffers
- `send_buffer` × frame size of outbound queue capacity

```
100,000 connections × 3 avg subscriptions
  ≈ 500,000 goroutines
  ≈ 2 GB of buffers
```

Go handles this, but tune:

```bash
ulimit -n 1000000
sysctl -w net.core.somaxconn=65535
sysctl -w net.ipv4.tcp_max_syn_backlog=65535
```

And cap subscriptions per session (`max_subscriptions`, default 200) so one
client cannot spawn unbounded goroutines.

## Memory

| Component | Cost |
|---|---|
| Presence registry | ~200 B per session, sharded 64 ways |
| Dedup table | ~150 B per claim × claim rate × TTL |
| Gateway sessions | ~500 B per session including subscriptions |
| Cursor store | ~80 B per (topic, partition, group, member) |
| Sparse indexes | 16 B per `index_interval` bytes of log |

Worked example:

```
100,000 users × 2 devices          = 200,000 sessions   ≈  40 MB presence
1,000 msg/s × 600 s dedup TTL      = 600,000 claims     ≈  90 MB dedup
200,000 sessions                                        ≈ 100 MB gateway
200,000 devices × 20 conversations = 4M cursors         ≈ 320 MB cursors
100 GB of log ÷ 4 KB × 16 B                             ≈ 400 MB indexes
────────────────────────────────────────────────────────────────────────
                                                        ≈ 950 MB
```

The cursor store is the one that surprises people: it scales with
devices × conversations, not with users. Raising `index_interval` trades seek
time for index memory if the last line dominates.

## Ephemeral signals

Typing indicators run at roughly 10× the message rate and cost **no disk and no
replication**. The rate limiter (default 5/s per user, burst 20) bounds the
worst case.

The cost is memory for limiter buckets — one per active publisher, garbage
collected after `limiter_idle_ttl` (5 minutes).

## Push dispatch

One goroutine per watched inbox partition. With one partition per user inbox
that is **one goroutine per user with an inbox**, which at 100,000 users is
100,000 mostly-sleeping goroutines — acceptable but not free.

If that becomes a problem, the fix is fewer inbox topics (shard users into
shared inbox topics keyed by user) rather than fewer goroutines. That is not
implemented.

Watch `lag` on the dispatcher cursor; growing lag means the webhook is too slow
or failing.

## Sizing worked example

A dating app, 100k daily active users, 2 devices each, 20 conversations each,
1M messages/day:

| Resource | Estimate |
|---|---|
| Disk | ~750 MB/day, ~270 GB/year unbounded |
| Memory | ~1 GB steady state |
| Connections | ~200k peak |
| Write rate | ~12/s average, budget 10× for peak |
| Partitions | 32 conversation, 1 per inbox |
| File handles | ~300k — raise `ulimit -n` |

The write rate is the striking number: 1M messages/day is **12 per second on
average**. Even with `sync_on_append: true` and a modest peak multiplier, this
is nowhere near any throughput limit. For an app at this scale the binding
constraints are connection count and disk growth, not message throughput — so
turn on fsync and stop worrying about it.

## When to shard

One node is enough until one of these is true:

- Disk growth outpaces what one volume can hold.
- Connection count exceeds what one process can hold (~500k is a reasonable
  practical ceiling).
- A single partition's write rate approaches the fsync ceiling — meaning one
  conversation is taking thousands of messages per second, which for chat means
  something is wrong.

Sharding is [described here](global-ha.md#sharded-by-conversation-with-a-routing-layer)
and is entirely your responsibility to build: BoltQ has no routing layer.

## Further reading

- [Global HA](global-ha.md) — what scaling out requires.
- [Durability](../architecture/durability.md) — the fsync trade.
- [Fan-out](../architecture/fanout.md) — write amplification.
