package config

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"time"
)

// MessagingConfig groups everything the chat/streaming subsystem needs.
//
// It is a separate block from the existing queue configuration because the two
// are independent: a deployment can run BoltQ purely as a work queue, purely
// as a chat backbone, or as both, and neither should force configuration of
// the other.
type MessagingConfig struct {
	Stream   StreamConfig   `json:"stream"`
	Identity IdentityConfig `json:"identity"`
	Presence PresenceConfig `json:"presence"`
	Gateway  GatewayConfig  `json:"gateway"`
	Chat     ChatConfig     `json:"chat"`
	Push     PushConfig     `json:"push"`
	Dedup    DedupConfig    `json:"dedup"`
	Signals  SignalsConfig  `json:"signals"`
	// Replication copies the stream log to other nodes. Without it, a node's
	// messages exist only on that node's disk.
	Replication ReplicationConfig `json:"replication"`
}

// ReplicationConfig configures stream log replication.
//
// Leadership is static: this node either leads the partitions it accepts writes
// for, or follows a leader for them. There is no automatic failover, because
// electing a leader without consensus can produce two leaders assigning
// conflicting sequences to the same log — the one failure a replicated log must
// never have. Promotion is a deliberate operator action; see
// docs/operations/global-ha.md.
type ReplicationConfig struct {
	Enabled bool `json:"enabled"`

	// Role is "leader" or "follower".
	Role string `json:"role"`

	// Listen is the leader's address for follower connections. Internal plane
	// only — never expose it publicly.
	Listen string `json:"listen"`

	// LeaderAddr is the address a follower connects to.
	LeaderAddr string `json:"leader_addr"`

	// Secret authenticates the replication link. A replication connection can
	// read every message in the log, so leaving this empty is only acceptable
	// on a trusted private network.
	Secret string `json:"secret,omitempty"`
	// SecretEnv names an environment variable holding the secret. Prefer it.
	SecretEnv string `json:"secret_env,omitempty"`

	// MinInSync is how many replicas, counting the leader, must hold a record
	// before an append is acknowledged. 1 replicates asynchronously; 2 means an
	// acknowledged message survives losing one node.
	MinInSync int `json:"min_in_sync"`

	// AckTimeout bounds how long an append waits for replicas.
	AckTimeout Duration `json:"ack_timeout"`

	// MaxLagInSync is how far a follower may fall behind and still count
	// toward quorum.
	MaxLagInSync uint64 `json:"max_lag_in_sync"`

	// Topics a follower replicates. Each entry is "topic:partitionCount", and
	// every partition of that topic is fetched.
	Topics []string `json:"topics"`

	// SyncOnApply makes a follower fsync before acknowledging, so its ack
	// means "on disk" rather than "in page cache".
	SyncOnApply bool `json:"sync_on_apply"`
}

// ResolveSecret returns the replication secret, preferring the environment.
func (r ReplicationConfig) ResolveSecret() (string, error) {
	if r.SecretEnv != "" {
		v := os.Getenv(r.SecretEnv)
		if v == "" {
			return "", fmt.Errorf("replication secret env %s is empty", r.SecretEnv)
		}
		return v, nil
	}
	return r.Secret, nil
}

// StreamConfig configures the partitioned log.
type StreamConfig struct {
	// Enabled turns the whole streaming subsystem on. Off by default so an
	// existing queue-only deployment upgrades without new files appearing in
	// its data directory.
	Enabled bool `json:"enabled"`
	// Dir holds stream data. Defaults to <storage.data_dir>/streams.
	Dir string `json:"dir"`
	// DefaultPartitions is the partition count for implicitly created topics.
	DefaultPartitions int32 `json:"default_partitions"`
	// SegmentBytes is the size at which a segment is rolled.
	SegmentBytes int64 `json:"segment_bytes"`
	// IndexInterval is the byte gap between sparse index entries.
	IndexInterval int64 `json:"index_interval"`
	// RetentionBytes caps a partition's size. Zero keeps everything, which is
	// usually right for chat: the history is the product.
	RetentionBytes int64 `json:"retention_bytes"`
	// RetentionAge caps record age, e.g. "8760h" for a year.
	RetentionAge Duration `json:"retention_age"`
	// SyncOnAppend fsyncs every append. See docs/architecture/durability.md
	// before turning this on.
	SyncOnAppend bool `json:"sync_on_append"`
	// MaintenanceInterval is how often retention and periodic fsync run.
	MaintenanceInterval Duration `json:"maintenance_interval"`
}

