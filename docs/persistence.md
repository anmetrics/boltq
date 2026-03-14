# Persistence & WAL

BoltQ uses a Write-Ahead Log (WAL) for optional disk persistence, ensuring messages survive server crashes and restarts.

## Storage Modes

### Memory Mode (Default)

```json
{ "storage": { "mode": "memory" } }
```

- Messages exist only in the in-memory ring buffer
- No disk I/O — maximum performance
- All messages lost on server restart
- Best for: transient jobs, high-throughput non-critical workloads

### Disk Mode

```json
{ "storage": { "mode": "disk", "data_dir": "./data" } }
```

- Messages written to WAL **before** being pushed to memory
- On restart, WAL is replayed to rebuild queue state
- Best for: critical jobs, durability requirements

## Write Flow (Disk Mode)

```
Client sends message
       │
       ▼
┌──────────────┐
│ Encode JSON  │
└──────┬───────┘
       │
       ▼
┌──────────────┐
│ Append WAL   │◄── 1. Write [length][crc32][data] to buffer
│ (buffered)   │◄── 2. Flush buffer to disk
└──────┬───────┘
       │
       ▼
┌──────────────┐
│ Push to      │◄── 3. Message available for consumers
│ Ring Buffer  │
└──────────────┘
```

**Ordering guarantee**: WAL write completes **before** the message enters the ring buffer. If the server crashes between WAL write and ring buffer push, the message is still recoverable.

## WAL Format

The WAL is a single append-only file: `{data_dir}/queue.wal`

### Record Layout

```
┌──────────────┬──────────────┬────────────────────────┐
│   Length      │   CRC32      │   Data                 │
│   4 bytes     │   4 bytes    │   {Length} bytes       │
│   (LE uint32) │   (LE uint32)│   (JSON)              │
└──────────────┴──────────────┴────────────────────────┘
```

| Field | Size | Encoding | Description |
|-------|------|----------|-------------|
| Length | 4 bytes | Little-endian uint32 | Size of the data payload |
| CRC32 | 4 bytes | Little-endian uint32 | IEEE CRC32 checksum of data |
| Data | Variable | UTF-8 JSON | Serialized message |

### Example Binary Layout

For a message with JSON payload `{"id":"abc","topic":"test","payload":"hello","timestamp":123}` (60 bytes):

```
Offset  Bytes        Meaning
0x00    3C 00 00 00  Length = 60
0x04    A7 B2 C3 D4  CRC32 checksum
0x08    7B 22 69 64  {"id... (60 bytes of JSON)
0x44    ...          Next record starts here
```

## Recovery Process

On server startup with disk mode enabled:

```
Server starts
     │
     ▼
┌─────────────────┐
│ Open WAL file   │
└────────┬────────┘
         │
         ▼
┌─────────────────┐
│ Read records    │◄── Sequential scan from start
│ one by one      │
└────────┬────────┘
         │
    ┌────┴────┐
    │ Valid?  │◄── Check: length valid? CRC32 matches?
    └────┬────┘
    Yes  │  No
    │    │  └──▶ Stop recovery (truncated/corrupted tail)
    │    │
    ▼    │
┌────────┴────────┐
│ Decode message  │
│ Push to queue   │◄── Rebuild in-memory ring buffer
└────────┬────────┘
         │
         ▼
    (next record)
```

**Recovery behavior:**
- Valid records are replayed in order
- First corrupted/truncated record stops recovery
- Records after corruption are **not** recovered (conservative approach)
- Malformed JSON within a valid record is skipped

### Recovery Log Example

```
[server] storage mode: disk (dir=./data)
[server] recovering 1523 messages from WAL
[server] BoltQ started on 0.0.0.0:9090
```

## Buffered I/O

WAL writes use a **64KB write buffer** (`bufio.Writer`) to reduce syscall overhead:

```
Write message ──▶ Buffer (64KB) ──▶ Flush() ──▶ OS Page Cache ──▶ Disk
```

- Each `Write()` call flushes the buffer immediately after writing
- This ensures durability at the cost of one `write()` syscall per message
- The 64KB buffer amortizes the overhead when messages are small

### Sync Guarantees

| Operation | Guarantee |
|-----------|-----------|
| `Write()` | Data in OS page cache (survives process crash) |
| `Sync()` | Data on physical disk (survives power loss) |

By default, `Write()` flushes to the OS but does not call `fsync()`. For maximum durability at the cost of performance, call `Sync()` periodically.

## WAL Compaction

The WAL file can be compacted to remove acknowledged messages and reclaim disk space. BoltQ supports both manual and automatic compaction.

### Automatic Compaction

Triggers automatically when the WAL file size exceeds a configurable threshold.

- **Config**: `storage.compaction_threshold` (default: 100MB)
- **Mechanism**: The broker captures a consistent snapshot of all active messages and rewrites a clean WAL file.
- **Background operation**: Compaction runs asynchronously to avoid blocking the message processing pipeline.

### Manual Compaction

Can be triggered programmatically or via administrative actions:

```go
broker.Checkpoint() // Snapshots current state and rewrites WAL
```

### Benefits

- **Reduced Disk Usage**: Reclaims space immediately after significant message consumption.
- **Faster Recovery**: A smaller, compacted WAL means the server restarts and replays messages much faster.
- **Consistent State**: Only unacknowledged messages are persisted, preventing unnecessary replays.

## Data Directory Layout

```
data/
└── queue.wal     # Append-only WAL file
```

## Durability Trade-offs

| Mode | Crash Safety | Performance | Use Case |
|------|-------------|-------------|----------|
| Memory | None | ~52ns push | Dev, non-critical |
| Disk (no sync) | Process crash safe | ~2.4μs push | Production default |
| Disk (with sync) | Power loss safe | ~50μs+ push | Critical data |

## Limitations

1. **No per-topic WAL**: All topics share one WAL file. Recovery replays all messages to all queues.
2. **Sequential Rewrite**: Compaction requires rewriting the entire WAL (though done in background).
3. **No checksumming of WAL file itself**: Only individual records are checksummed.
4. **Recovery replays active messages**: Only messages present in the WAL after compaction (or appended since) are replayed. Consumers should still be idempotent.
5. **Single WAL file**: No segment rotation. Very large WAL files (before compaction) may slow recovery.
