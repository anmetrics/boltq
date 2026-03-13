# boltq-client

Zero-dependency Node.js client SDK for the [BoltQ](https://github.com/thien/boltq) message queue server.

Requires **Node.js 18+** (uses native `fetch`).

## Installation

```bash
npm install boltq-client
```

## Quick Start

```js
import { BoltQClient } from 'boltq-client';

const queue = new BoltQClient('http://localhost:9090');

// Publish a message
const { id } = await queue.publish('email_jobs', { to: 'user@example.com' });

// Consume a message
const msg = await queue.consume('email_jobs');
if (msg) {
  console.log(msg.payload);
  await queue.ack(msg.id);
}
```

## Constructor

```js
new BoltQClient(baseURL, options?)
```

| Parameter | Type | Description |
|-----------|------|-------------|
| `baseURL` | `string` | Server URL, e.g. `http://localhost:9090` |
| `options.apiKey` | `string` | Optional API key (sent as `X-API-Key` header) |
| `options.timeout` | `number` | Request timeout in ms (default: `30000`) |

## API

### `publish(topic, payload, headers?)`

Publish a message to a queue topic. Non-string payloads are JSON-stringified.

```js
const result = await queue.publish('orders', { orderId: 42 }, { priority: 'high' });
// result: { id: '...', topic: 'orders' }
```

### `publishTopic(topic, payload, headers?)`

Publish a message to a pub/sub topic. Same signature as `publish`.

```js
await queue.publishTopic('events', { type: 'user_signup', userId: 7 });
```

### `consume(topic)`

Consume a single message. Returns `null` when no messages are available.

```js
const msg = await queue.consume('orders');
if (msg) {
  console.log(msg.id, msg.payload);
}
```

### `ack(messageId)`

Acknowledge a consumed message (marks it as complete).

```js
await queue.ack(msg.id);
```

### `nack(messageId)`

Negatively acknowledge a message so it gets redelivered.

```js
await queue.nack(msg.id);
```

### `subscribe(topic, subscriberId, callback)`

Subscribe to a pub/sub topic via SSE. Returns an unsubscribe function.

```js
const unsubscribe = queue.subscribe('events', 'worker-1', (message) => {
  console.log('Received:', message);
});

// Later, stop listening:
unsubscribe();
```

### `stats()`

Get broker statistics.

```js
const stats = await queue.stats();
```

### `metrics()`

Get Prometheus-format metrics as a raw string.

```js
const text = await queue.metrics();
```

### `health()`

Health check. Returns `true` if the server is healthy, `false` otherwise.

```js
if (await queue.health()) {
  console.log('Server is up');
}
```

## Authentication

Pass an API key via the constructor options:

```js
const queue = new BoltQClient('http://localhost:9090', {
  apiKey: 'my-secret-key',
});
```

## Error Handling

All methods throw `BoltQError` on failure, which includes:

- `statusCode` - HTTP status code (0 for network/timeout errors)
- `body` - Raw response body (if available)

```js
import { BoltQClient, BoltQError } from 'boltq-client';

try {
  await queue.publish('topic', 'data');
} catch (err) {
  if (err instanceof BoltQError) {
    console.error(err.statusCode, err.body);
  }
}
```

## License

MIT
