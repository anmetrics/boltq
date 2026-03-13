# BoltQ

High-performance message queue server written in Go. Built for low latency, high throughput, and production clustering.

## Features

- **Work Queue** — 1 message → 1 consumer (like RabbitMQ)
- **Pub/Sub** — 1 message → all subscribers (like Redis Pub/Sub)
- **Raft Cluster** — quorum-based replication with automatic leader election
- **Auto-Scale** — non-voter read replicas scale horizontally without affecting consensus
- **Seed Discovery** — nodes auto-discover leader via seed list, no hardcoded topology
- **TCP Binary Protocol** — low-latency binary framing for all messaging (port 9091)
- **HTTP Admin API** — stats, metrics, health, cluster management (port 9090)
- **Web Dashboard** — Nuxt 3 admin UI with real-time monitoring
- **ACK/NACK** — consumer acknowledgment with configurable timeout
- **Retry** — exponential retry with max retries → dead letter queue
- **WAL Persistence** — Write-Ahead Log for crash recovery
- **Prometheus Metrics** — `/metrics` endpoint
- **API Key Auth** — optional authentication
- **TLS Encryption** — full end-to-end encryption for TCP and HTTP
- **Lock-free ring buffer** — 1M+ message capacity

## Performance

| Metric | Target |
|--------|--------|
| Publish latency | < 1ms |
| Throughput | 200k msg/sec |
| Memory footprint | < 50MB |
| Startup time | < 1s |

## Quick Start

```bash
make build
./bin/boltq-server
```

Server starts with TCP messaging on `:9091` and HTTP admin on `:9090`.

```bash
# With config file
./bin/boltq-server -config configs/default.json

# With Docker
docker build -t boltq .
docker run -p 9090:9090 -p 9091:9091 boltq
```

## TCP Protocol

All messaging operations use the binary TCP protocol.

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
| CLUSTER_JOIN | `0x10` | Join node to cluster |
| CLUSTER_LEAVE | `0x11` | Remove node from cluster |
| CLUSTER_STATUS | `0x12` | Get cluster status |

### Response Status

| Status | Byte | Description |
|--------|------|-------------|
| OK | `0x00` | Success |
| ERROR | `0x01` | Error (payload contains JSON) |
| EMPTY | `0x02` | No message available |
| NOT_LEADER | `0x03` | Redirect to leader (payload contains leader address) |

## Go Client SDK

```go
import boltq "github.com/boltq/boltq/client/golang"

client := boltq.New("localhost:9091")
client.Connect()
defer client.Close()

// Publish to work queue
id, err := client.Publish("email_jobs", map[string]interface{}{
    "to": "user@example.com",
}, nil)

// Publish to pub/sub topic
id, err = client.PublishTopic("user_signup", eventData, nil)

// Consume
msg, err := client.Consume("email_jobs")
if msg != nil {
    client.Ack(msg.ID)
}

// NACK (retry)
client.Nack(msg.ID)

// Stats
stats, err := client.Stats()
```

### Client Options

```go
client := boltq.New("localhost:9091", boltq.WithAPIKey("secret"))
client := boltq.New("localhost:9091", boltq.WithTimeout(5 * time.Second))
client := boltq.New("localhost:9091", boltq.WithTLS(&tls.Config{InsecureSkipVerify: true}))
```

### Cluster-Aware Client

```go
client := boltq.New("10.0.0.2:9091")
client.Connect()

id, err := client.Publish("orders", payload, nil)
if err != nil {
    if nle, ok := err.(*boltq.NotLeaderError); ok {
        client.Close()
        client = boltq.New(nle.Leader)
        client.Connect()
        id, err = client.Publish("orders", payload, nil)
    }
}
```

## CLI

```bash
export BOLTQ_ADDR=localhost:9091

boltq publish -topic email_jobs -payload '{"to":"user@example.com"}'
boltq consume -topic email_jobs
boltq ack -id <message_id>
boltq nack -id <message_id>
boltq stats
boltq health

# With TLS
export BOLTQ_TLS_ENABLED=true
export BOLTQ_CA_FILE=./certs/ca.crt
boltq stats
```

## HTTP Admin API

HTTP is admin-only. All messaging goes through TCP.

