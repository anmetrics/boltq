# Configuration reference

Every messaging option, its default, and what changing it actually does.

The messaging block sits alongside the existing configuration:

```json
{
  "server":  { … },
  "storage": { … },
  "queue":   { … },
  "cluster": { … },
  "messaging": { … }
}
```

Everything under `messaging` is **off by default**. An existing queue-only
deployment upgrades with no behaviour change and no new files on disk.

## Validation

Configuration is validated at startup and a bad combination is a fatal error,
not a runtime surprise:

- `gateway.enabled` requires `stream.enabled`
- `gateway.enabled` requires `identity.enabled` — a public WebSocket endpoint
  cannot be authorised by a shared API key alone
- `push.enabled` requires `stream.enabled` and a `webhook_url`
- `identity.enabled` requires at least one key, each ≥ 32 bytes

## `messaging.stream`

| Option | Default | Notes |
|---|---|---|
| `enabled` | `false` | Master switch for the whole subsystem |
| `dir` | `<data_dir>/streams` | Stream data root |
| `default_partitions` | `16` | For implicitly created topics. **Cannot change after creation** — it would remap every key |
| `segment_bytes` | `268435456` (256MB) | Segment roll size. Larger = fewer files, coarser retention |
| `index_interval` | `4096` | Bytes between sparse index entries. Smaller = faster seeks, more RAM |
| `retention_bytes` | `0` (unlimited) | Per-partition size cap |
| `retention_age` | `0` (unlimited) | Max record age, e.g. `"8760h"` |
| `sync_on_append` | `false` | fsync every append. **Read [Durability](../architecture/durability.md) before leaving this off** |
| `maintenance_interval` | `"30s"` | Retention and periodic fsync cadence |

**On `default_partitions`:** pick with headroom. Partitions are cheap — two file
handles each — and resharding is not possible. 32 is a reasonable starting point
for a chat app; 16 is fine for a smaller one.

**On retention:** both caps default to unlimited because for chat the history
*is* the product. That makes disk growth your explicit decision. See
[Capacity planning](../operations/capacity.md).

## `messaging.identity`

| Option | Default | Notes |
|---|---|---|
| `enabled` | `false` | Turn on token verification |
| `keys` | `[]` | HMAC-SHA256 keys; ≥ 2 during rotation |
| `keys[].id` | — | Matches the token's `kid` header |
| `keys[].secret` | — | Key material. Avoid in production |
| `keys[].secret_env` | — | Environment variable holding it. **Prefer this** |
| `issuer` | `""` | When set, must match the token's `iss` |
| `leeway` | `"60s"` | Clock skew tolerance |
| `allow_anonymous` | `true` | Shared-API-key connections bypass the ACL |
| `membership_cache_ttl` | `"30s"` | How long a stale membership answer is reused |

**On `allow_anonymous`:** this preserves the trusted-backend model. It is safe
only while the API key stays server-side. Never ship it to a device.

**On `membership_cache_ttl`:** this is the window in which a removed user can
still act on a group. Short values cost backend queries; long ones delay
removals taking effect.

## `messaging.gateway`

| Option | Default | Notes |
|---|---|---|
| `enabled` | `false` | |
| `path` | `"/ws"` | HTTP route |
| `port` | `0` | `0` mounts on the admin port. **Set this in production** |
| `resume_window` | `"5m"` | How long a dropped session can be resumed |
| `pong_timeout` | `"90s"` | Socket declared dead after this without a pong |
| `max_subscriptions` | `200` | Per session; each costs a goroutine |
| `send_buffer` | `256` | Per-connection outbound queue depth |
| `read_limit` | `1048576` | Max inbound frame, bytes |
| `history_limit` | `200` | Max records per history request |
| `allowed_origins` | `[]` | Empty allows all. **Set it if browsers connect** |
| `tls` | disabled | Terminate here or upstream |

**On `port`:** sharing the admin port couples end-user traffic to the operator
plane — different exposure, different rate limits, different blast radius.
Separate them.

**On `send_buffer`:** when it fills, the client is disconnected rather than the
server blocked. That is deliberate: a client that has stopped reading must not
apply backpressure to everyone else. It reconnects and resumes from its cursor
with nothing lost. Raising this buys tolerance for brief stalls at the cost of
memory per connection.

**On `pong_timeout`:** mobile clients vanish without closing. An idle socket is
reaped by liveness probing, never by waiting for a FIN that will not arrive.

## `messaging.presence`

| Option | Default | Notes |
|---|---|---|
| `ttl` | `"90s"` | Session lifetime without a heartbeat |
| `sweep_interval` | `"15s"` | Expiry collection cadence |
| `region` | `""` | Locality label for routing hints |

