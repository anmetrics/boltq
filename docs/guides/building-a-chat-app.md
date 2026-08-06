# Building a chat app

End to end: configuration, a membership service, and a working client. The
example is a dating app — mostly 1:1 conversations — but the shape is the same
for group chat.

## 1. Configure the server

`configs/chat.json`:

```json
{
  "server":  { "http_port": 9090, "tcp_port": 9091, "host": "0.0.0.0" },
  "storage": { "mode": "disk", "data_dir": "/var/lib/boltq" },

  "messaging": {
    "stream": {
      "enabled": true,
      "default_partitions": 32,
      "segment_bytes": 268435456,
      "sync_on_append": true,
      "maintenance_interval": "30s"
    },
    "identity": {
      "enabled": true,
      "issuer": "auth.example.com",
      "allow_anonymous": true,
      "keys": [ { "id": "2025-q1", "secret_env": "BOLTQ_SIGNING_KEY" } ],
      "membership_cache_ttl": "30s"
    },
    "gateway": {
      "enabled": true,
      "path": "/ws",
      "port": 9095,
      "resume_window": "5m",
      "pong_timeout": "90s",
      "max_subscriptions": 200,
      "allowed_origins": ["https://app.example.com"]
    },
    "presence": { "ttl": "90s", "region": "eu-west-1" },
    "chat": {
      "fanout_on_write_limit": 256,
      "conversation_partitions": 32,
      "membership_url": "https://api.example.com/boltq/membership"
    },
    "push": {
      "enabled": true,
      "webhook_url": "https://api.example.com/boltq/push",
      "auth_header": "Bearer <service-token>",
      "grace_delay": "3s"
    },
    "dedup":   { "ttl": "10m" },
    "signals": { "rate_per_second": 5, "burst": 20 }
  }
}
```

```bash
export BOLTQ_SIGNING_KEY="$(openssl rand -hex 32)"
./bin/boltq-server -config configs/chat.json
```

Two settings deserve a decision rather than a default:

**`sync_on_append: true`** — because the stream log is not yet replicated,
fsync is your durability story. Read
[Durability](../architecture/durability.md) before turning it off.

**`gateway.port: 9095`** — a separate listener from the admin API. End-user
traffic and operator traffic have different exposure and different blast radii;
do not share a port in production.

## 2. Mint tokens

Your auth service issues a token when a user signs in:

```go
func issueToken(userID, deviceID string) (string, error) {
    return identity.Sign(
        identity.SigningKey{ID: "2025-q1", Secret: signingKey},
        identity.Claims{
            Subject:   userID,
            DeviceID:  deviceID,
            Issuer:    "auth.example.com",
            IssuedAt:  time.Now().Unix(),
            ExpiresAt: time.Now().Add(time.Hour).Unix(),
        },
    )
}
```

`deviceID` must be **stable for the life of the install**. Derive it from
platform identifiers plus a value you persist in the keychain — not from a
random value regenerated at launch, which would create a new cursor every time
the app starts.

## 3. Serve membership

Two endpoints. For a purely 1:1 app you can skip this entirely — conversation
IDs of the form `alice:bob` resolve their own members — but you need it as soon
as you have group chats.

```go
func isMember(w http.ResponseWriter, r *http.Request) {
    q := r.URL.Query()
    ok := db.IsMember(q.Get("tenant"), q.Get("user"), q.Get("group"))
    json.NewEncoder(w).Encode(map[string]bool{"member": ok})
}

func members(w http.ResponseWriter, r *http.Request) {
    q := r.URL.Query()
    ids := db.GroupMembers(q.Get("tenant"), q.Get("group"))
    json.NewEncoder(w).Encode(map[string][]string{"members": ids})
}
```

`is-member` is called on **every** send and subscribe, so it must be a fast
point lookup. Index it accordingly; BoltQ caches for 30s but the first call
after a cache miss is on the user's send path.

## 4. Conversation IDs

For 1:1, sort the two user IDs and join them:

```go
func directConversationID(a, b string) string {
    if a > b { a, b = b, a }
    return a + ":" + b
}
```

Sorting matters — both participants must derive the same ID, or they end up in
two different conversations that each contain half the messages.

## 5. The client

