# Messaging subsystem — status and open work

**Purpose of this file.** The messaging subsystem was built quickly and works,
but several gaps are load-bearing and easy to forget. This is the honest
inventory: what is verified, what is missing, what will cost you data, and what
decisions are still open. Read it before trusting the subsystem in production,
and update it when any line stops being true.

Last verified: all packages build, `go vet` clean, full suite race-clean.

---

## 1. What is verified

Not "implemented" — *verified*, with a test that would fail if it broke.

| Property | Evidence |
|---|---|
| Messages in a conversation are totally ordered | `TestPerConversationOrdering`, `TestConcurrentAppendsAreSerialised` |
| Sequences are gap-free under concurrency | `TestConcurrentAppendsAreSerialised` |
| A torn write is truncated, the healthy prefix survives | `TestRecoveryTruncatesTornWrite` |
| A mobile retry never duplicates a message | `TestDuplicateSendReturnsOriginal`, `TestConcurrentIdenticalSendsProduceOneMessage` |
| One user cannot read another's inbox | `TestPolicyOwnInboxOnly`, `TestSubscribeDeniedForOtherUsersInbox` |
| A membership-service outage denies rather than allows | `TestPolicyMembershipFailureDeniesAccess` |
| Forged/`alg=none`/tampered tokens are rejected | `TestVerifyRejectsAlgNone`, `TestVerifyRejectsTamperedClaims` |
| A leaked resume token cannot be used by another user | `TestResumeRejectsDifferentUser` |
| Each device keeps an independent read position | `TestCommitPersistsPerDeviceCursor` |
| A stale cursor commit cannot rewind and replay | `TestCursorCommitIsMonotonic` |
| A slow client is disconnected, not allowed to stall the server | `TestSlowSubscriberIsDroppedNotBlocking` |
| Replicated records are byte-identical and in the same order | `TestConcurrentAppendsReplicateInOrder` |
| A send actually waits for quorum when configured | `TestSendWaitsForReplication` |
| Data survives `kill -9` of the leader | two-node manual run, see §5 |

End-to-end, against two real server processes: 12 messages over WebSocket
replicated byte-for-byte to a follower; the leader was `kill -9`'d and the
follower still served all 12.

---

## 2. Bugs found and fixed (do not reintroduce)

These are recorded because each one passed review and testing before being
caught, and each has a regression test now.

### Quorum was configured but not enforced — **silent data-durability bug**

`fanout.Send` called `Log.Append`, but only `Log.AppendContext` consults the
`AckWaiter`. An operator setting `min_in_sync: 2` saw the startup log line
`quorum acknowledgement on (min_in_sync=2)` and believed acknowledged messages
were on two nodes. They were not — every send was acknowledged from the
leader's page cache alone.

Worse: the two-node integration test **passed**, because asynchronous
replication still delivered the records. The test asserted "the data arrived",
not "the send waited".

Guarded by `TestSendWaitsForReplication`, which blocks the waiter and asserts
the send blocks. If you ever add a new write path, it must use
`AppendContext`.

### `Close()` deadlocked with a connected follower

`Leader.Close` called `wg.Wait()` on goroutines blocked in a socket read.
Cancelling a context does not interrupt a blocked read; shutdown has to close
the sockets. Same bug existed on the follower side.

### A leader gave up permanently on a topic that did not exist yet

Stream topics are created lazily on the first message, so a follower assigned a
partition before anyone had spoken in that conversation got an error and the
stream goroutine returned **forever**. Now `awaitPartition` polls until it
appears.

### ACL deny-wins blocked users from their own inbox

`chat.inbox.#` Deny is needed so a later broad allow cannot open inboxes, but it
also denied each user their own. Fixed by adding `ExceptPattern`.

### `Read(0, ...)` reported history loss that never happened

Sequence 0 never names a record, so a caller passing it means "from the
beginning". It returned `ErrSeqTruncated`, which a client surfaces as "older
messages were removed". Now clamped to `firstSeq`.

### `BOLTQ_ADMIN_URL` was build-time only in the web console

`nuxt.config.ts` reads it when the app is *built*. An operator setting it on the
running container silently kept the baked-in `localhost:9090`. `getBoltqUrl()`
now re-reads the environment per request.

---

## 2b. The two planes fail differently — and only one of them says so

Queue and messaging run in the **same process** but have **completely separate**
high-availability machinery. They are configured independently, and nothing
links them.

| | Queue plane | Messaging plane |
|---|---|---|
| Replication | Raft (`internal/cluster`) | leader/follower (`internal/replication`) |
| Node dies → new leader elected | **automatic** | **manual promotion** |
| Clients redirected | `NOT_LEADER` at 10 call sites in `internal/api/tcp.go` | **none** — the gateway has zero leader awareness |
| Raft leader ≡ stream leader? | — | **not enforced anywhere** |

