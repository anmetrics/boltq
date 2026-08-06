package config

import (
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"
)

// --- Duration ---

func TestDurationUnmarshalString(t *testing.T) {
	cases := map[string]time.Duration{
		`"30s"`:   30 * time.Second,
		`"5m"`:    5 * time.Minute,
		`"8760h"`: 8760 * time.Hour,
		`"1h30m"`: 90 * time.Minute,
		`"250ms"`: 250 * time.Millisecond,
		`"0s"`:    0,
	}
	for in, want := range cases {
		var d Duration
		if err := json.Unmarshal([]byte(in), &d); err != nil {
			t.Errorf("unmarshal %s: %v", in, err)
			continue
		}
		if d.D() != want {
			t.Errorf("unmarshal %s = %v, want %v", in, d.D(), want)
		}
	}
}

func TestDurationUnmarshalNumber(t *testing.T) {
	// A raw nanosecond count must still work, so a config written by a tool
	// that marshals time.Duration natively is readable.
	var d Duration
	if err := json.Unmarshal([]byte("1500000000"), &d); err != nil {
		t.Fatalf("unmarshal number: %v", err)
	}
	if d.D() != 1500*time.Millisecond {
		t.Errorf("got %v, want 1.5s", d.D())
	}
}

func TestDurationUnmarshalRejectsGarbage(t *testing.T) {
	for _, in := range []string{`"not a duration"`, `"30"`, `true`, `{}`, `[]`, `"5 seconds"`} {
		var d Duration
		if err := json.Unmarshal([]byte(in), &d); err == nil {
			t.Errorf("garbage %s was accepted as %v", in, d.D())
		}
	}
}

func TestDurationMarshal(t *testing.T) {
	b, err := json.Marshal(Duration(90 * time.Second))
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if string(b) != `"1m30s"` {
		t.Errorf("marshal = %s, want \"1m30s\"", b)
	}
}

func TestDurationRoundTrip(t *testing.T) {
	for _, want := range []time.Duration{
		0, time.Second, 90 * time.Second, time.Hour, 8760 * time.Hour, 250 * time.Millisecond,
	} {
		b, err := json.Marshal(Duration(want))
		if err != nil {
			t.Fatalf("marshal %v: %v", want, err)
		}
		var got Duration
		if err := json.Unmarshal(b, &got); err != nil {
			t.Fatalf("unmarshal %s: %v", b, err)
		}
		if got.D() != want {
			t.Errorf("round trip of %v gave %v", want, got.D())
		}
	}
}

func TestDurationOr(t *testing.T) {
	if got := Duration(0).Or(5 * time.Second); got != 5*time.Second {
		t.Errorf("zero Duration.Or = %v, want the default", got)
	}
	if got := Duration(time.Minute).Or(5 * time.Second); got != time.Minute {
		t.Errorf("set Duration.Or = %v, want the set value", got)
	}
}

// --- SigningKeyConfig ---

func TestSigningKeyResolvePrefersEnv(t *testing.T) {
	t.Setenv("TEST_KEY_ENV", "from-environment")

	k := SigningKeyConfig{ID: "k1", Secret: "from-file", SecretEnv: "TEST_KEY_ENV"}
	got, err := k.Resolve()
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	// The environment must win: a secret in a config file is the fallback, not
	// the source of truth.
	if got != "from-environment" {
		t.Errorf("resolve = %q, want the environment value", got)
	}
}

func TestSigningKeyResolveFallsBackToLiteral(t *testing.T) {
	k := SigningKeyConfig{ID: "k1", Secret: "literal-secret"}
	got, err := k.Resolve()
	if err != nil || got != "literal-secret" {
		t.Errorf("got %q, %v", got, err)
	}
}