**On `ttl`:** too short and a brief network stall evicts a healthy connection;
too long and messages route to a dead node until it lapses. Three missed
heartbeats is the usual rule — the gateway pings at `pong_timeout / 3`, so the
default 90s ttl against a 90s pong timeout gives roughly that.

## `messaging.chat`

| Option | Default | Notes |
|---|---|---|
| `fanout_on_write_limit` | `256` | Above this, members read the conversation directly |
| `conversation_partitions` | `16` | Partitions for conversation topics |
| `inbox_partitions` | `1` | Partitions per user inbox |
| `membership_url` | `""` | Your social-graph endpoint |
| `membership_timeout` | `"3s"` | Per lookup |
| `membership_auth_header` | `""` | Sent verbatim as `Authorization` |

**On `inbox_partitions`:** each user's inbox is its own topic, so one partition
is enough and keeps file handles proportional to users rather than users × N.

**On `membership_url`:** without it, group conversations are unavailable —
direct conversations still work because their members come from the conversation
ID. See [Fan-out](../architecture/fanout.md).

## `messaging.push`

| Option | Default | Notes |
|---|---|---|
| `enabled` | `false` | |
| `webhook_url` | `""` | Required when enabled |
| `auth_header` | `""` | Sent verbatim |
| `timeout` | `"10s"` | Per webhook call |
| `max_attempts` | `5` | Then the batch is dropped and the cursor advances |
| `grace_delay` | `"3s"` | Wait before pushing |
| `scan_interval` | `"30s"` | How often new inbox topics are discovered |

**On `max_attempts`:** bounded on purpose. Without a bound, one permanently bad
batch stalls every notification behind it.

**On `grace_delay`:** a user who opens the app within a couple of seconds should
read the message in the app, not get a notification for something they are
looking at.

## `messaging.dedup`

| Option | Default | Notes |
|---|---|---|
| `ttl` | `"10m"` | Must exceed your longest client retry window |
| `max_entries` | `0` (unlimited) | Memory cap |

**On `ttl`:** a phone that loses signal mid-send may retry minutes later. A TTL
shorter than that reopens the duplicate the table exists to prevent.

## `messaging.signals`

| Option | Default | Notes |
|---|---|---|
| `rate_per_second` | `5` | Sustained per-user budget |
| `burst` | `20` | Bucket depth |
| `max_payload` | `4096` | Signals are metadata, not content |
| `subscriber_buffer` | `64` | Queue depth before drops |

## Environment variables

Every important option has an override, for container deployments:

```
BOLTQ_STREAM_ENABLED              BOLTQ_GATEWAY_ENABLED
BOLTQ_STREAM_DIR                  BOLTQ_GATEWAY_PATH
BOLTQ_STREAM_SYNC_ON_APPEND       BOLTQ_GATEWAY_PORT
BOLTQ_STREAM_RETENTION_AGE        BOLTQ_GATEWAY_RESUME_WINDOW
                                  BOLTQ_GATEWAY_ALLOWED_ORIGINS
BOLTQ_IDENTITY_ENABLED
BOLTQ_IDENTITY_ISSUER             BOLTQ_PRESENCE_REGION
BOLTQ_IDENTITY_ALLOW_ANONYMOUS    BOLTQ_PRESENCE_TTL
BOLTQ_IDENTITY_KEY                ← setting this also enables identity
BOLTQ_IDENTITY_KEY_ID             BOLTQ_MEMBERSHIP_URL
                                  BOLTQ_MEMBERSHIP_AUTH_HEADER
BOLTQ_PUSH_ENABLED                BOLTQ_FANOUT_ON_WRITE_LIMIT
BOLTQ_PUSH_WEBHOOK_URL
BOLTQ_PUSH_AUTH_HEADER
BOLTQ_PUSH_GRACE_DELAY
```

`BOLTQ_IDENTITY_KEY` sets a single key entirely from the environment and turns
identity on — the common case for a container.

Durations accept Go duration strings (`"30s"`, `"5m"`, `"8760h"`).

## Minimal configurations

**Chat only, development:**

```json
{ "messaging": {
    "stream":   { "enabled": true },
    "identity": { "enabled": true,
                  "keys": [{ "id": "dev", "secret_env": "BOLTQ_KEY" }] },
    "gateway":  { "enabled": true, "port": 9095 }
} }
```

**Production additions:** `sync_on_append`, a retention policy,
`allowed_origins`, TLS, `membership_url`, `push`, and a decision about
`allow_anonymous`.

## Further reading

- [Production checklist](../operations/production-checklist.md).
- [Durability](../architecture/durability.md) — the `sync_on_append` decision.
- [Capacity planning](../operations/capacity.md) — sizing.