// IdentityConfig configures per-user authentication and authorisation.
type IdentityConfig struct {
	// Enabled turns on token verification. When off, the gateway falls back to
	// the shared API key and every connection is a trusted backend.
	Enabled bool `json:"enabled"`
	// Keys are HMAC-SHA256 signing keys. Provide at least two during rotation.
	Keys []SigningKeyConfig `json:"keys"`
	// Issuer, when set, must match the token's iss claim.
	Issuer string `json:"issuer"`
	// Leeway tolerates clock skew against the token issuer.
	Leeway Duration `json:"leeway"`
	// AllowAnonymous lets shared-API-key connections bypass the ACL. Keep it
	// on for backend services, and never expose that key to a device.
	AllowAnonymous bool `json:"allow_anonymous"`
	// Policy replaces the built-in chat rules when non-empty.
	Policy []json.RawMessage `json:"policy,omitempty"`
	// MembershipCacheTTL bounds how long a stale group membership answer is
	// reused. Short values cost backend queries; long ones delay removals.
	MembershipCacheTTL Duration `json:"membership_cache_ttl"`
}

// SigningKeyConfig is one HMAC key.
type SigningKeyConfig struct {
	ID string `json:"id"`
	// Secret is the key material. Prefer SecretEnv in production so the key
	// never lands in a config file that gets committed or shipped in an image.
	Secret string `json:"secret,omitempty"`
	// SecretEnv names an environment variable holding the secret.
	SecretEnv string `json:"secret_env,omitempty"`
}

// Resolve returns the key material, preferring the environment variable.
func (k SigningKeyConfig) Resolve() (string, error) {
	if k.SecretEnv != "" {
		v := os.Getenv(k.SecretEnv)
		if v == "" {
			return "", fmt.Errorf("signing key %q: env %s is empty", k.ID, k.SecretEnv)
		}
		return v, nil
	}
	if k.Secret == "" {
		return "", fmt.Errorf("signing key %q has no secret", k.ID)
	}
	return k.Secret, nil
}

// PresenceConfig configures the presence registry.
type PresenceConfig struct {
	// TTL is how long a session survives without a heartbeat.
	TTL Duration `json:"ttl"`
	// SweepInterval is how often lapsed sessions are collected.
	SweepInterval Duration `json:"sweep_interval"`
	// Region labels this node for locality-aware routing.
	Region string `json:"region"`
}

// GatewayConfig configures the WebSocket edge.
type GatewayConfig struct {
	Enabled bool `json:"enabled"`
	// Path is the HTTP route the gateway is mounted on.
	Path string `json:"path"`
	// Port serves the gateway on its own listener. Zero mounts it on the
	// existing HTTP admin port instead, which is simpler but couples the
	// end-user traffic plane to the admin plane — separate them in production.
	Port int `json:"port"`
	// ResumeWindow is how long a dropped session can be resumed.
	ResumeWindow Duration `json:"resume_window"`
	// PongTimeout declares a socket dead after this long without a pong.
	PongTimeout Duration `json:"pong_timeout"`
	// MaxSubscriptions caps concurrent subscriptions per session.
	MaxSubscriptions int `json:"max_subscriptions"`
	// SendBuffer bounds the per-connection outbound queue.
	SendBuffer int `json:"send_buffer"`
	// ReadLimit caps an inbound frame in bytes.
	ReadLimit int64 `json:"read_limit"`
	// HistoryLimit caps records returned by one history request.
	HistoryLimit int `json:"history_limit"`
	// AllowedOrigins restricts browser origins. Empty allows all, which is
	// correct only when no browser client connects.
	AllowedOrigins []string `json:"allowed_origins"`
	// TLS secures the gateway listener. End-user traffic must be encrypted;
	// terminate here or at a proxy in front.
	TLS TLSConfig `json:"tls"`
}