func TestSigningKeyResolveErrors(t *testing.T) {
	if _, err := (SigningKeyConfig{ID: "k1"}).Resolve(); err == nil {
		t.Error("a key with no secret at all resolved")
	}

	// An env var that is named but empty must be an error, not a silent
	// fallback to an empty key.
	os.Unsetenv("TEST_MISSING_KEY")
	_, err := SigningKeyConfig{ID: "k1", SecretEnv: "TEST_MISSING_KEY"}.Resolve()
	if err == nil {
		t.Error("an unset env var resolved successfully")
	}
	if err != nil && !strings.Contains(err.Error(), "TEST_MISSING_KEY") {
		t.Errorf("error does not name the variable: %v", err)
	}
}

// --- Validate ---

func validMessaging() MessagingConfig {
	m := DefaultMessaging()
	m.Stream.Enabled = true
	m.Identity.Enabled = true
	m.Identity.Keys = []SigningKeyConfig{
		{ID: "k1", Secret: "0123456789abcdef0123456789abcdef"},
	}
	return m
}

func TestValidateAcceptsDefaults(t *testing.T) {
	m := DefaultMessaging()
	if err := m.Validate(); err != nil {
		t.Errorf("all-disabled defaults failed validation: %v", err)
	}
}

func TestValidateAcceptsValidConfig(t *testing.T) {
	m := validMessaging()
	m.Gateway.Enabled = true
	if err := m.Validate(); err != nil {
		t.Errorf("valid config rejected: %v", err)
	}
}

func TestValidateGatewayRequiresStream(t *testing.T) {
	m := DefaultMessaging()
	m.Gateway.Enabled = true
	err := m.Validate()
	if err == nil || !strings.Contains(err.Error(), "stream") {
		t.Errorf("gateway without stream: %v", err)
	}
}

func TestValidateGatewayRequiresIdentity(t *testing.T) {
	// This is the important one: a public WebSocket endpoint with no token
	// verification is an unauthenticated message bus.
	m := DefaultMessaging()
	m.Stream.Enabled = true
	m.Gateway.Enabled = true
	m.Identity.Enabled = false

	err := m.Validate()
	if err == nil {
		t.Fatal("a gateway with no identity verification was accepted")
	}
	if !strings.Contains(err.Error(), "identity") {
		t.Errorf("error does not mention identity: %v", err)
	}
}

func TestValidatePushRequirements(t *testing.T) {
	m := DefaultMessaging()
	m.Push.Enabled = true
	if err := m.Validate(); err == nil || !strings.Contains(err.Error(), "stream") {
		t.Errorf("push without stream: %v", err)
	}

	m = validMessaging()
	m.Push.Enabled = true
	if err := m.Validate(); err == nil || !strings.Contains(err.Error(), "webhook_url") {
		t.Errorf("push without a webhook URL: %v", err)
	}

	m.Push.WebhookURL = "https://example.com/push"
	if err := m.Validate(); err != nil {
		t.Errorf("valid push config rejected: %v", err)
	}
}

func TestValidateIdentityRequiresKeys(t *testing.T) {
	m := DefaultMessaging()
	m.Stream.Enabled = true
	m.Identity.Enabled = true
	m.Identity.Keys = nil

	if err := m.Validate(); err == nil || !strings.Contains(err.Error(), "signing key") {
		t.Errorf("identity with no keys: %v", err)
	}
}

func TestValidateRejectsShortKeys(t *testing.T) {
	m := DefaultMessaging()
	m.Stream.Enabled = true
	m.Identity.Enabled = true

	for _, secret := range []string{"", "short", strings.Repeat("a", 31)} {
		m.Identity.Keys = []SigningKeyConfig{{ID: "weak", Secret: secret}}
		if err := m.Validate(); err == nil {
			t.Errorf("a %d-byte secret was accepted", len(secret))
		}
	}

	m.Identity.Keys = []SigningKeyConfig{{ID: "ok", Secret: strings.Repeat("a", 32)}}
	if err := m.Validate(); err != nil {
		t.Errorf("a 32-byte secret was rejected: %v", err)
	}
}

