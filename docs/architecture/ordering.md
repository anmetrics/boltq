# Ordering guarantees

Ordering in a chat system is not a nicety. A reply that renders above the
message it answers is a bug users notice immediately and never forgive. This
document states precisely what BoltQ guarantees, and — more importantly — what
it does not.

## What is guaranteed

### Total order within a partition

Every record appended to a partition receives a sequence number exactly one
greater than the previous, assigned under the partition's lock:

```go
p.mu.Lock()
seq := p.nextSeq
rec.Seq = seq
// … write …
p.nextSeq = seq + 1
p.mu.Unlock()
```

Consequences:

- Sequences are **gap-free**. If you have records 40 and 42, record 41 exists
  (or was removed by retention, which moves `first_seq`, not the numbering).
- Any two readers scanning a partition see the **same order**. Order is a
  property of the log, not of the reader.
- Sequence order equals append order. There is no reordering by timestamp,
  priority, or anything else.

This is verified by `TestConcurrentAppendsAreSerialised`: eight goroutines
appending concurrently must between them receive each sequence in 1..N exactly
once, and reading back must produce them in order.

### Total order within a conversation

A conversation's messages all use the conversation ID as the partition key:

```go
partition = fnv1a(conversationID) % partitionCount
```

Since the key is fixed for a conversation, every message in it lands in the
same partition — and therefore inherits that partition's total order.

This is the guarantee that matters for chat, and it holds regardless of how
many senders are writing concurrently, which node they connected through, or
how the messages interleave with other conversations.

Verified by `TestPerConversationOrdering`: two senders write 50 messages each
into one conversation concurrently; reading back must yield 100 records with
consecutive sequences.

### Delivery order to a subscriber

A tailing subscriber receives records in sequence order, and the gateway writes
frames to a socket through a single serialising writer. A client that processes
frames in the order it reads them sees messages in log order.

## What is NOT guaranteed

### Order across partitions

Two conversations live in different partitions and have **no ordering relative
to each other**. If Alice sends to Bob at 10:00:00.000 and to Carol at
10:00:00.001, there is no guarantee about the relative order of those two
records in any global sense. There is no global sequence.

This is almost never a problem for chat, because users reason about
conversations independently. It *is* a problem if you try to build "a single
merged timeline across all conversations" from sequence numbers. Use timestamps
for that, and accept they are approximate.

### Order across topics

A conversation record and its inbox pointers are in different topics. The
conversation append happens first, so a pointer never references a nonexistent
record — but there is no ordering guarantee between two different users' inbox
streams.

### Timestamp ordering

`Record.Timestamp` is assigned by the server at append time, from
`time.Now()`. It is **not** the ordering authority and should not be used as
one:

- Two records in the same partition always have increasing sequences, but under
  a clock adjustment could in principle have non-increasing timestamps.
- Records in different partitions have unrelated timestamps and unrelated
  sequences.

**Sort by sequence within a conversation. Use timestamps for display only.**

Client-supplied timestamps are ignored entirely. Trusting them would let a
client place a message anywhere in the visual history of a conversation.

### Causal order across conversations

If Alice messages Bob "check the group chat" and then posts to the group, there
is no mechanism ensuring Bob observes the group post after the direct message.
They are separate partitions with separate delivery. BoltQ has no vector clocks
and no causal metadata.

If you need this, encode it yourself: put a reference in the payload and have
the client wait for what it references.

### Order across a region boundary

There are no regions in the ordering model, because the stream log is not
replicated across them. See [Global HA](../operations/global-ha.md).

## Implications for clients

**Sort by `seq`, always.** Not by arrival time, not by timestamp. The gateway
delivers in order, but a client that also fetches history concurrently can have
records arrive out of order relative to each other; sorting by sequence
reconciles them.

**Detect gaps by sequence arithmetic.** If you hold up to 1041 and receive
1043, you missed 1042 — fetch it. This is only meaningful because sequences are
gap-free.

**Handle the `gap` frame.** If retention removed records below your cursor, the
gateway sends `op: "gap"` with `first_seq`. You have a genuine hole; show a
"older messages are unavailable" marker rather than pretending history is
complete.

**Deduplicate by `message_id`.** Delivery is at-least-once across reconnects. A
message can be delivered twice; it will have the same sequence and the same
message ID both times.

## Why not a global sequence

A single global sequence across all conversations would make cross-conversation
ordering trivially available. It is not offered because it would require every
append in the system to pass through one lock — which is precisely the
bottleneck partitioning exists to remove. The choice is between global ordering
at single-partition throughput, or per-conversation ordering at N-partition
throughput. For chat, the second is obviously correct: nobody needs a global
order of every message in the system, and everybody needs conversations to be
fast.

## Further reading

- [The stream engine](stream-engine.md) — how sequences are assigned.
- [Message lifecycle](message-lifecycle.md).
- [Gateway protocol](../reference/gateway-protocol.md) — the `gap` frame.
