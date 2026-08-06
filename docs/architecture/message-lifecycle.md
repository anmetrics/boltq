# Message lifecycle

This traces one message from Alice's phone to Bob's, through every component,
including the paths taken when things go wrong.

## The happy path

Alice and Bob are both connected. Alice sends "hello".

```
Alice's phone                BoltQ                          Bob's phone
     │                         │                                 │
     │  1. send                │                                 │
     │  {op:"send",            │                                 │
     │   conversation:"a:b",   │                                 │
     │   client_msg_id:"c1",   │                                 │
     │   payload:"hello"}      │                                 │
     ├────────────────────────▶│                                 │
     │                         │ 2. authorize(write, chat.direct.a:b)
     │                         │ 3. dedup.Claim(alice, "c1")     │
     │                         │ 4. members = [alice, bob]       │
     │                         │ 5. append → chat.direct.a:b     │
     │                         │            partition 7, seq 1042│
     │                         │            ── wakes tailers ────┼──┐
     │                         │ 6. inbox pointers:              │  │
     │                         │      chat.inbox.alice           │  │
     │                         │      chat.inbox.bob             │  │
     │                         │ 7. dedup.Complete               │  │
     │  8. sent                │                                 │  │
     │  {seq:1042, …}          │                                 │  │
     │◀────────────────────────┤                                 │  │
     │                         │  9. record                      │  │
     │                         ├────────────────────────────────▶│◀─┘
     │                         │  {seq:1042, payload:"hello"}    │
     │                         │                                 │
     │                         │  10. commit {seq:1043}          │
     │                         │◀────────────────────────────────┤
```

Two things are worth noticing.

**Step 5 is the only step that matters for durability.** Once the append
returns, the message exists and is ordered. Everything after it — inbox
pointers, the response to Alice, the delivery to Bob — is derived. If the
process died at step 6, the message would still be in the conversation log and
still visible to anyone who read it.

**Step 9 is not a separate delivery action.** Bob's connection is tailing
partition 7. The append in step 5 closed the partition's notify channel, which
woke Bob's tail goroutine, which read the record and wrote it to his socket.
There is no code path that "sends to Bob" — there is only "append", and
everyone watching wakes up. This is why a message cannot be stored-but-not-
delivered: delivery is a consequence of storage, not a second operation that
can independently fail.

## Bob is offline

Steps 1–8 are identical. Step 9 does not happen — nobody is tailing.

```
     │                         │ 5. append → seq 1042
     │                         │ 6. inbox pointer → chat.inbox.bob, seq 88
     │                         │
     │                    ┌────┴─────────────────────────────┐
     │                    │  push dispatcher                 │
     │                    │  (separate goroutine, own cursor)│
     │                    │                                  │
     │                    │  reads chat.inbox.bob from 88    │
     │                    │  presence.Online("bob") → false  │
     │                    │  wait GraceDelay (3s)            │
     │                    │  POST /push-webhook              │
     │                    │    {notifications:[{user:"bob",  │
     │                    │      conversation:"a:b",         │
     │                    │      conv_seq:1042, …}]}         │
     │                    │  commit cursor → 89              │
     │                    └──────────────────────────────────┘
                                        │
                                        ▼
                          your application → APNs/FCM → Bob's phone
```

The dispatcher is a **normal log consumer with its own cursor**, running behind
the write path. Alice's send never waits on APNs. If the push provider is down,
the dispatcher's cursor simply stops advancing; when it recovers, it picks up
exactly where it left off. There is no queue to overflow and no coupling
between "the message was stored" and "the notification was sent".

The grace delay exists because a user who opens the app within a couple of
seconds should read the message in the app, not get a notification for
something they are already looking at.

## Bob reconnects

```
Bob's phone                    BoltQ
     │                           │
     │  hello {resume:"sess.tok"}│
     ├──────────────────────────▶│  SessionStore.Resume:
     │                           │    constant-time token compare
     │                           │    same user? yes
     │                           │    not already attached? yes
     │  welcome {resumed:true}   │
     │◀──────────────────────────┤
     │                           │  restore subscriptions from the session,
     │                           │  each from where delivery actually got to
     │  record {seq:1042…}       │
     │◀──────────────────────────┤
```

If the resume window has passed, or the token is wrong, Bob gets a fresh
session. He then subscribes normally, and because subscribe with no explicit
position resolves to his **committed cursor**, he still resumes from the right
place — just with the extra round trips of re-subscribing. The session is an
optimisation over the cursor, not a replacement for it.

A resume is refused if the session is still attached. Refusing rather than
stealing means two devices sharing a leaked token cannot fight over one session
and corrupt each other's cursors.

## Alice's send is retried

Alice's network drops after step 5 but before step 8. Her phone never sees the
response and retries with the same `client_msg_id`.

```
     │  send {client_msg_id:"c1"}      │
     ├────────────────────────────────▶│  dedup.Claim(alice, "c1")
     │                                 │    → already complete
     │  sent {seq:1042, duplicate:true}│
     │◀────────────────────────────────┤
```

She gets the **original** coordinates back, so her optimistic local echo
resolves to the same message rather than becoming a second one. The
conversation holds exactly one copy.

If the retry arrives while the first is still in flight, she gets
`conflict / duplicate send in flight, retry shortly` — a distinct answer,
because handing her a zero-valued result would be wrong and letting her claim
again would produce the duplicate.

If the original send *failed* — say the membership service was down — the claim
is released, and the retry is treated as a genuinely fresh attempt. This
matters: deduplicating against a message that was never stored would lose it
permanently.

## Alice types before sending

```
     │  {op:"typing", conversation:"a:b", typing:true}
     ├───────────────────────────────────▶│
     │                                    │  ephemeral hub
     │                                    │    rate limit check (5/s)
     │                                    │    NO disk write
     │                                    │    NO replication
     │                                    │    push to subscribers,
     │                                    │      dropping any that are full
     │                                    ├────────────────────▶ Bob
```

This is a completely separate path. Typing signals outnumber messages by an
order of magnitude — ten seconds of typing produces ten signals to send one
message — and none of them are worth a disk write. Routing them through the log
would mean the least valuable traffic consumed most of the write capacity.

They are dropped without ceremony when a subscriber is slow, and nothing
survives a restart. Both are correct: a typing indicator from before the
process died is not information.

## Where each guarantee comes from

| Property | Mechanism |
|---|---|
| Message is durable once acknowledged | Append returns only after the write reaches the page cache; see [Durability](durability.md) |
| Messages in a conversation are ordered | Conversation ID is the partition key; sequence assigned under one lock |
| No duplicates from retries | `dedup` claim table keyed by (sender, client_msg_id) |
| Offline users get notified | Push dispatcher tails inbox topics with its own cursor |
| Reconnect does not lose messages | Cursors are durable; session resume is an optimisation on top |
| One user's devices track independently | Cursor group = user, cursor member = device |
| A slow client cannot stall others | Bounded send queue; the client is disconnected rather than the server blocked |

## Further reading

- [Fan-out strategies](fanout.md) — why step 6 does not always happen.
- [Cursors and multi-device](cursors.md).
- [Gateway protocol](../reference/gateway-protocol.md) — the exact frames.
