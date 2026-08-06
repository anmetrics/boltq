# Replication

Until this existed, a message lived on exactly one node's disk: losing that
disk lost the messages. Replication closes that gap for the write path.

## What it gives you

A leader ships every record to its followers and — when configured to — waits
for them before telling the publisher the message was sent. An acknowledged
message then survives the loss of one machine.

```
                    publisher
                        │  send
                        ▼
        ┌───────────────────────────────┐
        │  LEADER                       │
        │  1. assign seq under the lock │
        │  2. append locally            │
        │  3. ship to followers    ─────┼──────▶ follower A
        │  4. wait for min_in_sync ◀────┼────── ack
        │  5. acknowledge publisher     │
        └───────────────────────────────┘
```

Only step 1 and 2 hold the partition lock. Waiting for replicas happens after
the lock is released, so replication latency delays one publisher's
acknowledgement — never other writers to the same partition.

## Durability levels

`min_in_sync` counts the leader.

| Value | Meaning | Survives |
|---|---|---|
| `1` (default) | Leader only; replication is asynchronous | nothing extra |
| `2` | Leader + one follower | losing one node |
| `3` | Leader + two followers | losing two nodes |

Only the **conversation record** waits for quorum. Inbox pointers are a derived
index — if one is lost the message still exists and is still readable from the
conversation log — so waiting on replication for each of N recipients would
multiply send latency by the group size to protect data that can be rebuilt.

Setting it above the number of replicas you actually have makes **every write
fail**. That is deliberate: refusing writes is better than accepting them at a
durability the operator did not ask for. The failure is fast — the leader
checks how many followers are connected before waiting, so a cluster that is
short a node fails immediately instead of blocking every publisher for the full
`ack_timeout`.

## Sequence assignment

The leader assigns sequences. Followers do not.

```go
// Leader
seq := p.nextSeq          // under the partition lock
rec.Seq = seq

// Follower
p.AppendReplicated(rec)   // seq is an INPUT, validated against the local head
```

`AppendReplicated` handles three cases, all normal:

| Relationship | Action |
|---|---|
| `seq == nextSeq` | append |
| `seq < nextSeq` | already applied — skip (idempotent replay after a reconnect) |
| `seq > nextSeq` | **refuse** with `ErrReplicationGap` |

Refusing a gap matters. Writing the record anyway would leave a hole in a log
whose whole contract is that sequences are gap-free. Instead the follower
re-fetches from its true head, and the leader replaces the stream.

## Catch-up and resume

A follower fetches from wherever its local log actually ends:

```
follower restarts, local log ends at 4096
  → FETCH { topic, partition, from_seq: 4096 }
  → leader streams 4096 onwards
```

The same mechanism serves both a brand-new follower (from 1) and a reconnect
mid-stream. There is no separate backfill path to get wrong.

If retention has removed the requested range, the leader reports it and resumes
at the oldest surviving record. The follower counts a gap rather than silently
believing it has complete history.

## Topics that do not exist yet

Stream topics are created lazily on the first message, so a follower is often
assigned a partition for a conversation nobody has spoken in. The leader waits
for the topic rather than erroring — an early version gave up permanently here,
which left the partition unreplicated forever.

## Configuration

**Leader:**

```json
{
  "messaging": {
    "stream": { "enabled": true },
    "replication": {
      "enabled": true,
      "role": "leader",
      "listen": "10.0.0.1:9200",
      "secret_env": "BOLTQ_REPLICATION_SECRET",
      "min_in_sync": 2,
      "ack_timeout": "5s"
    }
  }
}
```

**Follower:**

```json
{
  "messaging": {
    "stream": { "enabled": true },
    "replication": {
      "enabled": true,
      "role": "follower",
      "leader_addr": "10.0.0.1:9200",
      "secret_env": "BOLTQ_REPLICATION_SECRET",
      "topics": ["chat.direct.alice:bob:16", "chat.inbox.bob:1"],
      "sync_on_apply": true
    }
  }
}
```

Each topic entry is `topic:partitionCount`. The count is required so the
follower creates the topic with the leader's partitioning — guessing would
remap every key to a different partition.

`sync_on_apply` is what makes a follower's acknowledgement mean "on disk"
rather than "in page cache". Without it, simultaneous power loss to the whole
cluster can still lose recent records everywhere.

## Security

The replication listener is an **internal plane**. A connection to it can read
every message in the log, so:

- Never expose it publicly.
- Set a secret, from an environment variable. The comparison is constant-time.
- Put it on a private network or a mesh with mutual TLS.

