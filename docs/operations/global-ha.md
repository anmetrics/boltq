# Global HA

This document is deliberately blunt about what BoltQ's messaging subsystem
does and does not provide for multi-region deployment, because getting this
wrong is expensive and the failure mode is silent.

## What exists today

| Component | Replicated? | How |
|---|---|---|
| Queue broker (jobs, pub/sub) | **Yes** | Raft, quorum, automatic leader election |
| KV cache | Yes | Raft |
| Stream log (chat messages) | **Yes** | Leader/follower with quorum acks — see [replication](../architecture/replication.md) |
| Cursors | **No** | Local disk only |
| Presence registry | **No** | In memory, per node |
| Gateway sessions | **No** | In memory, per node |
| Dedup table | **No** | In memory, per node |

The queue side of BoltQ is clustered via Raft. The messaging side now replicates
its log, but **leadership is static and failover is manual**.

## What that means concretely

With replication enabled and `min_in_sync: 2`, an acknowledged message exists on
two nodes and survives losing one. What still does not happen automatically:

- **A leader failure does not promote a follower.** Followers keep the data and
  stop advancing; an operator promotes one, following the procedure in
  [Replication](../architecture/replication.md#manual-failover-procedure).
- **A client connected to node B cannot read a conversation led by node A.**
  There is no cross-node read path; a follower replicates for durability, not to
  serve reads.
- **Presence, cursors and gateway sessions are still per node.** A sharded
  deployment needs an external presence store.

With replication **disabled** (the default), none of the above applies and a
node's messages exist only on its own disk.

## What you can build today

### Single region, one leader plus one follower

The honest recommendation for launch.

```
                    ┌─────────────┐        replication
   clients ────────▶│  BoltQ      │  :9200 ──────────▶ ┌─────────────┐
                    │  LEADER     │◀───── acks ─────── │  FOLLOWER   │
                    │  gateway    │                    │  stream only│
                    │  + stream   │                    └──────┬──────┘
                    └──────┬──────┘                           │
                           ▼                                  ▼
                    replicated volume                  replicated volume
                    + snapshots                        + snapshots
```

- `min_in_sync: 2` means an acknowledged message is on both nodes before the
  publisher hears about it.
- Losing the leader loses no acknowledged data; it costs a manual promotion.
- Snapshots are crash-consistent, which is safe: recovery truncates torn tails.
- `sync_on_apply: true` on the follower makes its ack mean "on disk".

If you would rather run a single node, `sync_on_append: true` plus a replicated
volume and snapshots is defensible — recovery is then "attach the volume to a
new instance", costing minutes of downtime rather than data.

This carries a real dating app a long way. A single node handles hundreds of
thousands of concurrent WebSocket connections and tens of thousands of messages
per second on ordinary hardware. Do not add distributed-systems complexity
before you have measured that you need it.

### Sharded by conversation, with a routing layer

When one node is not enough.

```
                   ┌──────────────┐
   clients ───────▶│   router     │  hash(conversation_id) → shard
                   └───┬───┬───┬──┘
                       │   │   │
              ┌────────▼┐ ┌▼──┐ ┌▼────────┐
              │ BoltQ 0 │ │ 1 │ │ BoltQ 2 │   each with its own volume
              └─────────┘ └───┘ └─────────┘
```

The router must be **consistent**: the same conversation always reaches the
same node, or ordering breaks. Each shard is an independent BoltQ with its own
storage and its own failure domain.

What you must build yourself:

- The routing layer, and a shard map that survives router restarts.
- Cross-shard presence, if a user's conversations span shards. The presence
  registry is per node, so "is bob online" is only answerable by the node
  holding bob's socket.
- Resharding. There is no rebalancing; changing the shard count remaps keys and
  breaks ordering for conversations that move.

Pin a user's socket to the shard holding most of their conversations, or accept
a fan-in where the gateway a user connects to is not the node storing their
messages — in which case you need an internal read path between nodes, which
does not exist either.

### Multi-region, partitioned by user home region

The realistic multi-region shape given the constraints.

```
   EU users ──▶ eu-west-1 BoltQ ──▶ eu volume
   US users ──▶ us-east-1 BoltQ ──▶ us volume

   cross-region conversations: application-level bridge
```

Each region is independent. A user has a home region; their conversations live
there. A conversation between a EU user and a US user has to live in one of
them — pick by conversation ID hash, or by the initiator — and the far user
accepts cross-ocean latency.

`presence.region` is stamped on sessions and exposed in
`Registry.Routes()` as a hint for locality-aware routing, but BoltQ does not
route across regions itself. That is yours to build.

## What BoltQ does not do — and what it would take

### Automatic leader election and failover

**Not implemented.** Replication itself exists; choosing a leader does not.
Building it means, roughly:

1. A metadata layer that agrees on leadership under network partition. Without
   consensus, two nodes can both believe they lead a partition and assign
   conflicting sequences to the same log — the one failure a replicated log
   must never have.
2. Leader election per partition, not per node — otherwise one node's failure
   stalls every partition it happened to lead.
3. Fencing, so a recovering old leader cannot accept writes for a partition
   that moved on without it.
4. A follower read path with a defined staleness bound.

The existing Raft integration is not a shortcut. It replicates the queue
broker's whole state machine through a single Raft group, which is the wrong
shape: a chat log needs per-partition groups so throughput scales with
partitions rather than being serialised through one consensus stream.

This is a substantial project — realistically the largest single piece of work
remaining — not a configuration change.

### Cross-region replication

Even with per-partition replication, synchronous cross-region quorum adds
~150ms to every send. The standard answer is asynchronous replication with a
region-local leader, which introduces conflict resolution the current design has
no place for. Not implemented and not straightforward.

### Tiered storage

Segments live on local disk until retention removes them. There is no path to
object storage. A year of chat history for a large user base is measured in
terabytes; that either lives on expensive block storage or gets deleted.

The segment format is well-suited to tiering — sealed segments are immutable and
self-describing — so this is the most tractable of the gaps. Nothing implements
it.

### Distributed presence

The registry is per node. `EvictNode()` exists so a cluster layer *can* clear a
departed node's sessions, but nothing calls it automatically because there is no
cross-node membership signal wired to it.

For a sharded deployment, put presence in Redis or a similar shared store and
have the gateway write through to it.

### Cross-node message routing

If a user's socket is on node B and their conversation is on node A, there is no
internal path. Avoid the situation by pinning, or build the path.

## Recommended path

**Stage 1 — launch.** One leader plus one follower with `min_in_sync: 2`, on
replicated volumes, with snapshots. An acknowledged message then survives losing
a node, and a leader failure costs a manual promotion rather than data.

**Stage 2 — one node is not enough.** Shard by conversation with a consistent
router. Shared presence store. Accept that a shard outage means those
conversations are unavailable.

**Stage 3 — geography matters.** Region-local deployments, users pinned to a
home region, application-level bridging for cross-region conversations.

**Stage 4 — genuine HA.** Per-partition replication. This is where you decide
whether to build it, contribute it, or move the chat storage to something that
already has it and keep BoltQ as the edge.

That last option is legitimate and worth naming: BoltQ's gateway, ACL, presence,
fan-out and push dispatch are useful independently of its storage. Nothing stops
you from keeping them and swapping the log underneath.

## Monitoring for these limits

Alert on the things that are single points of failure:

- Disk usage on the stream volume — retention defaults to unlimited.
- `lag` on the push dispatcher cursor.
- Node liveness, since there is no automatic failover for the messaging plane.
- Snapshot age and restore-test recency. An untested backup is not a backup,
  and here it is your only durability story for node loss.

See [Monitoring](monitoring.md).

## Further reading

- [Durability](../architecture/durability.md) — what survives what.
- [Capacity planning](capacity.md) — where the single-node limits are.
- [Production checklist](production-checklist.md).
