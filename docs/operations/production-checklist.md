# Production checklist

Work through this before taking real traffic. Items marked **critical** cause
data loss or a security hole if skipped.

## Security

- [ ] **critical** — The shared API key is never shipped to a device. A client
      authenticated with it becomes an anonymous principal and bypasses the ACL
      entirely when `allow_anonymous` is on.
- [ ] **critical** — `messaging.identity.enabled: true`. The gateway refuses to
      start otherwise, but verify it is on for the reason, not by accident.
- [ ] **critical** — Signing keys come from `secret_env`, not `secret`. A secret
      in a config file gets committed, or shipped in an image, or both.
- [ ] Signing keys are ≥ 32 bytes of real entropy (`openssl rand -hex 32`).
- [ ] Token lifetime ≤ 1 hour. Revocation is per-node and in-memory; short
      lifetimes are the actual control.
- [ ] `issuer` is set and matches your auth service.
- [ ] TLS terminates in front of, or at, the gateway. End-user traffic must be
      encrypted.
- [ ] `allowed_origins` is set if browsers connect. Empty allows every origin.
- [ ] The gateway is on its **own port**, not the admin port.
- [ ] The admin port is not publicly reachable.
- [ ] The membership endpoint requires authentication (`membership_auth_header`).
- [ ] A key rotation procedure is written down and has been rehearsed once.

## Durability

- [ ] **critical** — A conscious decision about `sync_on_append`. Read
      [Durability](../architecture/durability.md). The stream log is **not
      replicated**, so fsync or storage-layer replication is your only
      protection against losing acknowledged messages.
- [ ] **critical** — Backups exist and a restore has been tested. This is your
      durability story for node loss; an untested backup is not a backup.
- [ ] Storage is a replicated volume (EBS, PD) rather than instance-local disk.
- [ ] `maintenance_interval` is short enough that the fsync window is
      acceptable, if `sync_on_append` is off.
- [ ] Snapshot schedule matches your acceptable data loss.

## Capacity

- [ ] **critical** — A retention policy is chosen. Both `retention_bytes` and
      `retention_age` default to **unlimited**; disk grows forever otherwise.
- [ ] Disk usage is alerted on well before full.
- [ ] `default_partitions` and `conversation_partitions` are chosen with
      headroom. **They cannot be changed after topics are created.**
- [ ] File descriptor limits are raised — two per segment plus one per
      connection.
- [ ] `max_subscriptions` matches how many conversations a client keeps open.
- [ ] Load has been tested at your expected peak, not just at idle.

## Correctness

- [ ] Device IDs are stable across app restarts and reinstalls. An unstable
      device ID resets read positions and orphans a cursor per install.
- [ ] Conversation IDs for 1:1 are **sorted** before joining, so both users
      derive the same ID.
- [ ] Clients always send `client_msg_id` on `send`.
- [ ] Clients sort by `seq`, never by timestamp or arrival order.
- [ ] Clients deduplicate by `message_id`.
- [ ] Clients handle the `gap` frame by showing a history-unavailable marker.
- [ ] Clients commit cursors after processing, batched, as `lastSeq + 1`.
- [ ] Reconnect uses exponential backoff **with jitter**. Without jitter every
      client reconnects in lockstep after an outage and takes the server down
      again.
- [ ] The push webhook is idempotent — dispatch is at-least-once.
- [ ] The push webhook returns non-2xx on failure so BoltQ retries.

## Operations

- [ ] Monitoring is in place — see [Monitoring](monitoring.md).
- [ ] Alerts on push dispatcher lag, disk usage, and node liveness.
- [ ] `/metrics` is scraped.
- [ ] Log aggregation captures the `[gateway]`, `[messaging]` and `[outbox]`
      prefixes.
- [ ] A runbook exists for node loss. There is **no automatic failover** for the
      messaging plane.
- [ ] Graceful shutdown is used (SIGTERM, not SIGKILL) so cursors are fsynced.
- [ ] Deployment does not run two instances against the same data directory.

## Known limitations you are accepting

Confirm each of these is acceptable, because none of them are configurable
away:

- [ ] The stream log is **not replicated**. Node loss with disk loss means
      message loss. See [Global HA](global-ha.md).
- [ ] There is **no cross-node read path**. A client must connect to the node
      holding its conversations.
- [ ] Presence is **per node**. A sharded deployment needs an external presence
      store.
- [ ] There is **no resharding**. Partition counts are fixed at creation.
- [ ] There is **no tiered storage**. History lives on local disk until
      retention removes it.
- [ ] Deletion is **segment-granular**. There is no "delete message N". Per-
      message deletion must be a client-honoured tombstone, and legal erasure
      must be crypto-shredding.
- [ ] Dedup claims are **in memory**. A restart reopens a brief duplicate window
      that client-side dedup must close.

## Smoke test

```bash
# Everything up?
curl -s localhost:9090/health
curl -s localhost:9090/messaging/overview | jq

# Send a message end to end, then confirm it landed.
curl -s 'localhost:9090/streams/topic?name=chat.direct.alice:bob' | jq

# Push keeping up? 'lag' should be near zero.
curl -s 'localhost:9090/streams/cursors?topic=chat.inbox.bob&group=push-dispatcher' | jq

# Kill -9 and restart; next_seq must match what you sent.
```

The last one is the test people skip and then regret. Run it once against your
actual configuration before launch — it is the only way to know whether your
`sync_on_append` decision is what you think it is.

## Further reading

- [Durability](../architecture/durability.md)
- [Capacity planning](capacity.md)
- [Global HA](global-ha.md)
- [Monitoring](monitoring.md)
