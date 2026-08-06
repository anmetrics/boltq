# BoltQ Documentation

BoltQ is two systems that share a process.

**A work queue and pub/sub broker** — the original BoltQ. Trusted backend
clients connect over a binary TCP protocol, push jobs, consume them, ack them.
Raft replicates the queue state.

**A messaging backbone** — a partitioned, replayable log with per-user
authorisation and a WebSocket edge, built for end-user devices: chat, direct
messages, presence, typing indicators, offline push.

The two are independent. You can run either alone or both together; enabling
one does not change the behaviour of the other. The messaging subsystem is
**off by default**.

> **Running the messaging subsystem in production?** Start with
> **[STATUS.md](STATUS.md)** — the honest inventory of what is verified, which
> gaps cost data today, and which design decisions are still open.

---

## Part 1 — Queue and pub/sub

1. **[Getting Started](./getting-started.md)** — Quick start guide, installation, first message
2. **[Architecture](./architecture.md)** — System design, components, data flow, concurrency model
3. **[API Reference](./api-reference.md)** — Full HTTP REST API documentation
4. **[Configuration](./configuration.md)** — Config file, environment variables, tuning guide
5. **[Persistence & WAL](./persistence.md)** — Write-Ahead Log, disk mode, recovery process
6. **[Monitoring](./monitoring.md)** — Prometheus metrics, Grafana dashboards, alerting
7. **[Go SDK](./sdk-go.md)** — Go client library reference and examples
8. **[Node.js SDK](./sdk-nodejs.md)** — Node.js client library reference and examples
9. **[CLI Reference](./cli.md)** — Command-line tool usage
10. **[Deployment](./deployment.md)** — Docker, Kubernetes, systemd, Nginx, production checklist
11. **[Security](./security.md)** — Security features, API key auth, TLS encryption

---

## Part 2 — Messaging (chat, presence, push)

### Start here

| If you want to… | Read |
|---|---|
| Understand why this subsystem exists at all | [Why a log](architecture/why-a-log.md) |
| See how a message travels from one phone to another | [Message lifecycle](architecture/message-lifecycle.md) |
| Build a chat app on BoltQ | [Building a chat app](guides/building-a-chat-app.md) |
| Wire up authentication | [Authentication and authorisation](guides/auth.md) |
| Connect a client | [Gateway protocol](reference/gateway-protocol.md) |
| Run this in production | [Production checklist](operations/production-checklist.md) |
| Plan for multiple regions | [Global HA](operations/global-ha.md) |
| Upgrade an existing queue deployment | [Migrating from queues](guides/migrating-from-queues.md) |

### Architecture

- **[Why a log](architecture/why-a-log.md)** — why chat needs a replayable log
  rather than a queue, and what that changes.
- **[The stream engine](architecture/stream-engine.md)** — segments, sparse
  indexes, sequence assignment, retention, crash recovery.
- **[Message lifecycle](architecture/message-lifecycle.md)** — the full path of
  a message, including what happens when the recipient is offline.
- **[Fan-out strategies](architecture/fanout.md)** — why a 3-person chat and a
  50,000-person channel are delivered differently.
- **[Cursors and multi-device](architecture/cursors.md)** — how four devices
  belonging to one person each keep their own read position.
- **[Replication](architecture/replication.md)** — how the log is copied to
  other nodes, quorum acknowledgement, and manual failover.
- **[Durability](architecture/durability.md)** — exactly what survives a crash,
  what does not, and why the defaults are what they are.
- **[Ordering guarantees](architecture/ordering.md)** — what BoltQ promises
  about message order, and what it does not.

### Guides

- **[Building a chat app](guides/building-a-chat-app.md)** — end to end, from
  configuration to a working client.
- **[Authentication and authorisation](guides/auth.md)** — tokens, scopes, the
  ACL, and the membership service you must provide.
- **[Presence and typing](guides/presence-and-typing.md)** — the ephemeral
  path, and why it is deliberately unreliable.
- **[Offline push](guides/offline-push.md)** — the notification webhook.
- **[Migrating from queues](guides/migrating-from-queues.md)** — for existing
  BoltQ deployments.

### Reference

- **[Gateway protocol](reference/gateway-protocol.md)** — every WebSocket frame.
- **[Configuration](reference/configuration.md)** — every messaging option.
- **[Admin API](reference/admin-api.md)** — the messaging HTTP endpoints.
- **[Topic conventions](reference/topics.md)** — the topic namespace.

### Operations

- **[Production checklist](operations/production-checklist.md)** — what to do
  before taking real traffic.
- **[Capacity planning](operations/capacity.md)** — sizing, and where the
  limits actually are.
- **[Global HA](operations/global-ha.md)** — multi-region deployment, and an
  honest account of what BoltQ does not yet do.
- **[Monitoring](operations/monitoring.md)** — what to alert on.

---

## A note on scope

**[STATUS.md](STATUS.md)** is the single place that records open work, bugs
already found and fixed, and undecided design questions. Keep it current — it
exists so nobody has to re-derive these conclusions later.

The Part 2 documents try to be honest about limits. Several things a
global-scale messaging system eventually needs are **not** implemented —
automatic leader election and failover, resharding, tiered storage to object
storage, and follower reads. Where that is the case the documents say so plainly
and describe what you would have to build or buy instead, rather than implying
coverage that does not exist. See [Global HA](operations/global-ha.md) for the
full list.