```bash
# Health
curl localhost:9090/health

# Overview (combined dashboard data)
curl localhost:9090/overview

# Stats
curl localhost:9090/stats

# Metrics (Prometheus)
curl localhost:9090/metrics

# Purge queue
curl -X POST localhost:9090/queues/purge \
  -H "Content-Type: application/json" \
  -d '{"queue":"email_jobs"}'

# Purge dead letters
curl -X POST localhost:9090/dead-letters/purge \
  -H "Content-Type: application/json" \
  -d '{"queue":"email_jobs"}'

# Cluster status
curl localhost:9090/cluster/status

# Join voter node
curl -X POST localhost:9090/cluster/join \
  -H "Content-Type: application/json" \
  -d '{"node_id":"node4","addr":"10.0.0.4:9100"}'

# Join non-voter (read replica)
curl -X POST localhost:9090/cluster/join \
  -H "Content-Type: application/json" \
  -d '{"node_id":"replica1","addr":"10.0.0.10:9100","non_voter":true}'

# Remove node
curl -X POST localhost:9090/cluster/leave \
  -H "Content-Type: application/json" \
  -d '{"node_id":"node4"}'
```

## Web Admin Dashboard

Nuxt 3 + Vuetify dark theme dashboard.

```bash
cd web && npm install && npm run dev
# or
make web
```

Pages: Dashboard, Queues, Topics, Dead Letters, Cluster, Metrics. Auto-refresh every 5s.

Set `BOLTQ_ADMIN_URL` to point to the BoltQ HTTP server (default: `http://localhost:9090`).

## Configuration

```json
{
  "server": {
    "http_port": 9090,
    "tcp_port": 9091,
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
  "security": {
    "api_key": ""
  },
  "server": {
    "tls": {
      "enabled": false,
      "cert_file": "",
      "key_file": ""
    }
  },
  "cluster": {
    "enabled": false,
    "node_id": "node1",
    "raft_addr": "0.0.0.0:9100",
    "raft_dir": "./data/raft",
    "bootstrap": false,
    "seeds": [],
    "non_voter": false,
    "snapshot_threshold": 8192
  }
}
```

### Environment Variables

| Variable | Description |
|----------|-------------|
| `BOLTQ_HTTP_PORT` | HTTP admin port (default: 9090) |
| `BOLTQ_TCP_PORT` | TCP messaging port (default: 9091) |
| `BOLTQ_STORAGE_MODE` | `memory` or `disk` |
| `BOLTQ_DATA_DIR` | Data directory |
| `BOLTQ_API_KEY` | API key for auth |
| `BOLTQ_CLUSTER_ENABLED` | Enable cluster mode |
| `BOLTQ_NODE_ID` | Node identifier (auto-generated from hostname if empty) |
| `BOLTQ_RAFT_ADDR` | Raft listen address |
| `BOLTQ_RAFT_DIR` | Raft data directory |
| `BOLTQ_BOOTSTRAP` | Bootstrap as first cluster node |
| `BOLTQ_SEEDS` | Comma-separated seed HTTP addresses for auto-discovery |
| `BOLTQ_NON_VOTER` | Join as non-voter read replica |
| `BOLTQ_JOIN_ADDR` | Comma-separated leader/seed addresses (alternative to seeds) |
| `BOLTQ_CLUSTER_PEERS` | Comma-separated peer addresses |
| `BOLTQ_TLS_ENABLED` | Enable TLS (true/false) |
| `BOLTQ_CA_FILE` | Path to CA certificate (for CLI/SDK) |
| `BOLTQ_TLS_INSECURE` | Skip TLS verification (for CLI/SDK) |

## Storage Modes

**Memory** (default) — fastest, messages in ring buffer only.

**Disk** — memory + WAL. Messages written to append-only log, replayed on restart.

```
write → append WAL → push ring buffer
restart → replay WAL → rebuild queues
```

## Cluster Mode

BoltQ uses Raft consensus for high availability, similar to RabbitMQ Quorum Queues.

### Architecture

```
                    ┌──────────────────────────────────┐
                    │     Raft Consensus (fixed)       │
                    │                                  │
                    │  ┌────────┐ ┌────────┐ ┌────────┐│
                    │  │Voter 1 │ │Voter 2 │ │Voter 3 ││
                    │  │(leader)│ │        │ │        ││
                    │  └───┬────┘ └───┬────┘ └───┬────┘│
                    │      └──────────┼──────────┘     │
                    │          replication              │
                    │      ┌──────────┼──────────┐     │
                    │  ┌───┴────┐ ┌───┴────┐ ┌───┴───┐ │
                    │  │Replica │ │Replica │ │  ...  │ │
                    │  │  (NV)  │ │  (NV)  │ │       │ │
                    │  └────────┘ └────────┘ └───────┘ │
                    │       Auto-scale (HPA)           │
                    └──────────────────────────────────┘
```

