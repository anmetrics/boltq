# The stream engine

The stream engine (`internal/stream`) is the storage layer everything else is
built on. This document describes how it works and why each decision was made.

## Layout

```
<data_dir>/streams/
├── chat.direct.alice%3Abob/          topic (name percent-encoded)
│   ├── 0/                            partition 0
│   │   ├── 00000000000000000001.log      segment, named by base sequence
│   │   ├── 00000000000000000001.index    sparse index for that segment
│   │   ├── 00000000000000004096.log
│   │   └── 00000000000000004096.index
│   ├── 1/
│   └── …
├── chat.group.eng-team/
├── chat.inbox.alice/
└── _cursors/
    └── cursors.log                   all reader positions
```

Topic names are user-controlled (`chat.direct.alice:bob`), so they are
percent-encoded before being used as a directory name. This is not cosmetic: a
topic named `../../etc/passwd` must not be able to escape the data directory,
and encoding is the only way to guarantee that without maintaining a blocklist.

## Records

Every record is a self-describing frame:

```
┌─────────┬──────────┬──────────────────────────────────────────────┐
│ crc32:4 │ bodyLen:4│ body                                         │
└─────────┴──────────┴──────────────────────────────────────────────┘
                     │
                     ├─ seq:8         monotonic within the partition
                     ├─ timestamp:8   UnixNano, assigned by the broker
                     ├─ flags:1
                     ├─ keyLen:2
                     ├─ hdrLen:4
                     ├─ payloadLen:4
                     ├─ key           partition key (conversation ID)
                     ├─ headers       length-prefixed k/v pairs
                     └─ payload       opaque application bytes
```

Three decisions worth explaining:

**The CRC covers the body, and `bodyLen` sits outside it.** A reader reads the
length first, then the body, then checks the checksum. A torn write — the tail
of a file after an unclean shutdown — fails the check, and the reader stops
there. Putting the length inside the CRC would make a corrupt length
undetectable until after a wild allocation.

**Headers are length-prefixed pairs, not JSON.** A record is written once and
read many times: every member of a conversation reads it, the push dispatcher
reads it, history requests re-read it. A JSON unmarshal per read would dominate
the cost. The flat encoding parses with no intermediate allocation.

**The payload is opaque.** The broker never parses it. For an end-to-end
encrypted app the payload is ciphertext the server cannot read, and that has to
work without the server needing to understand anything about it.

The server always overwrites `seq` and `timestamp`, even if a client supplies
them. Sequence is the ordering authority; letting a client set it would let a
client rewrite history.

## Sequence assignment

```go
p.mu.Lock()
seq := p.nextSeq
rec.Seq = seq
// … encode, append …
p.nextSeq = seq + 1
p.mu.Unlock()
```

Sequences are assigned under the partition lock, one per append, with no gaps.
This is the entire ordering guarantee, and it is deliberately simple: any
scheme with gaps, or with sequences assigned before the write is durable, would
make "read from sequence N" ambiguous.

The cost is that a partition's write throughput is bounded by one lock. That is
the reason for partitioning: a topic with 16 partitions has 16 independent
locks. Within a conversation you get one order and one lock; across
conversations you get parallelism.

## Segments and the sparse index

A partition is a sequence of segments. Only the last accepts writes; the rest
are sealed and read-only. A segment rolls when it exceeds `segment_bytes`
(default 256MB).

Each segment has a **sparse index** — one `(seq, filePos)` entry per
`index_interval` bytes of log (default 4KB):

```
index:   seq 1 → 0        seq 43 → 4096     seq 91 → 8192
                 │                 │                 │
log:    ┌────────┴─────────────────┴─────────────────┴──────────┐
        │ 1 2 3 … 42          │ 43 44 … 90        │ 91 …        │
        └───────────────────────────────────────────────────────┘

read(seq=57):  binary search → seq 43 @ 4096
               seek to 4096, scan forward to 57  (≤ 4KB of scanning)
```

