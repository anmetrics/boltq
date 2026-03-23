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
| `priority` | int | No | Priority level 0-9 (higher = more urgent, default 0) |

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

## Exchange Routing

BoltQ supports exchange-based routing for flexible message distribution. Exchanges receive messages and route them to bound queues based on type-specific rules.

---

### POST /exchange/declare

Create or assert an exchange.

**Request:**
```http
POST /exchange/declare
Content-Type: application/json

{
  "name": "logs",
  "type": "topic",
  "durable": true
}
```

| Field | Type | Required | Description |
|-------|------|----------|-------------|
| `name` | string | Yes | Exchange name |
| `type` | string | Yes | Exchange type: `direct`, `fanout`, `topic`, `headers` |
| `durable` | bool | No | Survive server restart (default false) |

**Response (200 OK):**
```json
{"status": "ok"}
```

---

### POST /exchange/delete

Delete an exchange.

**Request:**
```http
POST /exchange/delete
Content-Type: application/json

{"name": "logs"}
```

**Response (200 OK):**
```json
{"status": "ok"}
```

---

### POST /exchange/bind

Bind a queue to an exchange.

**Request:**
```http
POST /exchange/bind
Content-Type: application/json

{
  "exchange": "logs",
  "queue": "error_logs",
  "binding_key": "log.error.*",
  "headers": {},
  "match_all": false
}
```

| Field | Type | Required | Description |
|-------|------|----------|-------------|
| `exchange` | string | Yes | Exchange name |
| `queue` | string | Yes | Target queue name |
| `binding_key` | string | No | Routing pattern (used by `direct` and `topic` types) |
| `headers` | object | No | Header match rules (used by `headers` type) |
| `match_all` | bool | No | If true, all headers must match; if false, any match suffices |

**Response (200 OK):**
```json
{"status": "ok"}
```

---

### POST /exchange/unbind

Remove a binding.

**Request:**
```http
POST /exchange/unbind
Content-Type: application/json

{
  "exchange": "logs",
  "queue": "error_logs",
  "binding_key": "log.error.*"
}
```

**Response (200 OK):**
```json
{"status": "ok"}
```

---

### POST /exchange/publish

Publish to an exchange with routing key.

**Request:**
```http
POST /exchange/publish
Content-Type: application/json

{
  "exchange": "logs",
  "routing_key": "log.error.auth",
  "payload": {"message": "Failed login attempt"},
  "headers": {},
  "priority": 5
}
```

| Field | Type | Required | Description |
|-------|------|----------|-------------|
| `exchange` | string | Yes | Target exchange name |
| `routing_key` | string | No | Routing key for `direct`/`topic` exchanges |
| `payload` | any | Yes | Message payload (any valid JSON) |
| `headers` | object | No | Message headers |
| `priority` | int | No | Priority level 0-9 (higher = more urgent, default 0) |

**Response (200 OK):**
```json
{"status": "published", "id": "a1b2c3d4e5f67890abcdef1234567890"}
```

---

## WebSocket

BoltQ exposes a WebSocket endpoint at `GET /ws` for real-time, bidirectional messaging from browsers and other WebSocket clients. Zero external dependencies — implemented with pure Go standard library (RFC 6455).

### Connection

```
ws://localhost:9090/ws
```

### Protocol

Clients send JSON commands and receive JSON responses over WebSocket text frames.

**Request format:**
```json
{"cmd": "<command>", ...params}
```

**Response format:**
```json
{"status": "ok|error|empty", "data": {...}, "error": "..."}
```

### Commands

| Command | Description | Key Fields |
|---------|-------------|------------|
| `auth` | Authenticate | `api_key` |
| `ping` | Health check | — |
| `prefetch` | Set prefetch limit | `count` |
| `confirm` | Enable publisher confirm | — |
| `publish` | Publish to work queue | `topic`, `payload`, `headers`, `delay`, `ttl`, `priority` |
| `publish_topic` | Publish to pub/sub topic | `topic`, `payload`, `headers`, `delay`, `ttl`, `priority` |
| `consume` | Consume from queue | `topic` |
| `subscribe` | Subscribe to pub/sub | `topic`, `id`, `durable` |
| `ack` | Acknowledge message | `id` |
| `nack` | Negative acknowledge | `id` |
| `stats` | Get broker statistics | — |
| `exchange_declare` | Declare exchange | `exchange`, `type`, `durable` |
| `exchange_delete` | Delete exchange | `exchange` |
| `bind_queue` | Bind queue to exchange | `exchange`, `queue`, `binding_key`, `headers`, `match_all` |
| `unbind_queue` | Unbind queue | `exchange`, `queue`, `binding_key` |
| `publish_exchange` | Publish via exchange | `exchange`, `routing_key`, `payload`, `headers`, `priority` |

