# Why a log

BoltQ started as a work queue. A work queue's core operation is destructive: a
consumer pops a message and it is gone. That is exactly right for jobs — you
want a resized image processed once, not once per worker — and exactly wrong
for chat.

This document explains why, and what changed.

## The mismatch

Consider what a chat client actually asks for.

**"Show me the last 50 messages."** A queue cannot answer this. The messages
were consumed and deleted when they were delivered the first time. The history
a chat app displays *is* the product; deleting it on delivery destroys the
thing users came for.

**"I was offline for two hours. What did I miss?"** A queue can hold messages
for an offline consumer, but only by keeping them queued — and it has no way to
express "deliver these again to this device, but they were already delivered to
that other device". One user with a phone and a laptop breaks the model.

**"I dropped my connection at message 1042. Resume from there."** A queue has
no addressable position. There is no "message 1042" to resume from; there is
only the head of the queue.

**"These messages must appear in the order they were sent."** A work queue with
competing consumers deliberately has no such guarantee — parallelism is the
point. In a conversation, order is not negotiable: a reply appearing before the
message it answers is a bug users notice instantly.

Each of these is a symptom of the same thing. A queue models *work to be done*.
Chat needs a model of *what happened, in order, kept*.

## What replaced it

The messaging subsystem is built on a **partitioned append-only log**:

```
chat.group.eng-team, partition 3

  seq:      1        2        3        4        5        6   ← next: 7
         ┌────────┬────────┬────────┬────────┬────────┬────────┐
         │ alice  │ bob    │ alice  │ carol  │ alice  │ bob    │
         │ "hi"   │ "hey"  │ "..."  │ "!"    │ "ok"   │ "sure" │
         └────────┴────────┴────────┴────────┴────────┴────────┘
              ▲                          ▲             ▲
              │                          │             │
         alice's laptop           bob's phone     carol's phone
         (behind, catching up)    (caught up)     (caught up)
```

Reading does not consume. Each reader holds a *cursor* — a sequence number —
and that is the only per-reader state. Everything else follows:

- **History** is a read from a lower sequence. No special case.
- **Catch-up after being offline** is a read from your committed cursor. Also
  no special case — it is the same operation as reading history.
- **Resume after a dropped connection** is a read from wherever you got to.
- **Multi-device** is several cursors over one log. A phone at 1042 and a
  laptop at 900 are just two numbers.
- **Ordering** is structural: sequence numbers are assigned under a lock, one
  per append, gap-free. Two readers scanning the same partition necessarily see
  the same order.

The log is *partitioned* so that throughput can scale beyond one lock, and
records are routed to a partition by key. The key for a conversation is the
conversation ID, so every message in a conversation lands in one partition and
therefore in one total order. See [Ordering guarantees](ordering.md).

## What this costs

Honesty about the trade:

**Storage grows.** A queue's steady state is empty. A log's steady state is
everything ever sent. This is the correct behaviour for chat — but it means
retention is a product decision you must make explicitly, not a detail that
takes care of itself. See [Durability](durability.md#retention).

**Deletion is coarse.** Records are removed a whole segment at a time. There is
no "delete message 4053". If you need per-message deletion for a "delete for
everyone" feature or a GDPR erasure request, you implement it as a tombstone
record the client honours, plus — for the legal case — key-based crypto-shredding
where the message body is encrypted and erasure means destroying the key. The
log itself does not support surgical removal, by design: rewriting history in
place would break every cursor pointing past the removed record.

**Readers must track position.** A queue's ack is fire-and-forget. A log reader
has to commit a cursor, and a reader that never commits will re-read from its
last commit after a restart. This is a real burden on clients, and it is why
the gateway commits on the client's behalf when asked and defaults new
subscriptions to the head rather than the beginning.

## What did not change

The work queue is untouched. `PUBLISH`, `CONSUME`, `ACK`, `NACK`, exchanges,
dead-letter queues, the Raft-replicated broker — all of it behaves exactly as
before, on the same ports, with the same wire protocol. The streaming subsystem
is off unless `messaging.stream.enabled` is set, and when it is off BoltQ does
not even create the directory.

If you are running BoltQ for jobs today, nothing here affects you. If you want
both, run both: they share a process and nothing else.

## Further reading

- [The stream engine](stream-engine.md) — how the log is actually stored.
- [Message lifecycle](message-lifecycle.md) — the full path of one message.
- [Migrating from queues](../guides/migrating-from-queues.md) — if you have an
  existing deployment.