func TestValidateResolvesKeysFromEnv(t *testing.T) {
	t.Setenv("TEST_VALIDATE_KEY", strings.Repeat("z", 32))

	m := DefaultMessaging()
	m.Stream.Enabled = true
	m.Identity.Enabled = true
	m.Identity.Keys = []SigningKeyConfig{{ID: "k1", SecretEnv: "TEST_VALIDATE_KEY"}}

	if err := m.Validate(); err != nil {
		t.Errorf("env-sourced key rejected: %v", err)
	}

	// And a short env-sourced key must be caught the same way.
	t.Setenv("TEST_VALIDATE_KEY", "tooshort")
	if err := m.Validate(); err == nil {
		t.Error("a short env-sourced key was accepted")
	}
}

func TestValidateRejectsNegativeFanoutLimit(t *testing.T) {
	m := validMessaging()
	m.Chat.FanoutOnWriteLimit = -1
	if err := m.Validate(); err == nil {
		t.Error("a negative fan-out limit was accepted")
	}
}

// --- Environment overrides ---

func TestApplyMessagingEnvBooleans(t *testing.T) {
	for _, truthy := range []string{"true", "1", "yes"} {
		m := DefaultMessaging()
		t.Setenv("BOLTQ_STREAM_ENABLED", truthy)
		ApplyMessagingEnv(&m)
		if !m.Stream.Enabled {
			t.Errorf("%q did not enable the stream", truthy)
		}
	}
	for _, falsy := range []string{"false", "0", "no", "off"} {
		m := DefaultMessaging()
		m.Stream.Enabled = true
		t.Setenv("BOLTQ_STREAM_ENABLED", falsy)
		ApplyMessagingEnv(&m)
		if m.Stream.Enabled {
			t.Errorf("%q did not disable the stream", falsy)
		}
	}
}

func TestApplyMessagingEnvUnsetLeavesValues(t *testing.T) {
	os.Unsetenv("BOLTQ_STREAM_ENABLED")
	os.Unsetenv("BOLTQ_STREAM_DIR")

	m := DefaultMessaging()
	m.Stream.Enabled = true
	m.Stream.Dir = "/custom/path"
	ApplyMessagingEnv(&m)

	if !m.Stream.Enabled || m.Stream.Dir != "/custom/path" {
		t.Error("unset environment variables clobbered configured values")
	}
}

func TestApplyMessagingEnvStringsAndInts(t *testing.T) {
	t.Setenv("BOLTQ_STREAM_DIR", "/data/streams")
	t.Setenv("BOLTQ_IDENTITY_ISSUER", "auth.example.com")
	t.Setenv("BOLTQ_GATEWAY_PATH", "/socket")
	t.Setenv("BOLTQ_GATEWAY_PORT", "9095")
	t.Setenv("BOLTQ_PRESENCE_REGION", "eu-west-1")
	t.Setenv("BOLTQ_MEMBERSHIP_URL", "https://api.example.com/m")
	t.Setenv("BOLTQ_FANOUT_ON_WRITE_LIMIT", "512")

	m := DefaultMessaging()
	ApplyMessagingEnv(&m)

	checks := map[string][2]string{
		"stream dir":     {m.Stream.Dir, "/data/streams"},
		"issuer":         {m.Identity.Issuer, "auth.example.com"},
		"gateway path":   {m.Gateway.Path, "/socket"},
		"region":         {m.Presence.Region, "eu-west-1"},
		"membership url": {m.Chat.MembershipURL, "https://api.example.com/m"},
	}
	for name, v := range checks {
		if v[0] != v[1] {
			t.Errorf("%s = %q, want %q", name, v[0], v[1])
		}
	}
	if m.Gateway.Port != 9095 {
		t.Errorf("gateway port = %d", m.Gateway.Port)
	}
	if m.Chat.FanoutOnWriteLimit != 512 {
		t.Errorf("fanout limit = %d", m.Chat.FanoutOnWriteLimit)
	}
}