// ChatConfig configures conversation fan-out.
type ChatConfig struct {
	// FanoutOnWriteLimit is the largest conversation that still receives
	// per-member inbox pointers.
	FanoutOnWriteLimit int `json:"fanout_on_write_limit"`
	// ConversationPartitions is the partition count for conversation topics.
	ConversationPartitions int32 `json:"conversation_partitions"`
	// InboxPartitions is the partition count for per-user inbox topics.
	InboxPartitions int32 `json:"inbox_partitions"`
	// MembershipURL is an HTTP endpoint answering group membership and member
	// listing. Without it, BoltQ has no social graph and group conversations
	// cannot be authorised.
	MembershipURL string `json:"membership_url"`
	// MembershipTimeout bounds a membership lookup.
	MembershipTimeout Duration `json:"membership_timeout"`
	// MembershipAuthHeader is sent verbatim on membership requests.
	MembershipAuthHeader string `json:"membership_auth_header"`
}

// PushConfig configures offline notification dispatch.
type PushConfig struct {
	Enabled bool `json:"enabled"`
	// WebhookURL receives batches of notifications for offline users. The
	// application forwards them to APNs, FCM or whatever it uses.
	WebhookURL string `json:"webhook_url"`
	// AuthHeader is sent verbatim on webhook requests.
	AuthHeader string `json:"auth_header"`
	// Timeout bounds one webhook call.
	Timeout Duration `json:"timeout"`
	// MaxAttempts bounds retries before a batch is dropped.
	MaxAttempts int `json:"max_attempts"`
	// GraceDelay waits before pushing, giving an app that is opening a moment
	// to display the message itself.
	GraceDelay Duration `json:"grace_delay"`
	// ScanInterval is how often new inbox topics are discovered.
	ScanInterval Duration `json:"scan_interval"`
}

// DedupConfig configures the idempotency window.
type DedupConfig struct {
	// TTL must exceed the longest client retry window.
	TTL Duration `json:"ttl"`
	// MaxEntries caps memory. Zero is unlimited.
	MaxEntries int `json:"max_entries"`
}

// SignalsConfig configures ephemeral signals.
type SignalsConfig struct {
	// RatePerSecond is the sustained per-user signal budget.
	RatePerSecond float64 `json:"rate_per_second"`
	// Burst is the per-user bucket depth.
	Burst float64 `json:"burst"`
	// MaxPayload caps a signal body in bytes.
	MaxPayload int `json:"max_payload"`
	// SubscriberBuffer bounds a subscriber's queue before drops begin.
	SubscriberBuffer int `json:"subscriber_buffer"`
}

// Duration is a time.Duration that marshals as a Go duration string ("30s"),
// so config files stay readable instead of carrying raw nanosecond counts.
type Duration time.Duration

// D returns the underlying time.Duration.
func (d Duration) D() time.Duration { return time.Duration(d) }

// Or returns the duration, or def when unset.
func (d Duration) Or(def time.Duration) time.Duration {
	if d == 0 {
		return def
	}
	return time.Duration(d)
}

// MarshalJSON implements json.Marshaler.
func (d Duration) MarshalJSON() ([]byte, error) {
	return json.Marshal(time.Duration(d).String())
}

