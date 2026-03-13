# API Reference

BoltQ exposes a RESTful HTTP API on the configured port (default: 9090).

## Authentication

If `api_key` is configured, all endpoints (except `/health` and `/metrics`) require authentication.

**Header authentication:**
```
X-API-Key: your-secret-key
```

**Query parameter authentication:**
```
GET /stats?api_key=your-secret-key
```

---

## Endpoints

### POST /publish

Publish a message to a **work queue** (1 message → 1 consumer).

**Request:**
```http
POST /publish
Content-Type: application/json

{
  "topic": "email_jobs",
  "payload": {"to": "user@example.com", "subject": "Hello"},
  "headers": {
    "priority": "high",
    "source": "api"
  }
}
```

| Field | Type | Required | Description |
|-------|------|----------|-------------|
| `topic` | string | Yes | Queue name |
| `payload` | any | Yes | Message payload (any valid JSON) |
| `headers` | object | No | Key-value metadata headers |

**Response (200 OK):**
```json
{
  "id": "a1b2c3d4e5f67890abcdef1234567890",
  "topic": "email_jobs"
}
```

**Errors:**
| Code | Description |
|------|-------------|
| 400 | Missing topic or invalid JSON |
| 401 | Invalid or missing API key |
| 500 | Queue is full or storage write failure |

---

### POST /publish/topic

Publish a message to a **pub/sub topic** (1 message → all subscribers).

**Request:** Same format as `/publish`.

**Response:** Same format as `/publish`.

**Behavior:**
- Message is delivered to all active subscribers
- If a subscriber's buffer is full, the message is dropped for that subscriber
- No persistence of subscriber state

---

### GET /consume

Consume a message from a work queue (non-blocking).

**Request:**
```http
GET /consume?topic=email_jobs
```

| Parameter | Type | Required | Description |
|-----------|------|----------|-------------|
| `topic` | string | Yes | Queue name to consume from |

**Response (200 OK):**
```json
{
  "id": "a1b2c3d4e5f67890abcdef1234567890",
  "topic": "email_jobs",
  "payload": {"to": "user@example.com", "subject": "Hello"},
  "headers": {"priority": "high"},
  "timestamp": 1706900000000000000,
  "retry": 0
}
```

| Field | Type | Description |
|-------|------|-------------|
| `id` | string | Unique message ID (use for ACK/NACK) |
| `topic` | string | Queue name |
| `payload` | any | Original message payload |
| `headers` | object | Message headers (if any) |
| `timestamp` | number | Unix timestamp in nanoseconds |
| `retry` | number | Number of times this message has been retried |

**Response (204 No Content):** No messages available in the queue.

**Notes:**
- This is a non-blocking call. Returns immediately with 204 if no messages.
- After consuming, the message enters "pending" state with an ACK deadline.
- You must ACK or NACK the message within the configured `ack_timeout`.

---

### POST /ack

Acknowledge a consumed message (mark as successfully processed).

**Request:**
```http
POST /ack
Content-Type: application/json

{
  "id": "a1b2c3d4e5f67890abcdef1234567890"
}
```

| Field | Type | Required | Description |
|-------|------|----------|-------------|
| `id` | string | Yes | Message ID from consume response |

**Response (200 OK):**
```json
{
  "status": "acked"
}
```