Verified: `internal/cluster/fsm.go` holds `broker *broker.Broker` and contains
zero references to `stream.`; `cmd/server/messaging.go` never references
`cluster.`; `buildMessaging(cfg, nodeID)` receives only a node ID.

**The operational trap.** A node dies. Raft elects a new queue leader, queue
clients redirect, `/cluster/status` reports a healthy quorum. If that node was
also the stream leader, chat is down — and nothing in the Raft health signal
says so.

Two mitigations are in place:

1. **`GET /replication`** (and a `replication` section in
   `/messaging/overview`) reports this node's role, connected followers,
   per-partition replica lag, and `quorum_available` — the derived signal for
   "can writes still reach the configured durability". A standalone node
   reports `role: "standalone"` with an explicit warning rather than null.
   **Alert on this separately from `/cluster/status`.**

2. **Config refuses `role: "follower"` together with `gateway.enabled`.**
   Nothing in the write path checks partition leadership, so a send that
   reached a follower would append locally and assign its own sequences —
   two divergent logs with the same name, which no reconciliation can undo.
   Guarded by `TestFollowerCannotServeGateway`.

Still missing: a `NOT_LEADER` equivalent for the gateway, so a client that
reaches the wrong node is redirected rather than silently refused. That belongs
with automatic failover (§7).

## 3. Gaps that cost data today

**Fix these before relying on replication in production.**

### 3.1 Cursors are not replicated

Reader positions live in the leader's local `_cursors` directory. Promoting a
follower recovers the messages but **not the read positions**: every device
re-reads from wherever the promoted node's cursor store happens to be, and the
push dispatcher restarts at the head.

- Messages are not lost. Unread state and push progress are.
- Workaround today: back up `_cursors` with the stream directory, or accept the
  resync after a failover.
- Proper fix: ship cursor commits over the existing replication link. They are
  small and already an append-only log — the same transport works.

### 3.2 A follower only replicates the topics it was configured with

`messaging.replication.topics` is an explicit list. New conversations create new
topics, so **a conversation created after the follower was configured is not
replicated** until someone edits the config and restarts it.

For a dating app this is severe: every new match creates a conversation, and
none of them are protected.

- Proper fix: wildcard assignments (`chat.direct.*`), with the follower
  discovering topics from the leader and fetching them automatically.

---

## 4. Gaps that are limitations, not bugs

Each is documented in detail where it belongs; this is the index.