// UnmarshalJSON accepts either a duration string or a nanosecond number.
func (d *Duration) UnmarshalJSON(b []byte) error {
	var s string
	if err := json.Unmarshal(b, &s); err == nil {
		v, err := time.ParseDuration(s)
		if err != nil {
			return fmt.Errorf("parse duration %q: %w", s, err)
		}
		*d = Duration(v)
		return nil
	}
	var n int64
	if err := json.Unmarshal(b, &n); err != nil {
		return fmt.Errorf("duration must be a string like \"30s\" or a nanosecond count")
	}
	*d = Duration(n)
	return nil
}

// DefaultMessaging returns messaging defaults. The subsystem is off unless
// explicitly enabled.
func DefaultMessaging() MessagingConfig {
	return MessagingConfig{
		Stream: StreamConfig{
			Enabled:             false,
			DefaultPartitions:   16,
			SegmentBytes:        256 << 20,
			IndexInterval:       4 << 10,
			MaintenanceInterval: Duration(30 * time.Second),
		},
		Identity: IdentityConfig{
			Enabled:            false,
			Leeway:             Duration(60 * time.Second),
			AllowAnonymous:     true,
			MembershipCacheTTL: Duration(30 * time.Second),
		},
		Presence: PresenceConfig{
			TTL:           Duration(90 * time.Second),
			SweepInterval: Duration(15 * time.Second),
		},
		Gateway: GatewayConfig{
			Enabled:          false,
			Path:             "/ws",
			ResumeWindow:     Duration(5 * time.Minute),
			PongTimeout:      Duration(90 * time.Second),
			MaxSubscriptions: 200,
			SendBuffer:       256,
			ReadLimit:        1 << 20,
			HistoryLimit:     200,
		},
		Chat: ChatConfig{
			FanoutOnWriteLimit:     256,
			ConversationPartitions: 16,
			InboxPartitions:        1,
			MembershipTimeout:      Duration(3 * time.Second),
		},
		Push: PushConfig{
			Enabled:      false,
			Timeout:      Duration(10 * time.Second),
			MaxAttempts:  5,
			GraceDelay:   Duration(3 * time.Second),
			ScanInterval: Duration(30 * time.Second),
		},
		Dedup: DedupConfig{
			TTL: Duration(10 * time.Minute),
		},
		Signals: SignalsConfig{
			RatePerSecond:    5,
			Burst:            20,
			MaxPayload:       4 << 10,
			SubscriberBuffer: 64,
		},
		Replication: ReplicationConfig{
			Enabled:      false,
			MinInSync:    1,
			AckTimeout:   Duration(5 * time.Second),
			MaxLagInSync: 10000,
		},
	}
}