func TestApplyMessagingEnvIgnoresBadInts(t *testing.T) {
	t.Setenv("BOLTQ_GATEWAY_PORT", "not-a-number")

	m := DefaultMessaging()
	m.Gateway.Port = 9095
	ApplyMessagingEnv(&m)

	if m.Gateway.Port != 9095 {
		t.Errorf("an unparsable port clobbered the configured value: %d", m.Gateway.Port)
	}
}

func TestApplyMessagingEnvDurations(t *testing.T) {
	t.Setenv("BOLTQ_STREAM_RETENTION_AGE", "720h")
	t.Setenv("BOLTQ_GATEWAY_RESUME_WINDOW", "10m")
	t.Setenv("BOLTQ_PRESENCE_TTL", "45s")
	t.Setenv("BOLTQ_PUSH_GRACE_DELAY", "5s")

	m := DefaultMessaging()
	ApplyMessagingEnv(&m)

	if m.Stream.RetentionAge.D() != 720*time.Hour {
		t.Errorf("retention age = %v", m.Stream.RetentionAge.D())
	}
	if m.Gateway.ResumeWindow.D() != 10*time.Minute {
		t.Errorf("resume window = %v", m.Gateway.ResumeWindow.D())
	}
	if m.Presence.TTL.D() != 45*time.Second {
		t.Errorf("presence ttl = %v", m.Presence.TTL.D())
	}
	if m.Push.GraceDelay.D() != 5*time.Second {
		t.Errorf("grace delay = %v", m.Push.GraceDelay.D())
	}
}

func TestApplyMessagingEnvIgnoresBadDurations(t *testing.T) {
	t.Setenv("BOLTQ_PRESENCE_TTL", "forever")

	m := DefaultMessaging()
	original := m.Presence.TTL
	ApplyMessagingEnv(&m)

	if m.Presence.TTL != original {
		t.Errorf("an unparsable duration clobbered the default: %v", m.Presence.TTL.D())
	}
}

func TestApplyMessagingEnvSingleKeyEnablesIdentity(t *testing.T) {
	t.Setenv("BOLTQ_IDENTITY_KEY", strings.Repeat("k", 32))
	t.Setenv("BOLTQ_IDENTITY_KEY_ID", "prod-2025")

	m := DefaultMessaging()
	ApplyMessagingEnv(&m)

	if !m.Identity.Enabled {
		t.Error("supplying a key did not enable identity")
	}
	if len(m.Identity.Keys) != 1 {
		t.Fatalf("got %d keys", len(m.Identity.Keys))
	}
	if m.Identity.Keys[0].ID != "prod-2025" {
		t.Errorf("key id = %q", m.Identity.Keys[0].ID)
	}
	if err := m.Validate(); err != nil {
		// It should also produce a config that actually validates.
		m.Stream.Enabled = true
		if err := m.Validate(); err != nil {
			t.Errorf("env-configured identity does not validate: %v", err)
		}
	}
}

func TestApplyMessagingEnvKeyIDDefaults(t *testing.T) {
	t.Setenv("BOLTQ_IDENTITY_KEY", strings.Repeat("k", 32))
	os.Unsetenv("BOLTQ_IDENTITY_KEY_ID")

	m := DefaultMessaging()
	ApplyMessagingEnv(&m)

	if len(m.Identity.Keys) != 1 || m.Identity.Keys[0].ID != "default" {
		t.Errorf("keys = %+v", m.Identity.Keys)
	}
}

