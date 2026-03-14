# Getting Started

## Prerequisites

- Go 1.21 or later
- (Optional) Docker for containerized deployment

## Installation

### From Source

```bash
git clone https://github.com/boltq/boltq.git
cd boltq
make build
```

This produces two binaries in `bin/`:
- `boltq-server` — The queue server
- `boltq` — CLI client tool

### With Docker

```bash
docker build -t boltq .
docker run -p 9090:9090 -p 9091:9091 boltq
```

## Quick Start

### 1. Start the Server

**Memory mode** (default, fastest):
```bash
./bin/boltq-server
```

**Disk mode** (WAL persistence):
```bash
BOLTQ_STORAGE_MODE=disk BOLTQ_DATA_DIR=./data ./bin/boltq-server
```

> [!TIP]
> In disk mode, BoltQ automatically compacts the WAL to reclaim space. You can tune this via `BOLTQ_STORAGE_COMPACTION_THRESHOLD` (default: 100MB).

**With config file**:
```bash
./bin/boltq-server -config configs/default.json
```

The server starts with TCP messaging on `:9091` and HTTP admin on `:9090`.

### 2. Publish a Message (via Go SDK)

All messaging operations use the TCP binary protocol:

```go
import boltq "github.com/boltq/boltq/client/golang"

client := boltq.New("localhost:9091")
if err := client.Connect(); err != nil {
    log.Fatal(err)
}
defer client.Close()

id, err := client.Publish("email_jobs", map[string]string{
    "to":      "user@example.com",
    "subject": "Welcome",
}, nil)
fmt.Println("Published:", id)
```

### 3. Consume and ACK (via CLI)

```bash
# Set TCP server address (default: localhost:9091)
export BOLTQ_ADDR=localhost:9091

# Publish
./bin/boltq publish -topic email_jobs -payload '{"to":"user@example.com"}'

# Consume
./bin/boltq consume -topic email_jobs

# ACK
./bin/boltq ack -id <message_id>

# NACK (triggers retry)
./bin/boltq nack -id <message_id>
```

### 4. Check Server Status (HTTP Admin)

```bash
# Health check
curl http://localhost:9090/health

# Broker statistics
curl http://localhost:9090/stats

# Prometheus metrics
curl http://localhost:9090/metrics
```

## Using the Go SDK

```go
package main

import (
    "fmt"
    "log"

    boltq "github.com/boltq/boltq/client/golang"
)

func main() {
    client := boltq.New("localhost:9091")
    if err := client.Connect(); err != nil {
        log.Fatal(err)
    }
    defer client.Close()

    // Publish a message
    id, err := client.Publish("email_jobs", map[string]string{
        "to":      "user@example.com",
        "subject": "Welcome",
    }, nil)
    if err != nil {
        log.Fatal(err)
    }
    fmt.Println("Published:", id)

    // Consume a message
    msg, err := client.Consume("email_jobs")
    if err != nil {
        log.Fatal(err)
    }
    if msg != nil {
        fmt.Printf("Received: %s\n", msg.Payload)

        // Acknowledge
        if err := client.Ack(msg.ID); err != nil {
            log.Fatal(err)
        }
    }
}
```

## Message Flow

```
1. Producer publishes message via TCP
2. Message stored in WAL (if disk mode)
3. Message pushed to in-memory ring buffer
4. Consumer polls via TCP and receives message
5. Message enters "pending" state with ACK deadline
6. Consumer processes and ACKs the message via TCP
7. Message removed from pending

On failure:
5b. Consumer NACKs → message re-queued with retry++
5c. ACK timeout → scheduler re-queues automatically
5d. Max retries exceeded → message moved to dead letter queue
```

## Next Steps

- [Configuration](./configuration.md) — All configuration options
- [Persistence](./persistence.md) — WAL and storage details
- [Monitoring](./monitoring.md) — Prometheus metrics and health checks
- [Deployment](./deployment.md) — Production deployment guide
