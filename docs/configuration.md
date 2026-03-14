# Configuration

BoltQ can be configured via a JSON config file, environment variables, or command-line flags. Environment variables take precedence over the config file.

## Config File

Pass the config file path with the `-config` flag:

```bash
./bin/boltq-server -config configs/default.json
```

### Full Config Example

```json
{
  "server": {
    "http_port": 9090,
    "grpc_port": 9091,
    "host": "0.0.0.0",
    "tls": {
      "enabled": false,
      "cert_file": "/etc/boltq/certs/server.crt",
      "key_file": "/etc/boltq/certs/server.key"
    }
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

## Config Reference

### server

| Field | Type | Default | Description |
|-------|------|---------|-------------|
| `http_port` | int | `9090` | HTTP API listening port |
| `grpc_port` | int | `9091` | gRPC listening port (reserved) |
| `host` | string | `0.0.0.0` | Bind address |
| `tls.enabled` | bool | `false` | Enable TLS encryption |
| `tls.cert_file` | string | `""` | Path to TLS certificate file |
| `tls.key_file` | string | `""` | Path to TLS private key file |

### storage

| Field | Type | Default | Description |
|-------|------|---------|-------------|
| `mode` | string | `memory` | Storage mode: `memory` or `disk` |
| `data_dir` | string | `./data` | Directory for WAL files (disk mode only) |
| `compaction_threshold` | int64 | `104857600` | WAL size in bytes before automatic cleanup (100MB) |

**Storage modes:**

- **`memory`**: Messages stored in-memory only. Fastest performance. Data lost on restart.
- **`disk`**: Messages written to WAL first, then pushed to memory. Survives restart via WAL replay.

### queue

| Field | Type | Default | Description |
|-------|------|---------|-------------|
| `max_retry` | int | `5` | Maximum retry attempts before dead-letter |
| `ack_timeout` | duration | `30s` | Time allowed to ACK a message before re-queue |
| `capacity` | int | `1048576` | Maximum messages per queue (rounded up to power of 2) |

**Duration format:** Go duration strings — `30s`, `1m`, `5m30s`, `1h`, etc.

**Capacity notes:**
- Value is rounded up to the nearest power of 2 (e.g., 1000 → 1024)
- Each message pointer is 8 bytes, so 1M capacity = ~8MB per queue
- Consider available memory when setting high capacities

### performance

| Field | Type | Default | Description |
|-------|------|---------|-------------|
| `worker_pool` | int | `16` | Worker pool size (reserved for future use) |

### security

| Field | Type | Default | Description |
|-------|------|---------|-------------|
| `api_key` | string | `""` | API key for authentication. Empty = no auth |

## Environment Variables

Environment variables override config file values.

| Variable | Config Path | Description |
|----------|-------------|-------------|
| `BOLTQ_HTTP_PORT` | `server.http_port` | HTTP listening port |
| `BOLTQ_STORAGE_MODE` | `storage.mode` | `memory` or `disk` |
| `BOLTQ_DATA_DIR` | `storage.data_dir` | WAL data directory |
| `BOLTQ_STORAGE_COMPACTION_THRESHOLD` | `storage.compaction_threshold` | WAL size threshold in bytes |
| `BOLTQ_API_KEY` | `security.api_key` | API authentication key |

### Examples

```bash
# Memory mode on port 8080
BOLTQ_HTTP_PORT=8080 ./bin/boltq-server

# Disk mode with custom data directory
BOLTQ_STORAGE_MODE=disk BOLTQ_DATA_DIR=/var/lib/boltq ./bin/boltq-server

# With API key authentication
BOLTQ_API_KEY=my-secret-key-123 ./bin/boltq-server

# Combine with config file (env vars override)
BOLTQ_STORAGE_MODE=disk ./bin/boltq-server -config configs/default.json
```

## Default Values

If no config file is provided and no environment variables are set, BoltQ uses these defaults:

```
HTTP Port:     9090
Host:          0.0.0.0
Storage:       memory
Data Dir:      ./data
Compaction Threshold: 100MB
Max Retry:     5
ACK Timeout:   30 seconds
Queue Capacity: 1,048,576 (1M messages)
Worker Pool:   16
API Key:       (none — no authentication)
```

## Tuning Guide

### High Throughput

```json
{
  "queue": {
    "capacity": 4194304,
    "ack_timeout": "10s"
  },
  "storage": {
    "mode": "memory"
  }
}
```

- Use memory mode (no WAL overhead)
- Increase queue capacity for burst handling
- Reduce ACK timeout to reclaim messages faster

### Durability

```json
{
  "storage": {
    "mode": "disk",
    "data_dir": "/var/lib/boltq/data"
  },
  "queue": {
    "max_retry": 10,
    "ack_timeout": "60s"
  }
}
```

- Use disk mode for crash recovery
- Increase max retry for resilience
- Increase ACK timeout for long-running consumers

### Low Latency

```json
{
  "queue": {
    "capacity": 65536,
    "ack_timeout": "5s"
  },
  "storage": {
    "mode": "memory"
  }
}
```

- Smaller queue capacity = better cache locality
- Memory mode only
- Short ACK timeout

## Advanced: Autoscaling

When running on Kubernetes, BoltQ read replicas (non-voter nodes) can be autoscaled using HPA. This is configured in the deployment manifests. For more details, see [Deployment Guide](./deployment.md#autoscaling).