func TestApplyMessagingEnvAllowedOrigins(t *testing.T) {
	t.Setenv("BOLTQ_GATEWAY_ALLOWED_ORIGINS", "https://a.com,https://b.com,https://c.com")

	m := DefaultMessaging()
	ApplyMessagingEnv(&m)

	if len(m.Gateway.AllowedOrigins) != 3 {
		t.Fatalf("origins = %v", m.Gateway.AllowedOrigins)
	}
	if m.Gateway.AllowedOrigins[1] != "https://b.com" {
		t.Errorf("origins = %v", m.Gateway.AllowedOrigins)
	}
}

// --- Whole-config integration ---

func TestDefaultConfigIncludesMessagingDisabled(t *testing.T) {
	cfg := Default()
	if cfg.Messaging.Stream.Enabled {
		t.Error("the messaging subsystem is enabled by default")
	}
	if cfg.Messaging.Gateway.Enabled || cfg.Messaging.Identity.Enabled || cfg.Messaging.Push.Enabled {
		t.Error("a messaging component is enabled by default")
	}
	// Sensible defaults must still be present so enabling one flag works.
	if cfg.Messaging.Stream.DefaultPartitions == 0 || cfg.Messaging.Chat.FanoutOnWriteLimit == 0 {
		t.Error("defaults are missing from the disabled messaging block")
	}
}

func TestLoadConfigWithMessaging(t *testing.T) {
	dir := t.TempDir()
	path := dir + "/config.json"

	content := `{
      "server": { "http_port": 9090, "tcp_port": 9091 },
      "queue":  { "max_retry": 5, "ack_timeout": "30s", "capacity": 1024 },
      "messaging": {
        "stream":   { "enabled": true, "default_partitions": 32,
                      "retention_age": "8760h", "sync_on_append": true },
        "identity": { "enabled": true, "issuer": "auth.example.com",
                      "keys": [{ "id": "k1", "secret": "0123456789abcdef0123456789abcdef" }] },
        "gateway":  { "enabled": true, "port": 9095, "resume_window": "10m",
                      "allowed_origins": ["https://app.example.com"] },
        "chat":     { "fanout_on_write_limit": 128 }
      }
    }`
	if err := os.WriteFile(path, []byte(content), 0644); err != nil {
		t.Fatal(err)
	}

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("load: %v", err)
	}

	m := cfg.Messaging
	if !m.Stream.Enabled || m.Stream.DefaultPartitions != 32 || !m.Stream.SyncOnAppend {
		t.Errorf("stream config: %+v", m.Stream)
	}
	if m.Stream.RetentionAge.D() != 8760*time.Hour {
		t.Errorf("retention age = %v", m.Stream.RetentionAge.D())
	}
	if m.Gateway.Port != 9095 || m.Gateway.ResumeWindow.D() != 10*time.Minute {
		t.Errorf("gateway config: %+v", m.Gateway)
	}
	if len(m.Gateway.AllowedOrigins) != 1 {
		t.Errorf("allowed origins = %v", m.Gateway.AllowedOrigins)
	}
	if m.Chat.FanoutOnWriteLimit != 128 {
		t.Errorf("fanout limit = %d", m.Chat.FanoutOnWriteLimit)
	}
	if err := m.Validate(); err != nil {
		t.Errorf("loaded config does not validate: %v", err)
	}

	// Unspecified fields must keep their defaults rather than becoming zero.
	if m.Presence.TTL.D() == 0 {
		t.Error("an unspecified nested field lost its default")
	}
}

func TestLoadConfigOmittingMessagingKeepsDefaults(t *testing.T) {
	dir := t.TempDir()
	path := dir + "/config.json"
	os.WriteFile(path, []byte(`{"server":{"http_port":9090}}`), 0644)

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if cfg.Messaging.Stream.Enabled {
		t.Error("a config with no messaging block enabled the subsystem")
	}
	if cfg.Messaging.Stream.DefaultPartitions != 16 {
		t.Errorf("defaults lost: partitions = %d", cfg.Messaging.Stream.DefaultPartitions)
	}
}