An unauthenticated peer is rejected during the handshake, before it learns even
a topic name.

## Leader epochs

Every record carries the **leader epoch** it was written under — the term of the
node that accepted it. A node opens a new term when it is promoted, and the
sequence at which the term began is checkpointed beside the partition's segments
in `leader-epoch-checkpoint`.

This exists to answer a question sequence numbers cannot. Suppose a leader
accepts records 98–100 and dies before replicating them. A follower that holds
them reconnects to the new leader, which never had those records and has since
assigned 98–100 to different ones. Comparing sequence numbers, both nodes are
"at 100" and agree. They hold different data.

So before fetching anything, a reconnecting follower sends its last epoch and
asks where that epoch ended. Three answers are possible:

| Leader's answer | Meaning | Follower does |
|---|---|---|
| "still open, up to my end" | same term, no failover | keeps everything |
| "it ended at sequence N" | a newer term began at N | truncates to N, then fetches |
| "I cannot place that epoch" | histories share no common point | **stops replicating that partition** |

The third case is deliberately not automated. Neither keeping the records
(divergence) nor discarding them (data loss on a guess) is safe, so it surfaces
as an `epoch_conflicts` counter and an error rather than a silent choice. Alert
on it.

A truncation discards records that were *accepted but never acknowledged as
durable* — no publisher was ever told they were committed, so nothing a client
believes it has is lost. `truncations` and `records_truncated` in the follower
stats count them: nonzero after a failover is normal, nonzero at any other time
means investigate.

Logs written before epochs existed carry none, and reconcile as a no-op — an
upgrade needs no migration step.

## What is NOT implemented

Stated plainly, because the gaps matter more than the feature list.

**No automatic failover.** Leadership is configuration. If the leader dies, the
followers keep their data and stop advancing; promoting one is a deliberate
operator action.

Leader epochs make the *consequences* of a bad promotion detectable and
repairable, which is the prerequisite for automating it — but they do not decide
who leads. Electing a leader without consensus can still produce two nodes both
assigning sequences to the same partition; epochs mean a follower will notice
and truncate rather than silently fork, not that the split-brain never happened.
Doing it properly needs a metadata layer that agrees on leadership under network
partition. Until that exists, manual promotion with a written procedure is
honest; automatic promotion without consensus is not.

**No follower reads.** A follower holds the data but the gateway does not route
reads to it. Its copy is for durability and manual promotion, not for scaling
reads.

**No automatic topic discovery.** A follower replicates the topics it is
configured with. New conversations create new topics, so a wildcard assignment
would be genuinely useful — it does not exist. In practice this means a
conversation created after the follower was configured is **not replicated**
until someone adds it and restarts the follower.

**Cursors are not replicated.** Reader positions live in the leader's local
`_cursors` directory and are not shipped to followers. Promoting a follower
therefore recovers the messages but not the read positions: every device
re-reads from wherever the promoted node's cursor store happens to be, and the
push dispatcher restarts at the head. Messages are not lost; unread state is.
Back up `_cursors` alongside the stream directory, or accept the resync.

**No cross-region tuning.** The protocol works over any TCP link, but there is
no batching or compression tuned for a high-latency path, and synchronous
`min_in_sync: 2` across regions adds the round trip to every send.

## Manual failover procedure

1. Confirm the leader is genuinely down, not partitioned. Promoting during a
   partition creates two leaders.
2. Pick the follower with the highest `next_seq` for the affected partitions —
   `GET /streams/topic?name=<topic>` on each.
3. Stop the follower.
4. Change its config: `role: "leader"`, set `listen`, remove `topics`.
5. Start it, and repoint remaining followers and clients at the new address.
6. **Do not restart the old leader with its old config.** It would accept
   writes for the same partitions and diverge. Wipe its stream directory and
   bring it back as a follower.

Step 6 is the one that causes data loss when skipped.

## Monitoring

```
GET /messaging/overview
```

Watch:

- **Follower lag** — how far behind each replica is.
- **`ack_timeouts`** — appends that could not reach the configured durability.
  Non-zero means publishers are seeing errors.
- **`followers_connected`** — dropping below `min_in_sync - 1` means writes are
  failing.
- **`auth_failures`** — someone is trying to connect to the replication plane.

## Further reading

- [Durability](durability.md) — what survives what, now including replication.
- [Global HA](../operations/global-ha.md) — the wider multi-region picture.
- [The stream engine](stream-engine.md).
