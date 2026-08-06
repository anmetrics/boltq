# The queue plane

Work-queue semantics — competing consumers, per-message acknowledgement,
redelivery, dead-lettering — built on the stream log rather than beside it.

## Why not beside it

The queue plane and the messaging plane need different *delivery* semantics.
They do not need different *storage*. Running two storage engines side by side
means two replication implementations, two durability models, two leader
election stories and two recovery paths for the same problem — and every
distributed bug has to be fixed twice.

No production message broker does this. Kafka has one log and builds queue
semantics on top of it. RabbitMQ has one broker and one metadata store; classic,
quorum and stream queues differ in their backend but share exchanges, bindings,
consumers and permissions. Pulsar separates compute from storage, which is a
different axis, and still has exactly one storage engine.

So the split here is at delivery semantics, and only there:

| | stream plane | queue plane |
|---|---|---|
| Position | consumer-held cursor, rewindable | broker-held per-record state |
| Readers | independent, all see everything | competing, each record goes to one |
| Retirement | retention only | acknowledgement |
| **Storage** | `internal/stream` | `internal/stream` |
| **Replication** | `internal/replication` | `internal/replication` |
| **Recovery** | segment replay + epoch reconcile | segment replay + epoch reconcile |

Messages never pass through Raft. `internal/cluster` stays where it belongs:
topology metadata — exchanges, bindings, queue declarations — which changes a
few times a day, not tens of thousands of times a second.

## The share partition

The one thing the log does not provide is per-record delivery state, so that is
the one thing this package adds. It lives in `internal/queuelog/share.go`.

Each `(queue, consumer group, partition)` triple owns a bounded window:

```
     retired          the window            never read
  ┌────────────┬─────────────────────────┬──────────────
  │ ...........│ A  ✓  N  A  ✓  .  .  .  │ 42 43 44 ...
  └────────────┴─────────────────────────┴──────────────
               ^base                     ^next
```

- Below `base`: acknowledged or dead-lettered. This is the only number
  persisted, via the existing `CursorStore`.
- `[base, next)`: the window. Each sequence is *available* (deliverable) or
  *acquired* (leased to a consumer, with a deadline), plus a set of retired
  sequences waiting for `base` to catch up.
- At or above `next`: not read yet. The log's problem, not ours.

`MaxInFlight` bounds the window, so queue semantics cost a constant amount of
memory per partition rather than growing with backlog depth.

### Why only the base is persisted

A cursor is a single number, so `base` can only advance past a *contiguous* run
of retired sequences. Committing past a record still in flight would mean a
crash loses it: the window would restart above it and nothing would ever read it
again.

The consequence is at-least-once: after a crash, records acknowledged out of
order above the base are delivered a second time. Persisting the whole window
would cost a cursor write per acknowledgement instead of per window advance, and
would buy exactly-once only for the crash case — which nothing else in this
system offers either.

### Redelivery prefers the oldest record

`acquire` drains released sequences before reading new ones, oldest first.
Handing out fresh records first would let one repeatedly-nacked message sit at
the base while the window filled behind it, and the queue would stall with
`MaxInFlight` records in memory and no way to advance.

### Leases, not ownership

A consumer holds a record for `AckTimeout`. Past that the lease expires and
peers may take it — which is what makes a crashed consumer harmless. A
background sweeper reclaims expired leases even when nothing is polling, and
`ReleaseConsumer` returns them immediately on a clean disconnect, since a closed
socket is proof the lease will never be acknowledged.

Acknowledgement is checked against the holder. A consumer that wakes up after
its lease expired cannot retire a record someone else is now working on.

## The router

`internal/queuelog/router.go` is the exchange layer. It reuses
`broker.Exchange`'s matching rules — direct, fanout, topic with `*`/`#`, headers
— rather than reimplementing them, because a second implementation is only a
second place for them to drift.

What it adds over the in-memory broker:

- **Unroutable messages are reported.** A publish that matches no binding returns
  `ErrUnroutable` instead of being dropped in silence.
- **Dead-lettering goes through an exchange.** `DeadLetterExchange` /
  `DeadLetterRoutingKey` on a `QueueSpec`, not a hardcoded `<queue>_dead_letter`.
  Several queues can share one dead-letter queue, or each can have its own, and
  the broker does not need to know which. Death metadata (`x-death-reason`,
  `x-death-queue`, `x-death-count`) rides along on the record.
- **A failing dead-letter route does not lose the message.** The record goes back
  to available and is retried. The one exception is a dead-letter exchange with
  no matching binding, which is an explicit "discard" — retrying it forever would
  pin the window base.
- **No dead-letter exchange means discard on exhaustion**, matching RabbitMQ. Not
  the safe default, which is why declaring one is a single field.

Each matching queue gets its own append. A record cannot be shared across queues
the way an in-memory broker shares a pointer — but in return, a dead consumer on
one queue cannot affect any other, and every queue's history is independently
replayable.

## What this does not do yet

- **Channels and per-consumer QoS.** `prefetch` is a per-call argument, not a
  registered per-consumer credit limit.
- **Virtual hosts** and per-vhost permissions.
- **Alternate exchanges** and exchange-to-exchange bindings.
- **Queue arguments**: `x-max-length`, `x-overflow`, per-queue message TTL,
  `x-expires`, priority.
- **Asynchronous publisher confirms** with sequence numbers and a multiple flag.
- **AMQP 0-9-1 wire protocol.** Existing RabbitMQ clients cannot connect.
- **Migration of `internal/broker`.** The in-memory broker and its Raft FSM are
  untouched; this package is additive. Cutting the TCP/HTTP API over to it is a
  separate change.
