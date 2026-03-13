# Monitoring

BoltQ exposes metrics and health checks for production monitoring.

## Health Check

```bash
curl http://localhost:9090/health
```

```json
{"status": "ok"}
```

Use this for load balancer health checks, Kubernetes liveness/readiness probes, and uptime monitoring.

### Kubernetes Probes

```yaml
livenessProbe:
  httpGet:
    path: /health
    port: 9090
  initialDelaySeconds: 5
  periodSeconds: 10

readinessProbe:
  httpGet:
    path: /health
    port: 9090
  initialDelaySeconds: 3
  periodSeconds: 5
```

## Prometheus Metrics

### Endpoint

```bash
curl http://localhost:9090/metrics
```

Returns metrics in Prometheus text exposition format (no authentication required).

### Available Metrics

| Metric | Type | Description |
|--------|------|-------------|
| `boltq_messages_published` | counter | Total number of messages published |
| `boltq_messages_consumed` | counter | Total number of messages consumed |
| `boltq_messages_acked` | counter | Total number of messages acknowledged |
| `boltq_messages_nacked` | counter | Total number of messages negatively acknowledged |
| `boltq_retry_count` | counter | Total number of message retries |
| `boltq_dead_letter_count` | counter | Total number of messages sent to dead letter queues |

### Sample Output

```
# HELP boltq_messages_published Total messages published
# TYPE boltq_messages_published counter
boltq_messages_published 15234
# HELP boltq_messages_consumed Total messages consumed
# TYPE boltq_messages_consumed counter
boltq_messages_consumed 15100
# HELP boltq_messages_acked Total messages acknowledged
# TYPE boltq_messages_acked counter
boltq_messages_acked 15050
# HELP boltq_messages_nacked Total messages negatively acknowledged
# TYPE boltq_messages_nacked counter
boltq_messages_nacked 50
# HELP boltq_retry_count Total retry count
# TYPE boltq_retry_count counter
boltq_retry_count 73
# HELP boltq_dead_letter_count Total dead letter count
# TYPE boltq_dead_letter_count counter
boltq_dead_letter_count 8
```

### JSON Format

Request with `Accept: application/json` header:

```bash
curl -H "Accept: application/json" http://localhost:9090/metrics
```

```json
{
  "messages_published": 15234,
  "messages_consumed": 15100,
  "messages_acked": 15050,
  "messages_nacked": 50,
  "retry_count": 73,
  "dead_letter_count": 8
}
```

## Prometheus Configuration

Add to your `prometheus.yml`:

```yaml
scrape_configs:
  - job_name: 'boltq'
    scrape_interval: 15s
    static_configs:
      - targets: ['localhost:9090']
    metrics_path: '/metrics'
```

### Multiple Instances

```yaml
scrape_configs:
  - job_name: 'boltq'
    scrape_interval: 15s
    static_configs:
      - targets:
        - 'boltq-1:9090'
        - 'boltq-2:9090'
        - 'boltq-3:9090'
```

## Broker Statistics

The `/stats` endpoint provides real-time broker state (requires authentication if configured):

```bash
curl http://localhost:9090/stats
```

```json
{
  "Queues": {
    "email_jobs": 42,
    "notifications": 128,
    "analytics": 0
  },
  "Topics": {
    "user_signup": 3,
    "order_placed": 1
  },
  "DeadLetters": {
    "email_jobs_dead_letter": 5,
    "notifications_dead_letter": 0
  },
  "PendingCount": 12
}
```

| Field | Description |
|-------|-------------|
| `Queues` | Queue name → number of messages waiting |
| `Topics` | Topic name → number of active subscribers |
| `DeadLetters` | Dead letter queue name → message count |
| `PendingCount` | Messages consumed but not yet ACK'd/NACK'd |

## Grafana Dashboard

### Recommended Panels

**1. Message Throughput (rate)**
```promql
rate(boltq_messages_published[5m])
rate(boltq_messages_consumed[5m])
```

**2. Processing Success Rate**
```promql
rate(boltq_messages_acked[5m]) / rate(boltq_messages_consumed[5m]) * 100
```

**3. Retry Rate**
```promql
rate(boltq_retry_count[5m])
```

**4. Dead Letter Rate**
```promql
rate(boltq_dead_letter_count[5m])
```

**5. Consumer Lag (publish - consume rate)**
```promql
rate(boltq_messages_published[5m]) - rate(boltq_messages_consumed[5m])
```

## Alerting Rules

### Prometheus Alerting

```yaml
groups:
  - name: boltq
    rules:
      - alert: BoltQHighDeadLetterRate
        expr: rate(boltq_dead_letter_count[5m]) > 1
        for: 5m
        labels:
          severity: warning
        annotations:
          summary: "BoltQ dead letter rate is high"
          description: "More than 1 msg/sec going to dead letter queue"

      - alert: BoltQConsumerLag
        expr: rate(boltq_messages_published[5m]) - rate(boltq_messages_consumed[5m]) > 100
        for: 10m
        labels:
          severity: warning
        annotations:
          summary: "BoltQ consumer lag growing"
          description: "Publish rate exceeds consume rate by >100 msg/sec"

      - alert: BoltQDown
        expr: up{job="boltq"} == 0
        for: 1m
        labels:
          severity: critical
        annotations:
          summary: "BoltQ server is down"
```

## Logging

BoltQ logs to stdout using Go's standard `log` package:

```
[server] storage mode: memory
[server] BoltQ started on 0.0.0.0:9090
[http] listening on 0.0.0.0:9090
[scheduler] requeue timeout msg abc123: message abc123 not found
[server] received signal interrupt, shutting down...
[server] BoltQ stopped
```

### Log Prefixes

| Prefix | Component |
|--------|-----------|
| `[server]` | Server lifecycle |
| `[http]` | HTTP API |
| `[scheduler]` | ACK timeout / retry scheduler |
