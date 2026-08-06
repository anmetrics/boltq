# Cursors and multi-device

A cursor is a reader's position in a partition. It is the only per-reader state
in the system, and nearly every feature users think of as "the app remembering
things" reduces to it.

## The key structure

```
CursorKey{
    Topic:     "chat.group.eng-team",
    Partition: 3,
    Group:     "user:alice",     ← the logical reader
    Member:    "phone",          ← the instance within it
}  → 1042
```

The **group/member split** is what makes multi-device work.

One user with a phone, a laptop and two tablets is *one group* with *four
members*. Each holds an independent position:

```
chat.group.eng-team, partition 3          next_seq: 1100

  user:alice / phone    → 1042      42 unread    ← was reading on the train
  user:alice / laptop   →  900     200 unread    ← left open at work
  user:alice / tablet-1 → 1100       0 unread    ← currently in hand
  user:alice / tablet-2 →  310     790 unread    ← in a drawer

  watermark (slowest member) = 310
```

Reading on the tablet advances only the tablet's cursor. The phone still shows
42 unread, which is correct — Alice has not read them *on the phone*, and a
chat app that silently marked them read everywhere would be wrong for exactly
the reason users complain about.

## Why not one cursor per user

A single per-user cursor is simpler and wrong in both directions:

- Advance it when *any* device reads, and a device that has been offline for a
  week reconnects believing it is caught up, showing nothing.
- Advance it only when *all* devices read, and a tablet in a drawer pins the
  unread count at hundreds forever.

Neither is acceptable, so the position is per device, and the aggregate — the
watermark — is computed when needed rather than stored.

## Groups other than users

`Group` is not restricted to users. The push dispatcher uses its own:

```
chat.inbox.bob / partition 0 / push-dispatcher / ""   → 89
```

This is why push progress and read progress are independent. The dispatcher
advancing does not mark anything read, and Bob reading does not skip
notifications for messages the dispatcher has not evaluated.

Any consumer you build — an analytics pipeline, a moderation scanner, a search
indexer — takes its own group and gets its own independent position over the
same log, with no coordination with anyone else.

## Unread counts

```
unread = partition.next_seq - cursor
```

That is the whole computation. It is O(1) — a subtraction of two integers — and
it works for a channel with a million messages exactly as well as for one with
ten. There is no per-message read state to scan, which is the usual reason
unread counts become slow.

Query it:

```
GET /streams/cursors?topic=chat.group.eng-team&partition=3&group=user:alice
```

```json
{
  "topic": "chat.group.eng-team",
  "partition": 3,
  "group": "user:alice",
  "members": { "phone": 1042, "laptop": 900, "tablet-1": 1100 },
  "watermark": 900,
  "next_seq": 1100,
  "lag": 200
}
```

## Monotonicity

A commit below the stored value is **ignored**, not applied.

Out-of-order commits are normal: a device with several requests in flight can
have a commit for sequence 900 arrive after one for 1000, simply because of
network reordering or a retry. Honouring the stale one would rewind the cursor
and silently replay messages the user already read — a bug that presents as
"the app keeps showing me old messages as new".

Verified by `TestCursorCommitIsMonotonic`.

## Where cursors are used

**Resuming a subscription.** `subscribe` with no explicit `from_seq` resolves
to the committing device's cursor, or the partition head if there is none.
Defaulting to the head rather than the beginning matters — a client attaching
to a busy channel for the first time should not be flooded with its entire
history.

**Session resume.** A resumed session restores subscriptions from where
delivery actually reached, which is slightly ahead of the committed cursor (the
difference being records delivered but not yet acknowledged). If the session is
gone, the client falls back to the cursor and loses only round trips.

**Push dispatch.** The dispatcher's cursor is what makes push at-least-once and
restart-safe. A fresh dispatcher starts at the *head*, not the beginning, so
enabling push notifications does not notify every user about every message they
ever received (`TestFreshDispatcherStartsAtHead`).

## Storage

Cursors live in a compacted append-only log, `_cursors/cursors.log`:

```
append:   (key, seq) …  (key, seq) …  (key, seq) …
                              ↓ threshold exceeded
compact:  one record per live key, sorted
```

This shape was chosen because commits are the highest-frequency write in the
system — one per message per device — and are overwhelmingly "same key, larger
value". That is the workload a compacted log handles best and a B-tree handles
worst: a B-tree would rewrite a page per commit, while an append is a memcpy
into a buffer.

Compaction is concurrency-safe: commits arriving during a rewrite are detected
after the swap and re-appended, so none are lost
(`TestCursorCompactionPreservesState`).

Deletion forces a compaction, because the format has no tombstone. Device
unlinking is rare enough for that to be fine.

## Durability

Commits are buffered and flushed every second, not fsynced per commit. Losing
the last second means a device re-receives a handful of messages it already had
— which client-side dedup by message ID handles anyway. See
[Durability](durability.md#cursors-are-not-fsynced).

Cursors *are* fsynced on graceful shutdown.

## Retention interaction

Retention does not consult cursors. A segment is dropped when it exceeds the
policy, whether or not a reader is still behind it.

A reader whose cursor falls below `first_seq` gets `ErrSeqTruncated`, surfaced
to clients as a `gap` frame. This is deliberate: a device that has been offline
for longer than the retention window has genuinely missed messages, and the only
honest response is to say so. The alternative — pinning segments until every
device catches up — would let one abandoned tablet hold a channel's entire
history on disk forever.

## Client responsibilities

**Commit after processing, not after receiving.** Committing on receipt means a
crash between receipt and rendering loses the message from the user's
perspective.

**Commit `lastSeq + 1`.** The cursor is "the next sequence I want", not "the
last one I read".

**Do not commit on every message in a burst.** Commit every N messages or every
few hundred milliseconds. The cost of a lost commit is redelivery, which dedup
absorbs.

**Handle the `gap` frame.** Show the user that older history is unavailable
rather than silently presenting an incomplete conversation.

## Further reading

- [The stream engine](stream-engine.md#cursors) — storage details.
- [Ordering guarantees](ordering.md).
- [Gateway protocol](../reference/gateway-protocol.md) — `commit` and `gap`.