// Validate checks the messaging configuration for combinations that would fail
// at runtime, so a misconfiguration is a startup error rather than a 3am page.
func (m *MessagingConfig) Validate() error {
	if m.Gateway.Enabled && !m.Stream.Enabled {
		return fmt.Errorf("messaging.gateway requires messaging.stream.enabled")
	}
	if m.Push.Enabled && !m.Stream.Enabled {
		return fmt.Errorf("messaging.push requires messaging.stream.enabled")
	}
	if m.Push.Enabled && m.Push.WebhookURL == "" {
		return fmt.Errorf("messaging.push.enabled requires webhook_url")
	}
	if m.Identity.Enabled && len(m.Identity.Keys) == 0 {
		return fmt.Errorf("messaging.identity.enabled requires at least one signing key")
	}
	for _, k := range m.Identity.Keys {
		secret, err := k.Resolve()
		if err != nil {
			return err
		}
		if len(secret) < 32 {
			return fmt.Errorf("signing key %q is %d bytes; 32 or more required", k.ID, len(secret))
		}
	}
	// A gateway open to end users with neither token verification nor a
	// restricted anonymous path is an unauthenticated message bus. Refuse.
	if m.Gateway.Enabled && !m.Identity.Enabled {
		return fmt.Errorf("messaging.gateway requires messaging.identity.enabled: " +
			"end-user connections cannot be authorised by a shared API key alone")
	}
	if m.Chat.FanoutOnWriteLimit < 0 {
		return fmt.Errorf("messaging.chat.fanout_on_write_limit must not be negative")
	}

	if m.Replication.Enabled {
		if !m.Stream.Enabled {
			return fmt.Errorf("messaging.replication requires messaging.stream.enabled")
		}
		// An empty role means the control plane assigns leadership per
		// partition, which is the only arrangement that expresses the truth in
		// a cluster: a node leads some partitions and follows others at the
		// same time. The static roles below are the pre-control-plane model,
		// where the whole node is one or the other and a failover needs a
		// human.
		switch m.Replication.Role {
		case "":
			if m.Replication.Listen == "" {
				return fmt.Errorf("messaging.replication with no role is controller-driven " +
					"and requires listen, so peers can fetch the partitions this node leads")
			}
		case "leader":
			if m.Replication.Listen == "" {
				return fmt.Errorf("messaging.replication.role=leader requires listen")
			}
		case "follower":
			if m.Replication.LeaderAddr == "" {
				return fmt.Errorf("messaging.replication.role=follower requires leader_addr")
			}
			if len(m.Replication.Topics) == 0 {
				return fmt.Errorf("messaging.replication.role=follower requires topics")
			}
			for _, spec := range m.Replication.Topics {
				if _, _, err := ParseTopicSpec(spec); err != nil {
					return fmt.Errorf("messaging.replication.topics: %w", err)
				}
			}
		default:
			return fmt.Errorf("messaging.replication.role must be \"leader\" or \"follower\", got %q",
				m.Replication.Role)
		}
		if _, err := m.Replication.ResolveSecret(); err != nil {
			return err
		}
		if m.Replication.MinInSync < 1 {
			return fmt.Errorf("messaging.replication.min_in_sync must be at least 1")
		}

		// A follower must not accept end-user writes. Nothing in the write path
		// checks partition leadership, so a send that reached a follower would
		// append to its local log and assign its own sequences — producing two
		// divergent logs with the same name, which no reconciliation can undo.
		//
		// There is no read-only gateway mode yet, so the only safe answer is to
		// refuse the combination outright.
		if m.Replication.Role == "follower" && m.Gateway.Enabled {
			return fmt.Errorf("messaging.gateway cannot be enabled on a replication " +
				"follower: writes would diverge from the leader's log. Run the " +
				"gateway on the leader, and promote this node before serving clients")
		}
		// A leader configured to need more replicas than it can ever have would
		// reject every write. Catching it here beats discovering it in
		// production under load.
		if m.Replication.Role == "follower" && m.Replication.MinInSync > 1 {
			return fmt.Errorf("messaging.replication.min_in_sync applies to the leader only")
		}
	}
	return nil
}

// ParseTopicSpec splits a replication topic spec of the form
// "topic:partitionCount". The partition count is required because a follower
// must create the topic with the same count as the leader; guessing would
// remap every key.
func ParseTopicSpec(spec string) (topic string, partitions int32, err error) {
	i := strings.LastIndex(spec, ":")
	if i <= 0 || i == len(spec)-1 {
		return "", 0, fmt.Errorf("%q must be \"topic:partitionCount\"", spec)
	}
	var n int
	if _, err := fmt.Sscanf(spec[i+1:], "%d", &n); err != nil || n <= 0 {
		return "", 0, fmt.Errorf("%q has an invalid partition count", spec)
	}
	return spec[:i], int32(n), nil
}

