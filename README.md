# BoltQ

High-performance message queue server written in Go. Zero external dependencies.

## Features

- **Work Queue**: 1 message → 1 consumer (like RabbitMQ)
- **Pub/Sub**: 1 message → all subscribers (like Redis PubSub)
- **TCP Binary Protocol**: Low-latency binary framing protocol (port 9091)
- **HTTP REST API**: JSON-based REST interface (port 9090)
- **ACK/NACK**: Consumer acknowledgment with timeout
- **Retry**: Exponential retry with configurable max retries
- **Dead Letter Queue**: Failed messages after max retries
- **WAL Persistence**: Write-Ahead Log for crash recovery
- **Prometheus Metrics**: `/metrics` endpoint
- **API Key Auth**: Optional authentication
- **Lock-free ring buffer**: 1M+ message capacity

## Performance Targets

| Metric | Target |
|--------|--------|
| Publish latency | < 1ms |
| Throughput | 200k msg/sec |
| Memory footprint | < 50MB |
| Startup time | < 1s |

## Quick Start

```bash
# Build
make build

# Run server (starts HTTP on :9090 and TCP on :9091)
./bin/boltq-server

# With config
./bin/boltq-server -config configs/default.json

# With Docker
docker build -t boltq .
docker run -p 9090:9090 -p 9091:9091 boltq
```

## TCP Protocol

BoltQ supports a binary TCP protocol for lower latency and higher throughput compared to HTTP.

### Wire Format

```
[command: 1 byte][length: 4 bytes LE][payload: N bytes JSON]
```

Max frame size: 4MB.

### Commands

| Command | Byte | Description |
|---------|------|-------------|
| PUBLISH | `0x01` | Publish to work queue |
| PUBLISH_TOPIC | `0x02` | Publish to pub/sub topic |
| CONSUME | `0x03` | Consume from queue |
| ACK | `0x04` | Acknowledge message |
| NACK | `0x05` | Negative acknowledge (retry) |
| PING | `0x06` | Health check |
| STATS | `0x07` | Get queue statistics |
| AUTH | `0x08` | Authenticate with API key |

### Response Status

| Status | Byte | Description |
|--------|------|-------------|
| OK | `0x00` | Success |
| ERROR | `0x01` | Error (payload contains JSON error) |
| EMPTY | `0x02` | No message available |

### Go TCP Client

```go
import boltq "github.com/boltq/boltq/client/golang"

// Connect via TCP
client := boltq.New("localhost:9091")
if err := client.Connect(); err != nil {
    log.Fatal(err)
}
defer client.Close()

// Publish to work queue
id, err := client.Publish("email_jobs", map[string]interface{}{
    "to": "user@example.com",
}, nil)

// Consume
msg, err := client.Consume("email_jobs")
if msg != nil {
    client.Ack(msg.ID)
}

// Ping
err = client.Ping()

// Stats
stats, err := client.Stats()
```

### Client Options

```go
// With API key authentication
client := boltq.New("localhost:9091", boltq.WithAPIKey("your-secret-key"))

// With custom timeout
client := boltq.New("localhost:9091", boltq.WithTimeout(5 * time.Second))
```

## HTTP API

### Publish (Work Queue)

```bash
curl -X POST http://localhost:9090/publish \
  -H "Content-Type: application/json" \
  -d '{"topic":"email_jobs","payload":"hello","headers":{}}'
```

### Publish (Pub/Sub)

```bash
curl -X POST http://localhost:9090/publish/topic \
  -H "Content-Type: application/json" \
  -d '{"topic":"user_signup","payload":"event_data"}'
```

### Consume

```bash
curl http://localhost:9090/consume?topic=email_jobs
```

### ACK

```bash
curl -X POST http://localhost:9090/ack \
  -H "Content-Type: application/json" \
  -d '{"id":"message_id_here"}'
```

### NACK

```bash
curl -X POST http://localhost:9090/nack \
  -H "Content-Type: application/json" \
  -d '{"id":"message_id_here"}'
```

### Subscribe (SSE)

```bash
curl http://localhost:9090/subscribe?topic=events&id=my-subscriber
```

### Stats

```bash
curl http://localhost:9090/stats
```

### Metrics (Prometheus)

```bash
curl http://localhost:9090/metrics
```

### Health

```bash
curl http://localhost:9090/health
```

## Go Client SDK

```go
import boltq "github.com/boltq/boltq/client/golang"

// Connect via TCP (recommended for performance)
client := boltq.New("localhost:9091")
if err := client.Connect(); err != nil {
    log.Fatal(err)
}
defer client.Close()

// Publish
id, err := client.Publish("email_jobs", map[string]interface{}{"to": "user@example.com"}, nil)

// Consume
msg, err := client.Consume("email_jobs")

// ACK
err = client.Ack(msg.ID)

// NACK (retry)
err = client.Nack(msg.ID)
```

## CLI

```bash
# Set server URL (default: http://localhost:9090)
export BOLTQ_URL=http://localhost:9090

# Publish
boltq publish -topic email_jobs -payload '{"to":"user@example.com"}'

# Consume
boltq consume -topic email_jobs

# ACK/NACK
boltq ack -id <message_id>
boltq nack -id <message_id>

# Stats & Health
boltq stats
boltq health
```

## Configuration

JSON config file (`configs/default.json`):

```json
{
  "server": {
    "http_port": 9090,
    "tcp_port": 9091,
    "grpc_port": 9092,
    "host": "0.0.0.0"
  },
  "storage": {
    "mode": "memory",
    "data_dir": "./data"
  },
  "queue": {
    "max_retry": 5,
    "ack_timeout": "30s",
    "capacity": 1048576
  },
  "performance": {
    "worker_pool": 16
  },
  "security": {
    "api_key": ""
  }
}
```

### Environment Variables

| Variable | Description |
|----------|-------------|
| `BOLTQ_HTTP_PORT` | HTTP server port |
| `BOLTQ_TCP_PORT` | TCP server port |
| `BOLTQ_STORAGE_MODE` | `memory` or `disk` |
| `BOLTQ_DATA_DIR` | WAL data directory |
| `BOLTQ_API_KEY` | API key for auth |

## Storage Modes

### Memory (default)
Fastest mode. Messages stored in ring buffer only.

### Disk
Memory + WAL. Messages written to append-only log before memory queue. On restart, WAL is replayed to rebuild queues.

```
write message → append WAL → push memory queue

server restart → replay WAL → rebuild queue
```

## Architecture

```
boltq/
  cmd/
    server/     # Server entrypoint
    cli/        # CLI tool
  internal/
    broker/     # Message broker (work queue + pub/sub)
    queue/      # Lock-free ring buffer queue
    storage/    # Storage interface + implementations
    wal/        # Write-Ahead Log
    api/        # HTTP REST + TCP binary API
    scheduler/  # ACK timeout watcher
    config/     # Configuration
    metrics/    # Prometheus metrics
  client/
    golang/     # Go client SDK
  pkg/
    protocol/   # Message types
  configs/      # Config files
```

## Benchmarks

```bash
# Run all benchmarks
make bench

# Run specific benchmarks
make bench-queue
make bench-wal
make bench-broker
make bench-api
```

## Testing

```bash
make test
```

## License

MIT
