# BoltQ Architecture

## Overview

BoltQ is a high-performance, memory-first message queue server written in Go with zero external dependencies. It supports both Work Queue and Pub/Sub messaging patterns with optional disk persistence.

## System Architecture

```
┌─────────────────────────────────────────────────────────────┐
│                        BoltQ Server                         │
│                                                             │
│  ┌──────────┐    ┌───────────┐    ┌──────────────────────┐  │
│  │ HTTP API │───▶│  Broker   │───▶│   Queue Engine       │  │
│  │ (REST)   │    │           │    │   (Ring Buffer)      │  │
│  └──────────┘    │  ┌──────┐ │    └──────────────────────┘  │
│                  │  │ Work │ │                               │
│  ┌──────────┐    │  │Queue │ │    ┌──────────────────────┐  │
│  │  gRPC    │───▶│  ├──────┤ │───▶│   Storage Engine     │  │
│  │  (TBD)   │    │  │Pub/  │ │    │   ┌──────┐ ┌──────┐ │  │
│  └──────────┘    │  │Sub   │ │    │   │Memory│ │ Disk │ │  │
│                  │  ├──────┤ │    │   │      │ │ (WAL)│ │  │
│  ┌──────────┐    │  │Dead  │ │    │   └──────┘ └──────┘ │  │
│  │ Metrics  │    │  │Letter│ │    └──────────────────────┘  │
│  │Prometheus│    │  └──────┘ │                               │
│  └──────────┘    └───────────┘    ┌──────────────────────┐  │
│                       │           │   Scheduler          │  │
│                       └──────────▶│   - ACK Timeout      │  │
│                                   │   - Retry Handler    │  │
│                                   │   - Dead Letter      │  │
│                                   └──────────────────────┘  │
└─────────────────────────────────────────────────────────────┘
         ▲               ▲               ▲
         │               │               │
    ┌────┴───┐    ┌──────┴──┐    ┌───────┴───┐
    │Go SDK  │    │Node SDK │    │  CLI Tool  │
    └────────┘    └─────────┘    └───────────┘
```

## Core Components

### 1. Queue Engine (`internal/queue/`)

Lock-free ring buffer implementation optimized for high throughput.

- **Data structure**: Power-of-two sized ring buffer using bitwise masking
- **Capacity**: Default 1M messages (1,048,576), configurable
- **Operations**: `Push()`, `Pop()` (blocking), `TryPop()` (non-blocking)
- **Concurrency**: Mutex-protected with `sync.Cond` for blocking waits
- **Memory**: Zero allocations on push/pop operations (0 B/op in benchmarks)

```
Ring Buffer Layout:
┌───┬───┬───┬───┬───┬───┬───┬───┐
│ 0 │ 1 │ 2 │ 3 │ 4 │ 5 │ 6 │ 7 │  capacity = 8 (power of 2)
└───┴───┴───┴───┴───┴───┴───┴───┘  mask = 7 (capacity - 1)
      ▲               ▲
     tail            head
   (read)          (write)

Position = counter & mask  (bitwise AND, no modulo)
```

### 2. Broker (`internal/broker/`)

Central message routing engine that manages both Work Queue and Pub/Sub patterns.

**Work Queue Mode:**
- 1 message → 1 consumer (round-robin)
- Messages stored in ring buffer queues
- Consumer must ACK within timeout
- Failed messages go through retry → dead letter pipeline

**Pub/Sub Mode:**
- 1 message → all subscribers
- Each subscriber gets a buffered channel (default 256)
- Slow subscribers may have messages dropped (backpressure)

**Pending Message Tracking:**
- Every consumed message enters the `pending` map
- Tracked with `AckDeadline` timestamp
- Scheduler checks for timeouts and requeues

### 3. Storage Engine (`internal/storage/`)

Two storage backends behind a common `Storage` interface:

**Memory Storage** (default):
- Messages exist only in the ring buffer
- Fastest mode, no disk I/O
- Data lost on server restart

**Disk Storage** (WAL-based):
- Write-Ahead Log for crash recovery
- Automatic background compaction (Checkpointing)
- Append-only binary format with CRC32 checksums
- Buffered I/O (64KB write buffer)
- Recovery: replay WAL on startup to rebuild queues

### 4. WAL (`internal/wal/`)

Binary append-only log for persistence.

