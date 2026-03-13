# Go SDK

The Go SDK provides a simple client for interacting with BoltQ server over HTTP.

## Installation

```bash
go get github.com/boltq/boltq/client/golang
```

## Import

```go
import boltq "github.com/boltq/boltq/client/golang"
```

## Client Creation

```go
// Basic
client := boltq.New("http://localhost:9090")

// With API key
client := boltq.New("http://localhost:9090", boltq.WithAPIKey("my-secret"))

// With custom timeout
client := boltq.New("http://localhost:9090", boltq.WithTimeout(30 * time.Second))

### Options

- `WithAPIKey(string)`: Sets the API key.
- `WithTimeout(time.Duration)`: Sets connection timeout.
- `WithTLS(*tls.Config)`: Enables TLS encryption.

```go
client := boltq.New("addr:9091", boltq.WithTLS(&tls.Config{...}))
```

// Combined
client := boltq.New("http://localhost:9090",
    boltq.WithAPIKey("my-secret"),
    boltq.WithTimeout(30 * time.Second),
)
```

## API

### Publish

Publish a message to a work queue (1 message → 1 consumer).

```go
id, err := client.Publish("email_jobs", map[string]interface{}{
    "to":      "user@example.com",
    "subject": "Welcome",
    "body":    "Hello!",
}, map[string]string{
    "priority": "high",
})
```

**Parameters:**
| Param | Type | Description |
|-------|------|-------------|
| `topic` | `string` | Queue name |
| `payload` | `interface{}` | Any JSON-serializable value |
| `headers` | `map[string]string` | Optional metadata (can be `nil`) |

**Returns:** `(string, error)` — message ID and error

### PublishTopic

Publish a message to a pub/sub topic (1 message → all subscribers).

```go
id, err := client.PublishTopic("user_signup", map[string]string{
    "user_id": "42",
    "email":   "new@user.com",
}, nil)
```

Same parameters and return as `Publish`.

### Consume

Consume a message from a work queue (non-blocking).

```go
msg, err := client.Consume("email_jobs")
if err != nil {
    log.Fatal(err)
}
if msg == nil {
    fmt.Println("No messages available")
    return
}

fmt.Printf("ID: %s\n", msg.ID)
fmt.Printf("Topic: %s\n", msg.Topic)
fmt.Printf("Payload: %s\n", msg.Payload)
fmt.Printf("Headers: %v\n", msg.Headers)
fmt.Printf("Retry: %d\n", msg.Retry)
```

**Returns:** `(*Message, error)` — `nil` message means no messages available

### Message Type

```go
type Message struct {
    ID        string            `json:"id"`
    Topic     string            `json:"topic"`
    Payload   json.RawMessage   `json:"payload"`
    Headers   map[string]string `json:"headers,omitempty"`
    Timestamp int64             `json:"timestamp"`
    Retry     int               `json:"retry"`
}
```

### Ack

Acknowledge a consumed message.

```go
err := client.Ack(msg.ID)
```

### Nack

Negatively acknowledge a message (triggers retry).

```go
err := client.Nack(msg.ID)
```

### Stats

Get broker statistics.

```go
stats, err := client.Stats()
// stats is map[string]interface{}
```

### Health

Check server health.

```go
err := client.Health()
if err != nil {
    fmt.Println("Server is down:", err)
}
```

## Complete Examples

### Worker Pattern

Process jobs from a queue with error handling:

```go
package main

import (
    "encoding/json"
    "log"
    "time"

    boltq "github.com/boltq/boltq/client/golang"
)

type EmailJob struct {
    To      string `json:"to"`
    Subject string `json:"subject"`
    Body    string `json:"body"`
}

func main() {
    client := boltq.New("http://localhost:9090")

    for {
        msg, err := client.Consume("email_jobs")
        if err != nil {
            log.Printf("consume error: %v", err)
            time.Sleep(time.Second)
            continue
        }

        if msg == nil {
            time.Sleep(100 * time.Millisecond)
            continue
        }

        var job EmailJob
        if err := json.Unmarshal(msg.Payload, &job); err != nil {
            log.Printf("bad payload: %v", err)
            client.Nack(msg.ID) // send to retry
            continue
        }

        if err := sendEmail(job); err != nil {
            log.Printf("send failed: %v", err)
            client.Nack(msg.ID) // retry
            continue
        }

        client.Ack(msg.ID) // success
        log.Printf("sent email to %s", job.To)
    }
}

func sendEmail(job EmailJob) error {
    // Your email sending logic
    return nil
}
```

### Producer Pattern

Enqueue jobs from an API handler:

```go
package main

import (
    "encoding/json"
    "log"
    "net/http"

    boltq "github.com/boltq/boltq/client/golang"
)

var queue = boltq.New("http://localhost:9090")

func signupHandler(w http.ResponseWriter, r *http.Request) {
    // ... create user ...

    // Enqueue welcome email
    _, err := queue.Publish("email_jobs", map[string]string{
        "to":      "newuser@example.com",
        "subject": "Welcome!",
    }, nil)
    if err != nil {
        log.Printf("enqueue failed: %v", err)
    }

    // Notify all subscribers of new signup
    queue.PublishTopic("user_signup", map[string]string{
        "user_id": "42",
    }, nil)

    json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
}

func main() {
    http.HandleFunc("/signup", signupHandler)
    log.Fatal(http.ListenAndServe(":8080", nil))
}
```

### Multi-Worker Pool

Run multiple concurrent workers:

```go
package main

import (
    "log"
    "sync"
    "time"

    boltq "github.com/boltq/boltq/client/golang"
)

func worker(id int, client *boltq.Client, topic string) {
    for {
        msg, err := client.Consume(topic)
        if err != nil {
            time.Sleep(time.Second)
            continue
        }
        if msg == nil {
            time.Sleep(50 * time.Millisecond)
            continue
        }

        log.Printf("[worker-%d] processing %s", id, msg.ID)
        // Process message...
        client.Ack(msg.ID)
    }
}

func main() {
    client := boltq.New("http://localhost:9090")

    var wg sync.WaitGroup
    for i := 0; i < 8; i++ {
        wg.Add(1)
        go func(id int) {
            defer wg.Done()
            worker(id, client, "email_jobs")
        }(i)
    }

    wg.Wait()
}
```