func TestLoadRejectsBadDurationInMessaging(t *testing.T) {
	dir := t.TempDir()
	path := dir + "/config.json"
	os.WriteFile(path, []byte(`{"messaging":{"stream":{"retention_age":"forever"}}}`), 0644)

	if _, err := Load(path); err == nil {
		t.Error("an invalid duration in the messaging block was accepted")
	}
}

func TestConfigMarshalRoundTrip(t *testing.T) {
	cfg := Default()
	cfg.Messaging = validMessaging()
	cfg.Messaging.Gateway.Enabled = true
	cfg.Messaging.Stream.RetentionAge = Duration(720 * time.Hour)

	data, err := json.Marshal(cfg)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	var back Config
	if err := json.Unmarshal(data, &back); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if back.Messaging.Stream.RetentionAge.D() != 720*time.Hour {
		t.Errorf("retention age did not survive: %v", back.Messaging.Stream.RetentionAge.D())
	}
	if !back.Messaging.Gateway.Enabled {
		t.Error("gateway flag did not survive the round trip")
	}
}

// --- Replication config ---

func validReplicationLeader() MessagingConfig {
	m := validMessaging()
	m.Replication = ReplicationConfig{
		Enabled: true, Role: "leader", Listen: "10.0.0.1:9200",
		Secret: "rep-secret", MinInSync: 2,
	}
	return m
}

func TestReplicationDisabledByDefault(t *testing.T) {
	m := DefaultMessaging()
	if m.Replication.Enabled {
		t.Error("replication is enabled by default")
	}
	if m.Replication.MinInSync != 1 {
		t.Errorf("default min_in_sync = %d, want 1 (asynchronous)", m.Replication.MinInSync)
	}
}

func TestReplicationRequiresStream(t *testing.T) {
	m := DefaultMessaging()
	m.Replication = ReplicationConfig{Enabled: true, Role: "leader", Listen: "x:1", MinInSync: 1}
	if err := m.Validate(); err == nil || !strings.Contains(err.Error(), "stream") {
		t.Errorf("replication without stream: %v", err)
	}
}

func TestReplicationRoleValidation(t *testing.T) {
	m := validMessaging()
	m.Replication = ReplicationConfig{Enabled: true, MinInSync: 1}

	for _, role := range []string{"", "primary", "replica", "LEADER"} {
		m.Replication.Role = role
		if err := m.Validate(); err == nil || !strings.Contains(err.Error(), "role") {
			t.Errorf("role %q was accepted: %v", role, err)
		}
	}
}

func TestReplicationLeaderRequiresListen(t *testing.T) {
	m := validMessaging()
	m.Replication = ReplicationConfig{Enabled: true, Role: "leader", MinInSync: 1}
	if err := m.Validate(); err == nil || !strings.Contains(err.Error(), "listen") {
		t.Errorf("leader without listen: %v", err)
	}
}

func TestReplicationFollowerRequirements(t *testing.T) {
	m := validMessaging()
	m.Replication = ReplicationConfig{Enabled: true, Role: "follower", MinInSync: 1}

	if err := m.Validate(); err == nil || !strings.Contains(err.Error(), "leader_addr") {
		t.Errorf("follower without leader_addr: %v", err)
	}

	m.Replication.LeaderAddr = "10.0.0.1:9200"
	if err := m.Validate(); err == nil || !strings.Contains(err.Error(), "topics") {
		t.Errorf("follower without topics: %v", err)
	}

	m.Replication.Topics = []string{"chat.direct.alice:bob:16"}
	if err := m.Validate(); err != nil {
		t.Errorf("valid follower config rejected: %v", err)
	}
}

func TestReplicationFollowerRejectsMinInSyncAboveOne(t *testing.T) {
	// min_in_sync governs the leader's acknowledgement policy; setting it on a
	// follower means the operator misunderstood it, and silently ignoring the
	// value would leave them believing they had quorum durability.
	m := validMessaging()
	m.Replication = ReplicationConfig{
		Enabled: true, Role: "follower", LeaderAddr: "x:1",
		Topics: []string{"chat:1"}, MinInSync: 2,
	}
	if err := m.Validate(); err == nil || !strings.Contains(err.Error(), "leader only") {
		t.Errorf("follower with min_in_sync=2: %v", err)
	}
}