| Gap | Impact | Detail |
|---|---|---|
| No automatic failover | Leader death needs a manual promotion | [replication.md](architecture/replication.md#manual-failover-procedure) |
| No follower reads | A follower's copy is for durability only | [replication.md](architecture/replication.md) |
| No cross-node read path | A client must reach the node leading its conversation | [global-ha.md](operations/global-ha.md) |
| Presence/sessions/dedup are per node | Sharding needs an external presence store | [global-ha.md](operations/global-ha.md) |
| Partition count fixed at creation | No resharding; choose with headroom | [stream-engine.md](architecture/stream-engine.md) |
| No tiered storage | History lives on local disk until retention removes it | [capacity.md](operations/capacity.md) |
| Deletion is segment-granular | No "delete message N"; use tombstones + crypto-shredding | [why-a-log.md](architecture/why-a-log.md) |
| Stream publish is not exposed over the network | Backend services must import the Go package to publish events | §6 below |
| No consumer-group rebalancing | Multiple instances do not split partitions automatically | — |
| Gateway has no leader redirect | A client reaching the wrong node gets no `NOT_LEADER` equivalent | §2b |
| Retention ignores cursors | A device offline longer than the window genuinely misses messages | [cursors.md](architecture/cursors.md) |

---

## 5. Reproducing the two-node verification

```bash
# leader.json: replication { enabled, role: "leader", listen: "127.0.0.1:19200",
#                            secret: "...", min_in_sync: 2 }
# follower.json: replication { enabled, role: "follower",
#                              leader_addr: "127.0.0.1:19200", secret: "...",
#                              topics: ["chat.direct.alice:bob:2", "chat.inbox.bob:1"] }

./boltq -config leader.json &
./boltq -config follower.json &

go test -tags smoke ./test/smoke/     # drives real WebSocket traffic

curl -s localhost:19190/streams       # leader
curl -s localhost:19290/streams       # follower — byte counts must match

kill -9 <leader pid>
curl -s 'localhost:19290/streams/topic?name=chat.direct.alice:bob'
# records must still be there
```

The smoke test lives behind a build tag (`//go:build smoke`) because it needs a
running server. It is not part of the normal suite.

---

## 6. Event bus — what works and what does not

Three event-bus shapes exist. Only the first two are usable over the network.

| Shape | Publish | Subscribe |
|---|---|---|
| Pub/Sub topics (queue plane) | TCP `0x02` | TCP `0x03` |
| Exchanges (direct/fanout/topic/headers) | TCP `0x18` | bind queues, TCP `0x16` |
| **Stream topics** | **in-process Go only** | WebSocket `subscribe` (any topic, ACL-gated) |

The gap: `gateway.handleSend` requires a `conversation` field, so it always
routes to `chat.direct.*` / `chat.group.*`. There is no way to publish an
arbitrary event to an arbitrary stream topic from outside the process.

For a dating app this matters — swipe/match events are a natural stream use
case and cannot currently be published over the network.

- Minimal fix: a `publish` op on the gateway (topic + key + payload, gated by
  `ActionWrite`).
- Fuller fix: TCP commands for streams so the existing Go/Node SDKs work,
  plus consumer-group partition assignment.

Also note: the default ACL only covers `chat.*`, `presence.*`, `typing.*` and
`system.*`. A custom event namespace such as `match.*` is **default-denied**
until you add rules.

---

## 7. Open decision: which Raft group carries stream metadata

Automatic failover is tractable — more so than first assumed — because the
consensus layer already exists. `internal/cluster` provides `Apply`,
`IsLeader`, `VerifyLeader`, `LeaderID`, `Join`/`Leave`.

The design that works is **Raft for metadata, not for data**:

```
Raft group           →  low frequency, a few bytes
  "chat.direct.x/3 → node-b, epoch 7"

Existing replication →  high frequency, no consensus
  leader → follower streaming, quorum acks
```

This is Kafka's model: KRaft/ZooKeeper holds metadata and elects leaders; a
separate fetch protocol moves data. Pushing the log itself through Raft would
serialise every conversation through one consensus stream and destroy partition
parallelism — that is the mistake to avoid.

Work required: three command types (`AssignPartition`, `FenceEpoch`,
`ReportHealth`), an FSM branch, a leadership-change observer, and epoch fencing
in the replication wire protocol. Roughly 800–1200 lines with tests.

**Epoch fencing is the part that is easy to get wrong.** A node that led
partition X, was network-partitioned, and returns must not accept writes. Raft
supplies the term; the assignment must carry an epoch, followers must reject
records from a stale epoch, and the leader must `VerifyLeader()` before
assigning sequences.

### The decision still to make

The existing Raft group **also carries every queue publish and ack** —
`CmdRaftPublish` and `CmdRaftAck` are Raft log entries. Putting stream metadata
in the same FSM means a leadership change queues behind queue traffic: slowest
exactly when it matters most.

| Option | Trade |
|---|---|
| **A — separate Raft group for metadata** (own port, own dir) | Clean isolation; one more cluster to operate |
| **B — reuse the existing group** | Less work; metadata couples to queue throughput. Acceptable if the queue plane is lightly used, which it is for a chat-only deployment |

Not yet decided. Whoever picks one, record the reason here.

---

## 8. Suggested order of work

1. **Replicate cursors** (§3.1) — without it a failover still hurts users.
2. **Wildcard topic assignment** (§3.2) — without it new conversations are
   unprotected. This is a live data-loss exposure, not a nicety.
3. **Automatic failover** (§7) — after the decision in §7 is made.
4. **Gateway leader redirect** (§2b) — so clients find the right node the way
   queue clients already do.
5. **Stream publish over the network** (§6) — unblocks non-chat event use.
6. **Chaos testing** — network partition, disk full, kill mid-write. None of
   these has been exercised; the tests cover logic, not environment failure.

Items 1 and 2 are what currently stop replication from being genuinely useful.

---

## 9. Honest limits of the test suite

The suite is large and race-clean, and it has caught real bugs. It has not
proven the system correct.

- Roughly 100 statements in `stream` remain uncovered; almost all are
  `if err != nil` on syscalls that only fail on real I/O errors. Covering them
  needs either a filesystem interface in production code (indirection added
  purely for tests) or a small tmpfs filled to `ENOSPC` in CI. Neither is done.
- The quorum bug in §2 sat on a line that already had 100% coverage. Coverage
  measures execution, not correctness.
- Nothing has been run under network partition, disk exhaustion, or realistic
  connection counts.

Treat the numbers as a floor, not a guarantee.

---

## 10. Known-failing tests that predate this work

Three failures exist at commit `1cb48ce`, before any messaging code, and are
unrelated to it:

- `internal/api` — `TestMessagingEndpointsRemoved` hangs. It asserts `/consume`
  returns 404, but the route is still registered and the handler blocks.
- `internal/cluster` — times out on Raft transport.
- `test/integration` — `TestSDKEndToEnd` fails on a port collision (`9095`).

`9095` is also the gateway port used in these docs; pick another if you fix that
integration test.
