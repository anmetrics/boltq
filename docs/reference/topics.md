# Topic conventions

Topic names are the contract shared by three things: the ACL rules, the fan-out
logic, and every client SDK. Changing one means changing all three.

## The namespace

```
chat.direct.<conversationID>       1:1 conversation
chat.group.<groupID>               many-party conversation
chat.inbox.<userID>                a user's cross-conversation index
presence.<userID>                  a user's presence (ephemeral)
typing.<conversationID>            typing signals (ephemeral)
system.<...>                       server-originated announcements
```

## `chat.direct.<conversationID>`

The full history of a 1:1 conversation. Partition key is the conversation ID,
so all its messages share one partition and one total order.

**Conversation IDs should be the sorted user IDs joined by `:`** —
`alice:bob`, never `bob:alice`. Both participants must derive the same ID, or
they end up in two conversations each holding half the messages.

This convention also means direct conversations need **no membership service**:
the members are derivable from the ID.

```
chat.direct.alice:bob
chat.direct.user-8891:user-9902
```

Access: read/write for members, checked per request.

## `chat.group.<groupID>`

The full history of a group conversation. Same partitioning rule.

Group membership is **not** derivable from the ID, so a
[membership service](../guides/auth.md#the-membership-service) is required.
Without one, group conversations are unavailable — which is a deliberate fail-
closed, not a silent open.

```
chat.group.eng-team
chat.group.g-7f3a9c
```

## `chat.inbox.<userID>`

A user's index of everything that happened across all their conversations.
Contains **pointer records only** — headers naming the conversation, partition
and sequence, with no payload.

This is what makes a cold app start cheap: tail one stream, learn about
everything, then fetch the bodies you need.

```
chat.inbox.alice
```

Access: read/write by that user **only**. Every other user's inbox is
explicitly denied, not merely un-granted, so a broad allow rule added later
cannot open it.

Populated by fan-out on write. **For conversations above
`fanout_on_write_limit` there are no pointers** — a client must tail those
conversations directly. See [Fan-out](../architecture/fanout.md).

Default: 1 partition. Each inbox is its own topic, so more would only multiply
file handles.

## `presence.<userID>`

A user's presence. Ephemeral — never written to the log.

```
presence.alice
```

Access is **asymmetric on purpose**: a user writes only their own, but may read
anyone's. Being able to observe everyone's presence is what a contact list is.

If your product needs presence restricted to mutual matches, replace that ACL
rule and gate it on membership instead.

## `typing.<conversationID>`

Typing indicators. Ephemeral, rate limited, dropped when a subscriber is slow.

```
typing.alice:bob
```

Access: read/write for conversation members. You are subscribed automatically on
your first `typing` frame for a conversation — send `typing: false` on opening a
conversation to receive others' indicators without claiming to type.

## `system.<...>`

Server-originated announcements. Read-only for users; writing requires the
`admin` scope.

```
system.maintenance
system.announcements
```

## Naming rules

Topic names are user-controlled, so they are **percent-encoded** before being
used as a directory name:

```
chat.direct.alice:bob   →   chat.direct.alice%3Abob
```

Only `a-z A-Z 0-9 . - _` survive unencoded. This is not cosmetic — a topic
named `../../etc/passwd` must not be able to escape the data directory, and
encoding guarantees that without maintaining a blocklist.

Constraints:

- Non-empty.
- Encoded form ≤ 200 characters, so it fits filesystem limits.
- Segments are dot-separated; a dot in an ID becomes a segment boundary for ACL
  matching. **Avoid dots inside conversation and user IDs** — they will change
  which ACL patterns match.

## Pattern matching

Used by both the ACL and, in a different form, the queue exchange layer.

| Pattern | Matches | Does not match |
|---|---|---|
| `chat.inbox.alice` | exactly that | anything else |
| `chat.inbox.*` | `chat.inbox.alice` | `chat.inbox.alice.archive` |
| `chat.inbox.#` | `chat.inbox`, `chat.inbox.alice`, `chat.inbox.alice.archive` | `chat.group.g1` |
| `chat.group.*.#` | `chat.group.g1`, `chat.group.g1.meta` | `chat.direct.d1` |
| `#` | everything | — |

`*` is exactly one segment. `#` is zero or more trailing segments.

Placeholders expand from the authenticated principal before matching:
`${user}`, `${device}`, `${tenant}`.

## Custom topics

Nothing stops you creating your own. They are ordinary stream topics — you just
need ACL rules for them, since the default is deny.

Useful shapes:

```
match.<userID>            swipe/match events for a dating app
notify.<userID>           in-app notifications
audit.<tenant>            an append-only audit trail
search-index              a derived stream for an indexer to consume
```

Give each consumer its own **cursor group** and it gets an independent position
over the same log with no coordination with anyone else. That is how you add an
analytics pipeline or a moderation scanner without touching the delivery path.

## Partition resolution

Clients rarely need to compute a partition — omit it and the gateway resolves
it. When you do need it:

```
partition = fnv1a_64(key) % partitionCount
```

The key is the conversation ID for conversation topics, and the user ID for
inbox topics. FNV-1a/64 was chosen because it is trivially reimplementable in
every client language. **Changing this function is a breaking wire change.**

## Further reading

- [Authentication and authorisation](../guides/auth.md) — the ACL.
- [Fan-out strategies](../architecture/fanout.md).
- [Gateway protocol](gateway-protocol.md).