func TestReplicationMinInSyncFloor(t *testing.T) {
	m := validReplicationLeader()
	m.Replication.MinInSync = 0
	if err := m.Validate(); err == nil || !strings.Contains(err.Error(), "min_in_sync") {
		t.Errorf("min_in_sync=0: %v", err)
	}
}

func TestReplicationRejectsBadTopicSpecs(t *testing.T) {
	m := validMessaging()
	m.Replication = ReplicationConfig{
		Enabled: true, Role: "follower", LeaderAddr: "x:1", MinInSync: 1,
	}
	for _, spec := range []string{"chat", "chat:", ":16", "chat:0", "chat:-1", "chat:abc", ""} {
		m.Replication.Topics = []string{spec}
		if err := m.Validate(); err == nil {
			t.Errorf("topic spec %q was accepted", spec)
		}
	}
}

func TestParseTopicSpec(t *testing.T) {
	cases := map[string]struct {
		topic string
		count int32
	}{
		"chat:16":                  {"chat", 16},
		"chat.inbox.bob:1":         {"chat.inbox.bob", 1},
		"chat.direct.alice:bob:32": {"chat.direct.alice:bob", 32}, // colons inside the topic
	}
	for spec, want := range cases {
		topic, count, err := ParseTopicSpec(spec)
		if err != nil {
			t.Errorf("ParseTopicSpec(%q): %v", spec, err)
			continue
		}
		if topic != want.topic || count != want.count {
			t.Errorf("ParseTopicSpec(%q) = %q/%d, want %q/%d",
				spec, topic, count, want.topic, want.count)
		}
	}

	for _, bad := range []string{"chat", ":16", "chat:", "chat:0", "chat:xyz", "", ":"} {
		if _, _, err := ParseTopicSpec(bad); err == nil {
			t.Errorf("ParseTopicSpec(%q) succeeded", bad)
		}
	}
}

func TestReplicationSecretResolution(t *testing.T) {
	r := ReplicationConfig{Secret: "literal"}
	if got, err := r.ResolveSecret(); err != nil || got != "literal" {
		t.Errorf("literal secret: %q, %v", got, err)
	}

	t.Setenv("TEST_REP_SECRET", "from-env")
	r = ReplicationConfig{Secret: "literal", SecretEnv: "TEST_REP_SECRET"}
	if got, err := r.ResolveSecret(); err != nil || got != "from-env" {
		t.Errorf("env must win: %q, %v", got, err)
	}

	os.Unsetenv("TEST_REP_MISSING")
	r = ReplicationConfig{SecretEnv: "TEST_REP_MISSING"}
	if _, err := r.ResolveSecret(); err == nil {
		t.Error("an unset secret env resolved successfully")
	}

	// An empty secret is legal — it means "no authentication", which the docs
	// flag as private-network only.
	if got, err := (ReplicationConfig{}).ResolveSecret(); err != nil || got != "" {
		t.Errorf("empty secret: %q, %v", got, err)
	}
}

func TestReplicationValidateSurfacesSecretError(t *testing.T) {
	os.Unsetenv("TEST_REP_ABSENT")
	m := validReplicationLeader()
	m.Replication.Secret = ""
	m.Replication.SecretEnv = "TEST_REP_ABSENT"
	if err := m.Validate(); err == nil {
		t.Error("a missing replication secret env passed validation")
	}
}