// ApplyMessagingEnv overlays environment variables onto the messaging config,
// so a container deployment can configure it without a config file.
func ApplyMessagingEnv(m *MessagingConfig) {
	boolEnv := func(name string, target *bool) {
		if v := os.Getenv(name); v != "" {
			*target = v == "true" || v == "1" || v == "yes"
		}
	}
	strEnv := func(name string, target *string) {
		if v := os.Getenv(name); v != "" {
			*target = v
		}
	}
	intEnv := func(name string, target *int) {
		if v := os.Getenv(name); v != "" {
			var n int
			if _, err := fmt.Sscanf(v, "%d", &n); err == nil {
				*target = n
			}
		}
	}
	durEnv := func(name string, target *Duration) {
		if v := os.Getenv(name); v != "" {
			if d, err := time.ParseDuration(v); err == nil {
				*target = Duration(d)
			}
		}
	}

	boolEnv("BOLTQ_STREAM_ENABLED", &m.Stream.Enabled)
	strEnv("BOLTQ_STREAM_DIR", &m.Stream.Dir)
	boolEnv("BOLTQ_STREAM_SYNC_ON_APPEND", &m.Stream.SyncOnAppend)
	durEnv("BOLTQ_STREAM_RETENTION_AGE", &m.Stream.RetentionAge)

	boolEnv("BOLTQ_IDENTITY_ENABLED", &m.Identity.Enabled)
	strEnv("BOLTQ_IDENTITY_ISSUER", &m.Identity.Issuer)
	boolEnv("BOLTQ_IDENTITY_ALLOW_ANONYMOUS", &m.Identity.AllowAnonymous)

	// A single key can be supplied entirely from the environment, which is the
	// common case for a container.
	if secret := os.Getenv("BOLTQ_IDENTITY_KEY"); secret != "" {
		id := os.Getenv("BOLTQ_IDENTITY_KEY_ID")
		if id == "" {
			id = "default"
		}
		m.Identity.Keys = append(m.Identity.Keys, SigningKeyConfig{ID: id, Secret: secret})
		m.Identity.Enabled = true
	}

	boolEnv("BOLTQ_GATEWAY_ENABLED", &m.Gateway.Enabled)
	strEnv("BOLTQ_GATEWAY_PATH", &m.Gateway.Path)
	intEnv("BOLTQ_GATEWAY_PORT", &m.Gateway.Port)
	durEnv("BOLTQ_GATEWAY_RESUME_WINDOW", &m.Gateway.ResumeWindow)
	if v := os.Getenv("BOLTQ_GATEWAY_ALLOWED_ORIGINS"); v != "" {
		m.Gateway.AllowedOrigins = strings.Split(v, ",")
	}

	strEnv("BOLTQ_PRESENCE_REGION", &m.Presence.Region)
	durEnv("BOLTQ_PRESENCE_TTL", &m.Presence.TTL)

	strEnv("BOLTQ_MEMBERSHIP_URL", &m.Chat.MembershipURL)
	strEnv("BOLTQ_MEMBERSHIP_AUTH_HEADER", &m.Chat.MembershipAuthHeader)
	intEnv("BOLTQ_FANOUT_ON_WRITE_LIMIT", &m.Chat.FanoutOnWriteLimit)

	boolEnv("BOLTQ_REPLICATION_ENABLED", &m.Replication.Enabled)
	strEnv("BOLTQ_REPLICATION_ROLE", &m.Replication.Role)
	strEnv("BOLTQ_REPLICATION_LISTEN", &m.Replication.Listen)
	strEnv("BOLTQ_REPLICATION_LEADER_ADDR", &m.Replication.LeaderAddr)
	strEnv("BOLTQ_REPLICATION_SECRET", &m.Replication.Secret)
	intEnv("BOLTQ_REPLICATION_MIN_IN_SYNC", &m.Replication.MinInSync)
	boolEnv("BOLTQ_REPLICATION_SYNC_ON_APPLY", &m.Replication.SyncOnApply)
	if v := os.Getenv("BOLTQ_REPLICATION_TOPICS"); v != "" {
		m.Replication.Topics = strings.Split(v, ",")
	}

	boolEnv("BOLTQ_PUSH_ENABLED", &m.Push.Enabled)
	strEnv("BOLTQ_PUSH_WEBHOOK_URL", &m.Push.WebhookURL)
	strEnv("BOLTQ_PUSH_AUTH_HEADER", &m.Push.AuthHeader)
	durEnv("BOLTQ_PUSH_GRACE_DELAY", &m.Push.GraceDelay)
}