The index is sparse rather than dense because a busy partition holds millions
of records, and a dense index would cost roughly as much RAM as the data it
indexes. The trade is a bounded forward scan — at most `index_interval` bytes,
a few hundred microseconds — on every seek. For chat that is the right side of
the trade: reads are overwhelmingly sequential tails, where the scan happens
once and then the reader just keeps going.

## Crash recovery

On open, each segment is scanned from its last trustworthy index entry to EOF:

1. Index entries pointing past EOF are discarded — the index was fsynced but
   the log was not.
2. Records are read forward, verifying each CRC.
3. The first record that fails — bad CRC, impossible length, truncated read —
   ends recovery.
4. The file is truncated at that point, and `nextSeq` is set from the last
   good record.

Discarding a torn tail is safe because a record only reaches a publisher's
"sent" response after the append returns. A record lost to truncation was never
acknowledged, so no client believes it exists.

This is covered by `TestRecoveryTruncatesTornWrite`, which appends deliberate
garbage to a segment and asserts that the 20 intact records survive, the
garbage is truncated, and the next sequence continues correctly.

## Tailing

A reader at the head of the log needs to be woken when a record arrives.

```go
wake := p.NotifyChan()      // register FIRST
recs, _ := p.Read(from, …)  // then read
if len(recs) > 0 { … ; continue }
select {
case <-wake:                // sleep only if genuinely empty
case <-ctx.Done():
}
```

The registration must happen **before** the read. Reading first would leave a
window where an append lands between the read and the registration, and the
reader would sleep despite data being available — a wake-up lost until the
*next* message, which in a quiet conversation could be hours.

The wake-up itself is a closed-channel broadcast: `close(notify)` followed by
replacing the channel. This wakes every waiter at once and, unlike `sync.Cond`,
composes with `select` on `ctx.Done()`.

## Retention

Retention removes whole sealed segments — never partial ones, never the active
one:

```
before:  [seg 1..4095] [seg 4096..8191] [seg 8192..now, ACTIVE]
                 ↑ over the byte cap, drop it
after:                 [seg 4096..8191] [seg 8192..now, ACTIVE]
         FirstSeq: 1 → 4096
```

Coarse deletion means retention costs an `unlink`, not a rewrite. The
consequence is that `first_seq` jumps in segment-sized steps, and a reader
asking for a sequence below it gets `ErrSeqTruncated` rather than a silent gap.
The gateway turns that into an explicit `gap` frame so a client knows to
resynchronise instead of assuming it has complete history.

Both caps default to unlimited. For chat that is usually correct — the history
is the product — but it makes disk growth your explicit decision. See
[Capacity planning](../operations/capacity.md).

## Cursors

Reader positions live in a separate append-only log, compacted in place:

```
cursors.log:
  ("chat.group.g1", 3, "user:alice", "phone")  → 1042
  ("chat.group.g1", 3, "user:alice", "laptop") → 900
  ("chat.group.g1", 3, "push-dispatcher", "")  → 1042
```

A commit is one buffered write. When the file exceeds a threshold it is
rewritten with only the latest value per key. This shape was chosen because
commits are the highest-frequency write in the system — one per message per
device — and are overwhelmingly "same key, larger value", which is precisely
the workload a compacted log handles best and a B-tree handles worst.

Commits are **monotonic**: a lower sequence than the stored one is ignored.
Out-of-order commits are normal when a device has several requests in flight,
and honouring a stale one would silently replay messages the user already read.

Commits are flushed, not fsynced. See
[Durability](durability.md#cursors-are-not-fsynced) for why.

## What is not here

- **No compaction by key.** The `FlagTombstone` flag exists in the record
  format but no compaction process consumes it yet. Key-compacted topics (where
  only the latest record per key is kept) are not implemented.
- **No replication.** The stream log is local to a node. The Raft cluster
  replicates the *queue* broker, not the stream log. This is the largest gap
  for a multi-region deployment — see [Global HA](../operations/global-ha.md).
- **No tiered storage.** Segments live on local disk forever or until
  retention removes them. There is no path to object storage.

## Further reading

- [Durability](durability.md) — what survives a crash.
- [Ordering guarantees](ordering.md) — what order actually means here.
- [Cursors and multi-device](cursors.md).
