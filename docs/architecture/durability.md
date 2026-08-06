# Durability

This document states exactly what survives a crash and what does not. It is the
document to read before deciding whether BoltQ's defaults are acceptable for
your data.

## The short version

| Data | Default behaviour | Survives process crash | Survives machine/power loss |
|---|---|---|---|
| Message records | buffered write, periodic fsync | **Yes** | **Only if fsynced** |
| Message records with `sync_on_append` | fsync per append | Yes | Yes |
| Sealed segments | fsynced at roll | Yes | Yes |
| Reader cursors | buffered, flushed every 1s | Yes | Last ~1s may be lost |
| Replicated records (`min_in_sync` ≥ 2) | acknowledged by N nodes | **Yes** | **Yes, unless all replicas die together** |
| Presence sessions | in memory only | No | No |
| Ephemeral signals | in memory only | No | No |
| Dedup claims | in memory only | No | No |
| Gateway sessions | in memory only | No | No |

The distinction between "process crash" and "power loss" is the whole subject.
A buffered write that has reached the operating system's page cache survives
the process dying, because the kernel still holds it and will write it out. It
does **not** survive the machine losing power, because the page cache is RAM.

## Message records

By default an append is a buffered write into a 256KB buffer, flushed to the
page cache when the buffer fills or when a reader forces a flush. The active
segment is fsynced on the maintenance interval (default 30s) and unconditionally
when a segment is sealed.

This means: **with default settings, a message acknowledged to a client can be
lost if the machine loses power within the fsync window.**

That is a deliberate default, and here is the reasoning. An fsync costs roughly
100µs on NVMe and around 1ms on anything slower. At the message rates a chat
app reaches, paying it per append caps a partition at low thousands of messages
per second and makes send latency dominated by disk. The alternative — accept
the window and make the machine's failure survivable by having the data
somewhere else — is what every large messaging system does.

**The stream log can now be replicated.** With
[replication](replication.md) enabled and `min_in_sync: 2`, an acknowledged
message is held by at least two nodes before the publisher is told it was sent
— which is the usual and better mitigation, since it survives losing a machine
outright rather than only surviving a process crash.

Your options, in order of preference:

1. **Enable replication with `min_in_sync: 2`.** An acknowledged message then
   survives the loss of one node. Combine with `sync_on_apply` on the follower
   so its acknowledgement means "on disk", not "in page cache".

2. **Turn on `sync_on_append`** and accept the throughput cost. Correct for a
   single-node deployment; measure before assuming it is too slow, since a
   dating app at launch scale writes far fewer messages per second than the
   fsync ceiling.

3. **Run on storage with a battery-backed write cache** (most cloud block
   storage, e.g. EBS, GCP PD) where a power loss to the instance does not lose
   acknowledged writes to the volume. This makes the default safe against
   instance failure, though not against volume corruption.

4. **Shorten `maintenance_interval`** to narrow the window. This reduces
   exposure without eliminating it, and costs an fsync per interval rather than
   per append.

5. **Accept the window** for message classes where it genuinely does not matter.

Configure it:

```json
{
  "messaging": {
    "stream": {
      "sync_on_append": true,
      "maintenance_interval": "5s"
    },
    "replication": {
      "enabled": true,
      "role": "leader",
      "listen": "10.0.0.1:9200",
      "min_in_sync": 2
    }
  }
}
```

## What a torn write does

An unclean shutdown typically leaves a partial record at the end of the active
segment. On restart, recovery scans forward, and the first record that fails
its CRC ends the scan; the file is truncated there.

This is safe because a record only reaches the client's "sent" response after
the append call returns. A record lost to truncation was never acknowledged, so
no client believes it exists. The system never presents a message as sent and
then loses it *through this path* — the fsync window above is the path that can
do that.

Recovery is exercised by `TestRecoveryTruncatesTornWrite`, which appends
deliberate garbage and asserts the intact prefix survives and sequence numbering
continues correctly.

## Cursors are not fsynced

Cursor commits are buffered and flushed every second, not fsynced.

Losing the last second of cursor commits means a device re-receives a handful of
messages it already had. Client-side deduplication by message ID — which every
chat client needs anyway, because at-least-once delivery is unavoidable across a
reconnect — already handles this.

Paying an fsync per commit to avoid a benign redelivery would be the wrong
trade, especially since commits are the highest-frequency write in the system.

Cursors **are** fsynced on graceful shutdown, because there the loss is
avoidable and costs users visible redelivery.

## In-memory state

Presence, ephemeral signals, dedup claims and gateway sessions are all in
memory and all lost on restart. Each is the correct choice, for different
reasons:

**Presence** is inherently ephemeral — after a restart no client is connected
anyway, so a persisted "alice is online" record would be actively wrong. Clients
reconnect and re-bind.

**Ephemeral signals** are worthless a second after they are sent. A typing
indicator recovered from before a crash is noise.

**Dedup claims** are lost, which means a retry arriving across a restart could
duplicate a message. The window is the restart itself. Persisting them would put
a durable write on the hot path of every send to protect against a rare and
mild failure; client-side dedup by `client_msg_id` closes it.

**Gateway sessions** are lost, so a resume across a restart fails and the client
gets a fresh session. It then subscribes normally and resumes from its
**committed cursor**, which *is* durable — so no messages are missed, only the
round trips saved by resume.

## Multi-node

**The stream log can be replicated** — see [Replication](replication.md). With
`min_in_sync: 2`, an acknowledged message exists on two nodes before the
publisher hears about it, and the loss of one machine loses nothing.

What replication does **not** yet give you:

- **No automatic failover.** Leadership is configuration. If the leader dies,
  followers keep the data and stop advancing; promotion is a deliberate
  operator action with a written procedure.
- **No follower reads.** A follower's copy exists for durability and promotion,
  not for scaling reads. Clients must reach the leader.
- **No cross-region tuning.** Synchronous quorum across regions adds the round
  trip to every send.

With replication **off** (the default), a node's messages live only on that
node's disk, and backups are your only durability story for node loss.

Additional mitigations:

- **Filesystem/volume snapshots** on a schedule. Crash-consistent snapshots are
  safe to restore from because recovery handles torn tails.
- **A replicated block device** (cloud volumes with cross-AZ durability).
- **Pin conversations to nodes** with a sticky router and accept that a node
  outage means those conversations are unavailable until it returns.

See [Global HA](../operations/global-ha.md) for the full picture and what
building replication would involve.

## Verifying your setup

After configuring, verify rather than assume:

```bash
# 1. Send messages, note the last sequence.
# 2. Kill -9 the process.
kill -9 $(pgrep boltq-server)
# 3. Restart and read back.
curl 'localhost:9090/streams/topic?name=chat.direct.alice:bob'
```

Compare `next_seq` to what you sent. With `sync_on_append` it should match
exactly. Without it, expect to lose whatever was inside the fsync window — and
if that surprises you, this document has done its job.

## Further reading

- [The stream engine](stream-engine.md) — how recovery works.
- [Global HA](../operations/global-ha.md) — the replication gap.
- [Production checklist](../operations/production-checklist.md).
