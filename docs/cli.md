# CLI Reference

BoltQ includes a command-line client for interacting with the server.

## Build

```bash
make cli
# or
go build -o bin/boltq ./cmd/cli
```

## Environment Variables

| Variable | Default | Description |
|----------|---------|-------------|
| `BOLTQ_ADDR` | `localhost:9091` | Server address |
| `BOLTQ_API_KEY` | `""` | API key for authentication |
| `BOLTQ_TLS_ENABLED` | `false` | Enable TLS (true/false) |
| `BOLTQ_CA_FILE` | `""` | Path to CA certificate file |
| `BOLTQ_TLS_INSECURE` | `false` | Skip TLS verification (true/false) |

## Commands

### publish

Publish a message to a work queue.

```bash
boltq publish -topic <name> -payload <json>
```

**Flags:**
| Flag | Required | Description |
|------|----------|-------------|
| `-topic` | Yes | Queue name |
| `-payload` | Yes | JSON payload string |

**Examples:**
```bash
# Simple string payload
boltq publish -topic email_jobs -payload '"send welcome email"'

# JSON object payload
boltq publish -topic email_jobs -payload '{"to":"user@example.com","subject":"Hello"}'

# Array payload
boltq publish -topic batch_jobs -payload '[1, 2, 3, 4, 5]'
```

**Output:**
```
published: id=a1b2c3d4e5f67890 topic=email_jobs
```

### consume

Consume a message from a work queue (non-blocking).

```bash
boltq consume -topic <name>
```

**Flags:**
| Flag | Required | Description |
|------|----------|-------------|
| `-topic` | Yes | Queue name |

**Output (message available):**
```json
{
  "id": "a1b2c3d4e5f67890",
  "topic": "email_jobs",
  "payload": {"to": "user@example.com"},
  "timestamp": 1706900000000000000,
  "retry": 0
}
```

**Output (no messages):**
```
no messages available
```

### ack

Acknowledge a consumed message.

```bash
boltq ack -id <message_id>
```

**Flags:**
| Flag | Required | Description |
|------|----------|-------------|
| `-id` | Yes | Message ID from consume output |

**Output:**
```
acked: a1b2c3d4e5f67890
```

### nack

Negatively acknowledge a message (triggers retry or dead-letter).

```bash
boltq nack -id <message_id>
```

**Output:**
```
nacked: a1b2c3d4e5f67890
```

### stats

Show broker statistics.

```bash
boltq stats
```

**Output:**
```json
{
  "Queues": {
    "email_jobs": 42
  },
  "Topics": {
    "user_signup": 2
  },
  "DeadLetters": {
    "email_jobs_dead_letter": 1
  },
  "PendingCount": 5
}
```

### health

Check if the server is healthy.

```bash
boltq health
```

**Output (healthy):**
```
healthy
```

**Output (unhealthy):**
```
unhealthy: Get "http://localhost:9090/health": dial tcp: connection refused
```
Exit code: 1

## Usage Patterns

### Produce and consume pipeline

```bash
# Terminal 1: Publish messages
for i in $(seq 1 10); do
  boltq publish -topic jobs -payload "{\"id\":$i}"
done

# Terminal 2: Consume and process
while true; do
  MSG=$(boltq consume -topic jobs 2>/dev/null)
  if [ "$MSG" = "no messages available" ]; then
    sleep 0.1
    continue
  fi
  ID=$(echo "$MSG" | jq -r .id)
  echo "Processing: $ID"
  boltq ack -id "$ID"
done
```

### Monitor queue depth

```bash
watch -n 1 'boltq stats | jq .Queues'
```

### Health check in scripts

```bash
if boltq health > /dev/null 2>&1; then
  echo "BoltQ is running"
else
  echo "BoltQ is DOWN"
  exit 1
fi
```