### Subscription Events

When subscribed, the server pushes messages as:

```json
{
  "event": "message",
  "id": "msg-id",
  "topic": "events",
  "payload": {...},
  "headers": {},
  "timestamp": 1234567890,
  "subscriber_id": "sub-1",
  "priority": 0
}
```

### Example (JavaScript browser)

```js
const ws = new WebSocket('ws://localhost:9090/ws');

ws.onopen = () => {
  // Authenticate (if api_key configured)
  ws.send(JSON.stringify({ cmd: 'auth', api_key: 'secret' }));

  // Publish
  ws.send(JSON.stringify({
    cmd: 'publish',
    topic: 'orders',
    payload: { item: 'widget', qty: 3 },
    priority: 5
  }));

  // Subscribe to real-time events
  ws.send(JSON.stringify({
    cmd: 'subscribe',
    topic: 'notifications',
    id: 'browser-1'
  }));
};

ws.onmessage = (event) => {
  const msg = JSON.parse(event.data);
  if (msg.event === 'message') {
    console.log('Received:', msg.payload);
  } else {
    console.log('Response:', msg);
  }
};
```

---

## Cache / KV Store

BoltQ includes a built-in in-memory KV store with TTL support. Enable it via config (`cache.enabled: true`).

---

### POST /cache/set

Store a key-value pair.

**Request:**
```http
POST /cache/set
Content-Type: application/json

{
  "key": "user:123:session",
  "value": {"token": "abc", "role": "admin"},
  "ttl": 3600000
}
```

| Field | Type | Required | Description |
|-------|------|----------|-------------|
| `key` | string | Yes | Cache key |
| `value` | any | Yes | Value (string, number, JSON object) |
| `ttl` | number | No | Time-to-live in milliseconds. 0 = no expiry |

**Response (200 OK):**
```json
{"status": "ok", "key": "user:123:session"}
```

---

### GET /cache/get

Retrieve a value by key.

**Request:**
```http
GET /cache/get?key=user:123:session
```

**Response (200 OK):**
```json
{
  "key": "user:123:session",
  "value": {"token": "abc", "role": "admin"},
  "ttl": 3542100,
  "created_at": 1706900000000000000
}
```

**Response (404):** Key not found or expired.

---

### POST /cache/del

Delete a key.

**Request:**
```http
POST /cache/del
Content-Type: application/json

{"key": "user:123:session"}
```

**Response (200 OK):**
```json
{"status": "ok", "deleted": true}
```

---

### GET /cache/keys

List keys matching a glob pattern.

**Request:**
```http
GET /cache/keys?pattern=user:*
```

| Pattern | Matches |
|---------|---------|
| `*` | All keys |
| `user:*` | Keys starting with "user:" |
| `*:session` | Keys ending with ":session" |
| `*config*` | Keys containing "config" |

**Response (200 OK):**
```json
{"keys": ["user:123:session", "user:456:session"], "count": 2}
```

---

### GET /cache/exists

Check if a key exists.

**Request:**
```http
GET /cache/exists?key=user:123:session
```

**Response (200 OK):**
```json
{"key": "user:123:session", "exists": true}
```

---

### POST /cache/expire

Set or update TTL on a key.

**Request:**
```http
POST /cache/expire
Content-Type: application/json

{"key": "user:123:session", "ttl": 60000}
```

---

### POST /cache/incr

Atomically increment a numeric value. Creates the key with the delta if it doesn't exist.

**Request:**
```http
POST /cache/incr
Content-Type: application/json

{"key": "rate:api:calls", "delta": 1}
```

**Response (200 OK):**
```json
{"key": "rate:api:calls", "value": 42}
```

---

### POST /cache/flush

Remove all cache entries.

**Request:**
```http
POST /cache/flush
```

**Response (200 OK):**
```json
{"status": "flushed", "removed": 1234}
```

---

### GET /cache/stats

Get cache statistics.

**Response (200 OK):**
```json
{
  "enabled": true,
  "stats": {
    "key_count": 150,
    "memory_used": 48320,
    "hits": 12500,
    "misses": 340,
    "sets": 1200,
    "deletes": 50,
    "expired": 80
  }
}
```

---

### GET /cache/entries

Browse cache entries (for admin UI). Limited to 100 entries.

**Request:**
```http
GET /cache/entries?pattern=*&search=user
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
  "max_retry": 5,
  "priority": 0,
  "exchange": "",
  "routing_key": ""
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
| `priority` | int | Priority level 0-9 (higher = more urgent) |
| `exchange` | string | Source exchange name (empty if published directly) |
| `routing_key` | string | Routing key used for exchange routing |

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