func TestReplicationEnvOverrides(t *testing.T) {
	t.Setenv("BOLTQ_REPLICATION_ENABLED", "true")
	t.Setenv("BOLTQ_REPLICATION_ROLE", "follower")
	t.Setenv("BOLTQ_REPLICATION_LEADER_ADDR", "10.0.0.1:9200")
	t.Setenv("BOLTQ_REPLICATION_SECRET", "env-secret")
	t.Setenv("BOLTQ_REPLICATION_MIN_IN_SYNC", "1")
	t.Setenv("BOLTQ_REPLICATION_SYNC_ON_APPLY", "true")
	t.Setenv("BOLTQ_REPLICATION_TOPICS", "chat.inbox.bob:1,chat.direct.a:b:16")

	m := DefaultMessaging()
	ApplyMessagingEnv(&m)

	if !m.Replication.Enabled || m.Replication.Role != "follower" {
		t.Errorf("replication env not applied: %+v", m.Replication)
	}
	if m.Replication.LeaderAddr != "10.0.0.1:9200" || m.Replication.Secret != "env-secret" {
		t.Errorf("addr/secret: %+v", m.Replication)
	}
	if !m.Replication.SyncOnApply {
		t.Error("sync_on_apply not applied")
	}
	if len(m.Replication.Topics) != 2 {
		t.Fatalf("topics = %v", m.Replication.Topics)
	}
	for _, spec := range m.Replication.Topics {
		if _, _, err := ParseTopicSpec(spec); err != nil {
			t.Errorf("env topic %q does not parse: %v", spec, err)
		}
	}
}

func TestLoadConfigWithReplication(t *testing.T) {
	dir := t.TempDir()
	path := dir + "/config.json"
	content := `{
      "messaging": {
        "stream":   { "enabled": true },
        "identity": { "enabled": true,
                      "keys": [{ "id": "k1", "secret": "0123456789abcdef0123456789abcdef" }] },
        "replication": {
          "enabled": true, "role": "leader", "listen": "10.0.0.1:9200",
          "secret": "rep", "min_in_sync": 2, "ack_timeout": "3s",
          "max_lag_in_sync": 5000
        }
      }
    }`
	if err := os.WriteFile(path, []byte(content), 0644); err != nil {
		t.Fatal(err)
	}

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	r := cfg.Messaging.Replication
	if !r.Enabled || r.Role != "leader" || r.MinInSync != 2 {
		t.Errorf("replication config: %+v", r)
	}
	if r.AckTimeout.D() != 3*time.Second {
		t.Errorf("ack_timeout = %v", r.AckTimeout.D())
	}
	if r.MaxLagInSync != 5000 {
		t.Errorf("max_lag_in_sync = %d", r.MaxLagInSync)
	}
	if err := cfg.Messaging.Validate(); err != nil {
		t.Errorf("loaded replication config does not validate: %v", err)
	}
}

func TestFollowerCannotServeGateway(t *testing.T) {
	// Nothing in the write path checks partition leadership, so a send that
	// reached a follower would append locally and assign its own sequences —
	// two divergent logs with the same name. The config must refuse it.
	m := validMessaging()
	m.Replication = ReplicationConfig{
		Enabled: true, Role: "follower", LeaderAddr: "10.0.0.1:9200",
		Topics: []string{"chat:1"}, MinInSync: 1,
	}
	m.Gateway.Enabled = true

	err := m.Validate()
	if err == nil {
		t.Fatal("a follower with a gateway was accepted — writes would diverge")
	}
	if !strings.Contains(err.Error(), "diverge") {
		t.Errorf("error should explain the consequence: %v", err)
	}

	// The same node without a gateway is fine.
	m.Gateway.Enabled = false
	if err := m.Validate(); err != nil {
		t.Errorf("a follower without a gateway was rejected: %v", err)
	}
}

func TestLeaderMayServeGateway(t *testing.T) {
	m := validReplicationLeader()
	m.Gateway.Enabled = true
	if err := m.Validate(); err != nil {
		t.Errorf("a leader with a gateway was rejected: %v", err)
	}
}