- **Voter nodes** (3 or 5) — participate in elections and quorum. Fixed count.
- **Non-voter replicas** (N) — receive replicated data, don't vote. Scale horizontally.
- Leader handles all writes; followers/replicas handle reads.
- `NOT_LEADER` response redirects clients to the current leader.
- Graceful leave on shutdown — node removes itself from cluster.

### How It Works

1. Messages replicated to **quorum (majority)** before confirmed to publisher
2. Leader fails → automatic election from remaining voters
3. New nodes discover leader via **seed list** with retry + exponential backoff
4. Non-voters scale read capacity without impacting consensus
5. Snapshot + log compaction prevents unbounded Raft log growth

### Local Dev Cluster

```bash
make cluster          # Start 3-node cluster on localhost
make cluster-status   # Check cluster status
make cluster-stop     # Stop all nodes
make cluster-clean    # Remove all data
```

### Production with Docker Compose

```bash
# Build image
make docker

# Start 3 voter nodes (quorum)
make cluster-up

# Add read replicas
make cluster-scale N=5      # 5 replicas
make cluster-scale N=20     # scale to 20
make cluster-scale N=0      # remove all replicas

# Status
make cluster-ps

# Stop
make cluster-down
```

### Production with Kubernetes

```bash
# Deploy voter StatefulSet (3 nodes) + replica Deployment + gateway Service
kubectl apply -f deploy/kubernetes/

# Scale replicas manually
kubectl scale deployment boltq-replicas --replicas=10

# HPA auto-scales replicas 2→20 based on CPU > 70%
```

### Production with Environment Variables (Any Server)

**Bootstrap leader (first node):**

```bash
BOLTQ_CLUSTER_ENABLED=true \
BOLTQ_NODE_ID=node1 \
BOLTQ_RAFT_ADDR=10.0.0.1:9100 \
BOLTQ_BOOTSTRAP=true \
./bin/boltq-server
```

**Voter nodes (join via seeds):**

```bash
BOLTQ_CLUSTER_ENABLED=true \
BOLTQ_NODE_ID=node2 \
BOLTQ_RAFT_ADDR=10.0.0.2:9100 \
BOLTQ_SEEDS=10.0.0.1:9090 \
./bin/boltq-server
```

**Read replicas (non-voter, auto-scale these):**

```bash
BOLTQ_CLUSTER_ENABLED=true \
BOLTQ_RAFT_ADDR=10.0.0.10:9100 \
BOLTQ_SEEDS=10.0.0.1:9090,10.0.0.2:9090,10.0.0.3:9090 \
BOLTQ_NON_VOTER=true \
./bin/boltq-server
```

Node ID auto-generated from hostname when not set.

### Cluster Config Reference

| Field | Type | Default | Description |
|-------|------|---------|-------------|
| `enabled` | bool | `false` | Enable cluster mode |
| `node_id` | string | hostname | Unique node identifier |
| `raft_addr` | string | `0.0.0.0:9100` | Raft communication address |
| `raft_dir` | string | `./data/raft` | Raft log and snapshot directory |
| `bootstrap` | bool | `false` | Bootstrap as first node in new cluster |
| `seeds` | []string | `[]` | Seed node HTTP addresses for discovery |
| `non_voter` | bool | `false` | Join as non-voter (read replica) |
| `snapshot_threshold` | uint64 | `8192` | Log entries before snapshot |

## Project Structure

```
boltq/
  cmd/
    server/        # Server entrypoint
    cli/           # CLI tool (TCP client)
  internal/
    api/           # HTTP admin + TCP messaging
    broker/        # Message broker (work queue + pub/sub)
    queue/         # Lock-free ring buffer
    cluster/       # Raft consensus clustering
    storage/       # Storage interface
    wal/           # Write-Ahead Log
    scheduler/     # ACK timeout watcher
    config/        # Configuration
    metrics/       # Prometheus metrics
  client/
    golang/        # Go client SDK (TCP)
  pkg/
    protocol/      # Message types + TCP wire format
  deploy/
    docker/        # Docker Compose (production cluster)
    kubernetes/    # K8s manifests (StatefulSet + HPA)
  web/             # Nuxt 3 admin dashboard
  configs/         # Config files
```

## Benchmarks

```bash
make bench          # All benchmarks
make bench-queue    # Ring buffer
make bench-wal      # Write-Ahead Log
make bench-broker   # Message broker
make bench-api      # TCP API
```

## Testing

```bash
make test
```

## License

MIT
