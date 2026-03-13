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
docker run -p 9090:9090 boltq
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

**With config file**:
```bash
./bin/boltq-server -config configs/default.json
```

The server starts on `http://localhost:9090` by default.

### 2. Publish a Message

```bash
curl -X POST http://localhost:9090/publish \
  -H "Content-Type: application/json" \
  -d '{"topic":"email_jobs","payload":{"to":"user@example.com","subject":"Welcome"}}'
```

Response:
```json
{"id":"a1b2c3d4e5f6...","topic":"email_jobs"}
```

### 3. Consume a Message

```bash
curl http://localhost:9090/consume?topic=email_jobs
```

Response:
```json
{
  "id": "a1b2c3d4e5f6...",
  "topic": "email_jobs",
  "payload": {"to":"user@example.com","subject":"Welcome"},
  "timestamp": 1706900000000000000,
  "retry": 0
}
```

If no messages available, returns HTTP 204 No Content.

### 4. Acknowledge the Message

```bash
curl -X POST http://localhost:9090/ack \
  -H "Content-Type: application/json" \
  -d '{"id":"a1b2c3d4e5f6..."}'
```

### 5. Check Server Status

```bash
# Health check
curl http://localhost:9090/health

# Broker statistics
curl http://localhost:9090/stats

# Prometheus metrics
curl http://localhost:9090/metrics
```

## Using the CLI

```bash
# Set server URL (default: http://localhost:9090)
export BOLTQ_URL=http://localhost:9090

# Publish
./bin/boltq publish -topic email_jobs -payload '{"to":"user@example.com"}'

# Consume
./bin/boltq consume -topic email_jobs

# ACK
./bin/boltq ack -id <message_id>

# NACK (triggers retry)
./bin/boltq nack -id <message_id>

# View stats
./bin/boltq stats

# Health check
./bin/boltq health
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
    client := boltq.New("http://localhost:9090")

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

## Using the Node.js SDK

```javascript
import { BoltQClient } from 'boltq-client'

const queue = new BoltQClient('http://localhost:9090')

// Publish
const { id } = await queue.publish('email_jobs', {
  to: 'user@example.com',
  subject: 'Welcome'
})
console.log('Published:', id)

// Consume
const msg = await queue.consume('email_jobs')
if (msg) {
  console.log('Received:', msg.payload)
  await queue.ack(msg.id)
}
```

## Pub/Sub Example

### Publish to a topic

```bash
curl -X POST http://localhost:9090/publish/topic \
  -H "Content-Type: application/json" \
  -d '{"topic":"user_signup","payload":{"user_id":"123","email":"new@user.com"}}'
```

### Subscribe via SSE

```bash
curl http://localhost:9090/subscribe?topic=user_signup&id=my-subscriber
```

This opens a Server-Sent Events stream. Messages appear as:

```
data: {"id":"...","topic":"user_signup","payload":{...},"timestamp":...}
```

## Message Flow

```
1. Producer publishes message
2. Message stored in WAL (if disk mode)
3. Message pushed to in-memory ring buffer
4. Consumer polls and receives message
5. Message enters "pending" state with ACK deadline
6. Consumer processes and ACKs the message
7. Message removed from pending

On failure:
5b. Consumer NACKs → message re-queued with retry++
5c. ACK timeout → scheduler re-queues automatically
5d. Max retries exceeded → message moved to dead letter queue
```

## Next Steps

- [API Reference](./api-reference.md) — Full HTTP API documentation
- [Configuration](./configuration.md) — All configuration options
- [Persistence](./persistence.md) — WAL and storage details
- [Monitoring](./monitoring.md) — Prometheus metrics and health checks
- [Deployment](./deployment.md) — Production deployment guide