```
WAL Record Format:
┌──────────┬──────────┬──────────────────┐
│ Length   │ CRC32    │ Data (JSON)      │
│ 4 bytes  │ 4 bytes  │ variable length  │
│ (LE u32) │ (LE u32) │                  │
└──────────┴──────────┴──────────────────┘
```

- **Integrity**: CRC32 checksum per record, corrupted records stop recovery
- **Sync**: Configurable flush-to-disk via `Sync()`
- **Compaction**: Automatic threshold-based rewrite removes processed messages

### 5. Scheduler (`internal/scheduler/`)

Background goroutine that runs periodic checks:

- **ACK Timeout Watcher**: Scans pending messages every 1s (configurable). If `AckDeadline` passed, requeues the message with retry increment.
- **Retry Flow**: Message retry count incremented → if exceeds `MaxRetry`, moved to dead letter queue.

### 6. HTTP API (`internal/api/`)

Standard library `net/http` server (zero framework dependencies).

- RESTful JSON endpoints
- Optional API key authentication via `X-API-Key` header
- SSE (Server-Sent Events) for pub/sub streaming
- Prometheus metrics endpoint
- Request size limit: 1MB

### 7. Metrics (`internal/metrics/`)

Lock-free atomic counters for real-time metrics:

- `messages_published` - Total published messages
- `messages_consumed` - Total consumed messages
- `messages_acked` - Total ACK'd messages
- `messages_nacked` - Total NACK'd messages
- `retry_count` - Total retries
- `dead_letter_count` - Total dead-lettered messages

Exposed in both Prometheus text format and JSON.

## Message Lifecycle

```
Producer                    Broker                     Consumer
   │                          │                           │
   │──── Publish ────────────▶│                           │
   │                          │── Store (WAL if disk) ──▶│
   │                          │── Push to Ring Buffer ──▶│
   │                          │                           │
   │                          │◀── Consume ──────────────│
   │                          │── Track Pending ────────▶│
   │                          │                           │
   │                          │◀── ACK ─────────────────│
   │                          │── Remove from Pending ──▶│
   │                          │                           │

On NACK or Timeout:
   │                          │                           │
   │                          │── Increment Retry ──────▶│
   │                          │                           │
   │                     retry <= max?                    │
   │                     ┌─── Yes ──┐                    │
   │                     │          │                    │
   │                     ▼          ▼                    │
   │              Push back to   Move to Dead            │
   │              Ring Buffer    Letter Queue             │
```

## Concurrency Model

BoltQ uses Go's concurrency primitives:

| Component | Mechanism | Purpose |
|-----------|-----------|---------|
| Queue | `sync.Mutex` + `sync.Cond` | Protect ring buffer, signal consumers |
| Broker | `sync.RWMutex` (queues, topics) | Concurrent read, exclusive write |
| Pending | `sync.RWMutex` | Track ACK state |
| Metrics | `sync/atomic` | Lock-free counter increments |
| WAL | `sync.Mutex` | Serialize disk writes |
| Scheduler | goroutine + ticker | Background periodic checks |
| Pub/Sub | buffered channels | Fan-out to subscribers |

## Performance Characteristics

| Operation | Latency | Throughput | Allocations |
|-----------|---------|------------|-------------|
| Queue Push | ~52ns | ~19M ops/sec | 0 B/op |
| Queue Pop | ~44ns | ~54M ops/sec | 0 B/op |
| Broker Publish | ~2.1μs | ~470K ops/sec | 3 allocs |
| Broker End-to-End | ~4.3μs | ~324K ops/sec | 4 allocs |
| WAL Write | ~2.4μs | ~430K ops/sec | 2 allocs |
| HTTP Publish | ~7.7μs | ~157K ops/sec | 38 allocs |

## Design Decisions

1. **Ring buffer over channels**: Go channels add overhead from goroutine scheduling. Ring buffer with mutex gives lower latency and zero allocations.

2. **Power-of-two sizing**: Allows bitwise AND masking (`pos & mask`) instead of modulo (`pos % size`), saving CPU cycles per operation.

3. **WAL over LSM/B-tree**: Append-only log is the simplest and fastest persistence model for a queue workload where sequential writes dominate.

4. **CRC32 per record**: Detects corruption at record granularity. Recovery stops at first corrupted record (tail corruption is acceptable).

5. **Buffered WAL writes**: 64KB write buffer amortizes syscall overhead. Trade-off: up to 64KB of recent data may be lost on crash without explicit `Sync()`.

6. **Standard library only**: Zero external dependencies reduces supply chain risk and simplifies deployment. No framework overhead for HTTP serving.
