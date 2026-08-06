# Fan-out strategies

A 3-person direct chat and a 50,000-person announcement channel look identical
from the client's perspective — one send, everyone sees it. They must not be
implemented identically.

## The problem

The obvious implementation of "deliver to everyone" is to write a copy into
each recipient's inbox. This is **fan-out on write**, and for small
conversations it is exactly right:

```
alice sends "hi" to a 3-person group

  chat.group.g1        ← the message
  chat.inbox.alice     ← pointer
  chat.inbox.bob       ← pointer
  chat.inbox.carol     ← pointer

  4 writes for 1 message
```

Now scale it:

```
alice sends "hi" to a 50,000-person channel

  chat.group.announcements  ← the message
  chat.inbox.<user 1>       ← pointer
  …                            × 50,000
  chat.inbox.<user 50000>   ← pointer

  50,001 writes for 1 message
```

One person typing "hi" becomes fifty thousand writes. During a busy period in a
large channel this is not a slow path — it is an outage. Write amplification
proportional to audience size is the single most common way a chat backend
falls over.

## The two strategies

BoltQ picks per message, based on the recipient count:

### Fan-out on write (small conversations)

```
     ┌──────────────────────┐
     │ chat.group.g1        │  the message, with its payload
     │   seq 1042: "hi"     │
     └──────────────────────┘
              │
              ├──▶ chat.inbox.alice   seq 88:  → g1/1042
              ├──▶ chat.inbox.bob     seq 412: → g1/1042
              └──▶ chat.inbox.carol   seq 7:   → g1/1042
                   (pointers — no payload)
```

Each member's inbox gets a **pointer**: a record with headers naming the
conversation, partition and sequence, and no payload at all. Copying the body
into every inbox would multiply storage by the member count and, for an
encrypted app, would be duplicating ciphertext for no benefit.

The inbox is what makes a cold app start cheap. A client that has been offline
tails **one** stream — its own inbox — and learns everything that happened
across every conversation, then fetches the bodies it actually needs. Without
an inbox it would have to poll every conversation it belongs to.

### Fan-out on read (large conversations)

```
     ┌──────────────────────┐
     │ chat.group.announce  │  the message
     │   seq 90210: "hi"    │
     └──────────────────────┘
              ▲  ▲  ▲  ▲
              │  │  │  │      50,000 subscribers tail this partition
              │  │  │  │      directly, each with its own cursor
```

No inbox writes at all. Members subscribe to the conversation and read it
directly. One send is one write, regardless of audience.

The cost is that a member who has been offline does not learn about the channel
from their inbox — they have to check the conversation explicitly. For a large
channel that is the right trade: the client already knows it is subscribed, and
"unread count for a busy channel" is a cheap `next_seq - my_cursor` subtraction
rather than fifty thousand pointer records.

## The boundary

```
messaging.chat.fanout_on_write_limit   default: 256
```

At or below the limit, fan-out on write. Above it, fan-out on read.

256 was chosen so worst-case write amplification for one send stays in the low
hundreds — comfortably inside a single batch — while covering essentially every
direct chat and most group chats in a dating or messaging app. A dating app in
particular is almost entirely 1:1, so nearly every send takes the on-write path
and the on-read path exists for the occasional broadcast.

You can query the boundary:

```
GET /messaging/overview
```

and the response of a send tells you which path it took:

```json
{ "strategy": "fanout_on_write", "recipients": 3, "inbox_writes": 3 }
{ "strategy": "fanout_on_read",  "recipients": 50000, "inbox_writes": 0 }
```

## Invariants that hold either way

**The conversation log is always written.** It is the ordering authority and
the complete history regardless of strategy. Changing the limit does not change
what history exists — only which index entries were built.

**The conversation append happens first.** Inbox pointers are written after,
and only after, the conversation append returns. The reverse order would risk
an inbox pointing at a message that does not exist.

**A partial fan-out does not fail the send.** If the conversation append
succeeded but some inbox writes did not, the message is durable and visible.
The client is told the send succeeded and the failure is logged, because
reporting "your message failed" for a message that is sitting in the log would
be worse than a missing index entry.

## Changing the limit later

Raising or lowering `fanout_on_write_limit` affects only future sends.
Conversations that crossed the old boundary keep whatever pointers they already
had; conversations that cross the new one start behaving differently from the
next message.

This means a client must not assume its inbox is complete for large
conversations. The safe client behaviour is: tail your inbox *and* tail the
conversations you have open. That is what a chat UI does anyway.

## What is not implemented

**No adaptive strategy.** The limit is a static number, not a decision based on
observed write pressure or per-conversation activity. A 200-member channel that
is extremely busy still takes the on-write path.

**No partial fan-out.** There is no mode that writes pointers for *active*
members only. Doing that properly requires activity data BoltQ does not have.

**No fan-out to a secondary index.** Features like "all my unread mentions
across every channel" would need a separate derived stream. The pieces exist
(you can consume conversation logs and append to your own topic) but nothing
does it for you.

## Further reading

- [Message lifecycle](message-lifecycle.md) — where fan-out sits in the path.
- [Cursors and multi-device](cursors.md) — how unread counts work.
- [Capacity planning](../operations/capacity.md) — the numbers.
