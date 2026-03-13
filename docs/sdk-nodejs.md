# Node.js SDK

The Node.js SDK provides a simple client for interacting with BoltQ server over HTTP. Zero dependencies — uses native `fetch`.

## Requirements

- Node.js >= 18 (native `fetch` required)

## Installation

```bash
# Copy from project
cp -r client/nodejs ./node_modules/boltq-client

# Or link for development
cd client/nodejs && npm link
cd your-project && npm link boltq-client
```

## Import

```javascript
import { BoltQClient } from 'boltq-client'
```

## Client Creation

```javascript
// Basic
const queue = new BoltQClient('http://localhost:9090')

// With API key
const queue = new BoltQClient('http://localhost:9090', {
  apiKey: 'my-secret-key'
})

// With custom timeout
const queue = new BoltQClient('http://localhost:9090', {
  timeout: 30000 // milliseconds
})

// Combined
const queue = new BoltQClient('http://localhost:9090', {
  apiKey: 'my-secret-key',
  timeout: 30000
})
```

## API

### publish(topic, payload, headers?)

Publish a message to a work queue.

```javascript
const { id, topic } = await queue.publish('email_jobs', {
  to: 'user@example.com',
  subject: 'Welcome'
})
console.log(`Published: ${id}`)
```

| Param | Type | Description |
|-------|------|-------------|
| `topic` | `string` | Queue name |
| `payload` | `any` | Any JSON-serializable value |
| `headers` | `object` | Optional key-value metadata |

Returns: `Promise<{ id: string, topic: string }>`

### publishTopic(topic, payload, headers?)

Publish to a pub/sub topic (all subscribers receive).

```javascript
const { id } = await queue.publishTopic('user_signup', {
  userId: '42',
  email: 'new@user.com'
})
```

### consume(topic)

Consume a message from a work queue (non-blocking).

```javascript
const msg = await queue.consume('email_jobs')

if (msg) {
  console.log(msg.id)        // "abc123..."
  console.log(msg.topic)     // "email_jobs"
  console.log(msg.payload)   // { to: "...", subject: "..." }
  console.log(msg.headers)   // { priority: "high" }
  console.log(msg.timestamp) // 1706900000000000000
  console.log(msg.retry)     // 0
} else {
  console.log('No messages available')
}
```

Returns: `Promise<Message | null>`

### ack(messageId)

Acknowledge a consumed message.

```javascript
await queue.ack(msg.id)
```

### nack(messageId)

Negatively acknowledge (triggers retry or dead-letter).

```javascript
await queue.nack(msg.id)
```

### subscribe(topic, subscriberId, callback)

Subscribe to a pub/sub topic via SSE.

```javascript
const unsubscribe = queue.subscribe('user_signup', 'my-sub-1', (msg) => {
  console.log('Received:', msg.payload)
})

// Later, to stop:
unsubscribe()
```

| Param | Type | Description |
|-------|------|-------------|
| `topic` | `string` | Topic name |
| `subscriberId` | `string` | Unique subscriber ID |
| `callback` | `function` | Called with each message |

Returns: `() => void` (unsubscribe function)

### stats()

Get broker statistics.

```javascript
const stats = await queue.stats()
console.log(stats.Queues)      // { email_jobs: 42 }
console.log(stats.PendingCount) // 5
```

### health()

Check server health.

```javascript
const isHealthy = await queue.health()
// true or false
```

## Message Type

```typescript
interface Message {
  id: string
  topic: string
  payload: any
  headers?: Record<string, string>
  timestamp: number
  retry: number
}
```

## Complete Examples

### Worker Pattern

```javascript
import { BoltQClient } from 'boltq-client'

const queue = new BoltQClient('http://localhost:9090')

async function processJobs() {
  while (true) {
    try {
      const msg = await queue.consume('email_jobs')

      if (!msg) {
        await sleep(100) // no messages, wait a bit
        continue
      }

      console.log(`Processing: ${msg.id}`)

      try {
        await sendEmail(msg.payload)
        await queue.ack(msg.id)
        console.log(`Done: ${msg.id}`)
      } catch (err) {
        console.error(`Failed: ${msg.id}`, err)
        await queue.nack(msg.id) // retry
      }
    } catch (err) {
      console.error('Consumer error:', err)
      await sleep(1000)
    }
  }
}

function sleep(ms) {
  return new Promise(resolve => setTimeout(resolve, ms))
}

async function sendEmail(payload) {
  // Your email logic here
}

processJobs()
```

### Express.js Producer

```javascript
import express from 'express'
import { BoltQClient } from 'boltq-client'

const app = express()
const queue = new BoltQClient('http://localhost:9090')

app.use(express.json())

app.post('/signup', async (req, res) => {
  const { email, name } = req.body

  // Create user...

  // Enqueue welcome email
  const { id } = await queue.publish('email_jobs', {
    to: email,
    subject: `Welcome ${name}!`,
    template: 'welcome'
  })

  // Notify subscribers
  await queue.publishTopic('user_signup', {
    email, name, timestamp: Date.now()
  })

  res.json({ status: 'ok', emailJobId: id })
})

app.listen(3000)
```

### Pub/Sub Listener

```javascript
import { BoltQClient } from 'boltq-client'

const queue = new BoltQClient('http://localhost:9090')

// Listen for user signups
const unsubscribe = queue.subscribe('user_signup', 'analytics-service', (msg) => {
  console.log('New signup:', msg.payload)
  // Update analytics, send to data warehouse, etc.
})

// Cleanup on exit
process.on('SIGINT', () => {
  unsubscribe()
  process.exit(0)
})
```

### Concurrent Workers

```javascript
import { BoltQClient } from 'boltq-client'

const queue = new BoltQClient('http://localhost:9090')
const WORKER_COUNT = 4

async function worker(id, topic) {
  console.log(`Worker ${id} started on ${topic}`)
  while (true) {
    const msg = await queue.consume(topic)
    if (!msg) {
      await new Promise(r => setTimeout(r, 50))
      continue
    }
    try {
      console.log(`[worker-${id}] processing ${msg.id}`)
      // Process...
      await queue.ack(msg.id)
    } catch {
      await queue.nack(msg.id)
    }
  }
}

// Launch workers
for (let i = 0; i < WORKER_COUNT; i++) {
  worker(i, 'email_jobs')
}
```