```javascript
class ChatClient {
  constructor(baseURL, getToken) {
    this.baseURL = baseURL;
    this.getToken = getToken;
    this.resumeToken = localStorage.getItem('boltq.resume');
    this.deviceID = getStableDeviceID();
    this.pending = new Map();   // request id -> resolver
    this.seen = new Set();      // message ids, for dedup
    this.backoff = 500;
    this.nextID = 0;
  }

  async connect() {
    const token = await this.getToken();
    this.ws = new WebSocket(`${this.baseURL}/ws?token=${token}`);

    this.ws.onopen = () => {
      this.request({
        op: 'hello',
        version: 1,
        device_id: this.deviceID,
        resume: this.resumeToken || undefined,
      }).then(w => {
        // Store the resume token BEFORE anything else can fail.
        this.resumeToken = w.token;
        localStorage.setItem('boltq.resume', w.token);
        this.backoff = 500;
        if (!w.resumed) this.resubscribeAll();
      });
    };

    this.ws.onmessage = e => this.handle(JSON.parse(e.data));

    this.ws.onclose = () => {
      // Exponential backoff with jitter. Without jitter, every client
      // reconnects in lockstep after an outage and takes the server down again.
      const delay = this.backoff * (0.5 + Math.random());
      this.backoff = Math.min(this.backoff * 2, 30000);
      setTimeout(() => this.connect(), delay);
    };
  }

  request(frame) {
    const id = `r${this.nextID++}`;
    return new Promise((resolve, reject) => {
      this.pending.set(id, { resolve, reject });
      this.ws.send(JSON.stringify({ ...frame, id }));
    });
  }

  handle(f) {
    // Correlated responses.
    if (f.id && this.pending.has(f.id)) {
      const { resolve, reject } = this.pending.get(f.id);
      this.pending.delete(f.id);
      if (f.op === 'error') reject(f.error); else resolve(f);
      if (f.op !== 'record') return;
    }

    switch (f.op) {
      case 'record':          this.onRecords(f); break;
      case 'gap':             this.onGap(f);     break;
      case 'signal':          this.onSignal(f);  break;
      case 'presence_event':  this.onPresence(f); break;
    }
  }

  onRecords(f) {
    // Sort by seq — never by arrival order or timestamp.
    const records = [...f.records].sort((a, b) => a.seq - b.seq);

    for (const r of records) {
      const id = r.headers?.message_id;
      if (id && this.seen.has(id)) continue;   // at-least-once delivery
      if (id) this.seen.add(id);
      this.ui.append(decodeMessage(r));
    }

    // Commit after rendering, as lastSeq + 1.
    const last = records[records.length - 1];
    this.commitDebounced(f.topic, f.partition, last.seq + 1);
  }

  onGap(f) {
    // Retention removed history below our cursor. Say so; do not pretend
    // the conversation is complete.
    this.ui.showHistoryUnavailable(f.topic, f.first_seq);
  }

  async send(conversationID, text) {
    const clientMsgID = crypto.randomUUID();
    // Render optimistically, keyed by clientMsgID so the ack can reconcile it.
    this.ui.appendPending(clientMsgID, text);

    const res = await this.request({
      op: 'send',
      kind: 'direct',
      conversation: conversationID,
      client_msg_id: clientMsgID,        // idempotency key — always send it
      payload: btoa(text),
    });
    // duplicate:true means this was a retry; the coordinates are the
    // original's, so the same optimistic bubble resolves correctly.
    this.ui.resolvePending(clientMsgID, res.sent);
  }

  async openConversation(conversationID) {
    const topic = `chat.direct.${conversationID}`;
    // Omit from_seq: the server resumes from this device's committed cursor.
    await this.request({ op: 'subscribe', topic });
    // Register for typing indicators without claiming to be typing.
    await this.request({ op: 'typing', conversation: conversationID, typing: false });
  }
}
```

### Commit debouncing

```javascript
commitDebounced(topic, partition, seq) {
  const key = `${topic}/${partition}`;
  this.pendingCommits.set(key, seq);
  clearTimeout(this.commitTimer);
  // Committing per message is wasteful; the cost of a lost commit is a
  // redelivery, which dedup absorbs.
  this.commitTimer = setTimeout(() => {
    for (const [k, s] of this.pendingCommits) {
      const [topic, partition] = k.split('/');
      this.request({ op: 'commit', topic, partition: +partition, seq: s });
    }
    this.pendingCommits.clear();
  }, 500);
}
```

## 6. Receive push notifications

```go
func pushWebhook(w http.ResponseWriter, r *http.Request) {
    var payload struct {
        Notifications []struct {
            UserID         string `json:"user_id"`
            ConversationID string `json:"conversation_id"`
            SenderID       string `json:"sender_id"`
            ConvSeq        uint64 `json:"conv_seq"`
            Attempt        int    `json:"attempt"`
        } `json:"notifications"`
    }
    json.NewDecoder(r.Body).Decode(&payload)

    for _, n := range payload.Notifications {
        // Delivery is at-least-once: a crash between sending and committing
        // the cursor re-presents a batch. Deduplicate.
        if alreadySent(n.UserID, n.ConversationID, n.ConvSeq) { continue }
        sendAPNs(n.UserID, buildAlert(n))
    }
    w.WriteHeader(http.StatusOK)
}
```

Return non-2xx to make BoltQ retry with backoff. After `max_attempts` the batch
is dropped and the cursor advances — a permanently failing batch must not stall
every notification behind it.

## 7. Verify

```bash
curl localhost:9090/messaging/overview | jq
curl 'localhost:9090/streams/topic?name=chat.direct.alice:bob' | jq
curl 'localhost:9090/streams/cursors?topic=chat.inbox.bob&group=push-dispatcher' | jq
```

The third is the one to watch: `lag` is how far push notifications are behind
the log. Growing lag means the webhook is failing or too slow.

## Checklist before launch

- [ ] `sync_on_append` decided deliberately, not left at the default
- [ ] Gateway on its own port, TLS terminated
- [ ] `allowed_origins` set if browsers connect
- [ ] Shared API key never shipped to a device
- [ ] Token lifetime ≤ 1 hour, client refreshes before expiry
- [ ] Device IDs stable across app restarts
- [ ] Client sorts by `seq`, dedups by `message_id`, handles `gap`
- [ ] Reconnect uses backoff **with jitter**
- [ ] Push webhook is idempotent
- [ ] Retention policy chosen — the default keeps everything forever
- [ ] Backups configured (there is no replication)

See the [production checklist](../operations/production-checklist.md) for the
full version.