**Errors:**
| Code | Description |
|------|-------------|
| 400 | Missing message ID |
| 404 | Message not found in pending (already ACK'd or expired) |

---

### POST /nack

Negatively acknowledge a message (trigger retry or dead-letter).

**Request:**
```http
POST /nack
Content-Type: application/json

{
  "id": "a1b2c3d4e5f67890abcdef1234567890"
}
```

**Response (200 OK):**
```json
{
  "status": "nacked"
}
```

**Behavior:**
1. Message `retry` counter is incremented
2. If `retry <= max_retry`: message is re-queued to the original queue
3. If `retry > max_retry`: message is moved to the dead letter queue (`{topic}_dead_letter`)

---

### GET /subscribe

Subscribe to a pub/sub topic via Server-Sent Events (SSE).

**Request:**
```http
GET /subscribe?topic=user_signup&id=my-subscriber-1
```

| Parameter | Type | Required | Description |
|-----------|------|----------|-------------|
| `topic` | string | Yes | Topic name to subscribe to |
| `id` | string | Yes | Unique subscriber ID |

**Response:** SSE stream with `Content-Type: text/event-stream`

```
data: {"id":"abc123","topic":"user_signup","payload":{"user_id":"42"},"timestamp":1706900000000000000}

data: {"id":"def456","topic":"user_signup","payload":{"user_id":"43"},"timestamp":1706900001000000000}
```

**Notes:**
- Connection stays open until client disconnects
- Each subscriber ID is unique per topic — reconnecting with the same ID replaces the previous subscription
- Buffer size: 256 messages per subscriber

---

### GET /stats

Get broker statistics.

**Request:**
```http
GET /stats
```

**Response (200 OK):**
```json
{
  "Queues": {
    "email_jobs": 42,
    "notifications": 0
  },
  "Topics": {
    "user_signup": 3
  },
  "DeadLetters": {
    "email_jobs_dead_letter": 2
  },
  "PendingCount": 5
}
```

| Field | Type | Description |
|-------|------|-------------|
| `Queues` | object | Queue name → current message count |
| `Topics` | object | Topic name → active subscriber count |
| `DeadLetters` | object | Dead letter queue name → message count |
| `PendingCount` | number | Messages awaiting ACK |

---

### GET /metrics

Get Prometheus-compatible metrics.

**Request:**
```http
GET /metrics
```

**Response (Prometheus text format):**
```
# HELP boltq_messages_published Total messages published
# TYPE boltq_messages_published counter
boltq_messages_published 1234
# HELP boltq_messages_consumed Total messages consumed
# TYPE boltq_messages_consumed counter
boltq_messages_consumed 1200
# HELP boltq_messages_acked Total messages acknowledged
# TYPE boltq_messages_acked counter
boltq_messages_acked 1195
# HELP boltq_messages_nacked Total messages negatively acknowledged
# TYPE boltq_messages_nacked counter
boltq_messages_nacked 5
# HELP boltq_retry_count Total retry count
# TYPE boltq_retry_count counter
boltq_retry_count 8
# HELP boltq_dead_letter_count Total dead letter count
# TYPE boltq_dead_letter_count counter
boltq_dead_letter_count 2
```

**JSON format** (set `Accept: application/json` header):
```json
{
  "messages_published": 1234,
  "messages_consumed": 1200,
  "messages_acked": 1195,
  "messages_nacked": 5,
  "retry_count": 8,
  "dead_letter_count": 2
}
```

---

### GET /health

Health check endpoint (no authentication required).

**Request:**
```http
GET /health
```

**Response (200 OK):**
```json
{
  "status": "ok"
}
```

---

## Message Structure

```json
{
  "id": "a1b2c3d4e5f67890abcdef1234567890",
  "topic": "email_jobs",
  "payload": "<any valid JSON>",
  "headers": {
    "key": "value"
  },
  "timestamp": 1706900000000000000,
  "retry": 0,
  "max_retry": 5
}
```

| Field | Type | Description |
|-------|------|-------------|
| `id` | string | 32-character hex string (128-bit random) |
| `topic` | string | Queue or topic name |
| `payload` | any | Arbitrary JSON payload |
| `headers` | object | Optional key-value metadata |
| `timestamp` | int64 | Creation time (Unix nanoseconds) |
| `retry` | int | Current retry count |
| `max_retry` | int | Maximum retries before dead-letter |

## Rate Limits

BoltQ does not enforce rate limits at the API layer. Throughput is bounded by:
- Queue capacity (default: 1,048,576 messages)
- Request body size limit: 1MB
- HTTP connection limits (OS-level)

## Error Format

All errors follow the same JSON format:

```json
{
  "error": "description of the error"
}
```
