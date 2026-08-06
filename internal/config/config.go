package config

import (
	"encoding/json"
	"fmt"
	"os"
	"time"
)

// Config represents the server configuration.
type Config struct {
	Server      ServerConfig      `json:"server"`
	Storage     StorageConfig     `json:"storage"`
	Queue       QueueConfig       `json:"queue"`
	Cache       CacheConfig       `json:"cache"`
	Performance PerformanceConfig `json:"performance"`
	Security    SecurityConfig    `json:"security"`
	Cluster     ClusterConfig     `json:"cluster"`
	Messaging   MessagingConfig   `json:"messaging"`
}

// CacheConfig holds KV store / cache layer configuration.
type CacheConfig struct {
	Enabled    bool  `json:"enabled"`
	MaxKeys    int   `json:"max_keys"`       // 0 = unlimited
	DefaultTTL int64 `json:"default_ttl_ms"` // default TTL in ms, 0 = no expiry
	CleanupSec int   `json:"cleanup_sec"`    // expired key cleanup interval in seconds
}

// ClusterConfig holds Raft clustering configuration.
type ClusterConfig struct {
	Enabled           bool     `json:"enabled"`
	NodeID            string   `json:"node_id"`
	RaftAddr          string   `json:"raft_addr"`
	RaftDir           string   `json:"raft_dir"`
	Bootstrap         bool     `json:"bootstrap"`
	Peers             []string `json:"peers"`
	Seeds             []string `json:"seeds"`     // Seed node HTTP addresses for auto-discovery (e.g., ["10.0.0.1:9090","10.0.0.2:9090"])
	NonVoter          bool     `json:"non_voter"` // Join as non-voter (read replica) — scales without affecting consensus
	SnapshotThreshold uint64   `json:"snapshot_threshold"`

	// SessionTimeoutSeconds is how long a node may go without heartbeating
	// before the controller fences it and moves its partition leaderships.
	//
	// It is the single most consequential tuning knob in the control plane.
	// Too low and an ordinary GC pause triggers a failover that costs every
	// follower a truncation check; too high and writes to a dead node's
	// partitions stall for exactly that long. 15s is a deliberate default:
	// comfortably longer than a stop-the-world pause, comfortably shorter than
	// a user noticing. Zero means the default.
	SessionTimeoutSeconds int `json:"session_timeout_seconds"`

	// MetaRaftAddr is the control-plane consensus listener. It is a second Raft
	// group, separate from RaftAddr, and that separation is the point: a node
	// joins the control plane to learn what it leads, without receiving every
	// queue write committed anywhere in the cluster.
	//
	// Empty derives it from RaftAddr's port plus one, so an existing config
	// keeps working without naming a port it never had to name before.
	MetaRaftAddr string `json:"meta_raft_addr"`

	// QueuePlane enables the queue-plane consensus group on this node. Data
	// nodes in a large cluster leave it off: they serve the messaging plane and
	// have no reason to replicate queue state.
	QueuePlane bool `json:"queue_plane"`

	// Rebalance lets the controller move replicas toward even load when brokers
	// join or leave.
	//
	// Off by default, and deliberately so: a move copies an entire partition
	// across the network, and that must be something an operator chose, not
	// something that starts because clustering was enabled. Turn it on once the
	// cluster is stable and you are watching it.
	Rebalance bool `json:"rebalance"`

	// ReplicationFactor is how many copies of each partition to place. Two
	// means one failure leaves no redundancy; three is the lowest number that
	// survives losing a node without losing the guarantee.
	ReplicationFactor int `json:"replication_factor"`

	// MaxConcurrentMoves bounds partitions relocating at once.
	MaxConcurrentMoves int `json:"max_concurrent_moves"`
}

// SessionTimeout returns the configured fencing timeout, or fallback when unset.
func (c ClusterConfig) SessionTimeout(fallback time.Duration) time.Duration {
	if c.SessionTimeoutSeconds <= 0 {
		return fallback
	}
	return time.Duration(c.SessionTimeoutSeconds) * time.Second
}

type ServerConfig struct {
	HTTPPort int       `json:"http_port"`
	TCPPort  int       `json:"tcp_port"`
	GRPCPort int       `json:"grpc_port"`
	Host     string    `json:"host"`
	TLS      TLSConfig `json:"tls"`
}

// TLSConfig defines TLS settings.
type TLSConfig struct {
	Enabled  bool   `json:"enabled"`
	CertFile string `json:"cert_file"`
	KeyFile  string `json:"key_file"`
	CAFile   string `json:"ca_file"` // Optional: for verifying client certificates or intra-cluster auth
}

type StorageConfig struct {
	Mode                string `json:"mode"` // "memory" or "disk"
	DataDir             string `json:"data_dir"`
	CompactionThreshold int64  `json:"compaction_threshold"` // size in bytes
}

type QueueConfig struct {
	MaxRetry   int           `json:"max_retry"`
	AckTimeout time.Duration `json:"ack_timeout"`
	Capacity   int           `json:"capacity"`
}

type PerformanceConfig struct {
	WorkerPool int `json:"worker_pool"`
}

type SecurityConfig struct {
	APIKey string `json:"api_key"`
}

// Default returns a default configuration.
func Default() *Config {
	return &Config{
		Server: ServerConfig{
			HTTPPort: 9090,
			TCPPort:  9091,
			GRPCPort: 9092,
			Host:     "0.0.0.0",
			TLS: TLSConfig{
				Enabled: false,
			},
		},
		Storage: StorageConfig{
			Mode:                "memory",
			DataDir:             "./data",
			CompactionThreshold: 100 * 1024 * 1024, // 100MB
		},
		Queue: QueueConfig{
			MaxRetry:   5,
			AckTimeout: 30 * time.Second,
			Capacity:   1 << 20,
		},
		Performance: PerformanceConfig{
			WorkerPool: 16,
		},
		Cache: CacheConfig{
			Enabled:    true,
			MaxKeys:    0,  // unlimited
			DefaultTTL: 0,  // no expiry
			CleanupSec: 10, // cleanup every 10 seconds
		},
		Cluster: ClusterConfig{
			Enabled:           false,
			RaftAddr:          "0.0.0.0:9100",
			RaftDir:           "./data/raft",
			SnapshotThreshold: 8192,
		},
		Messaging: DefaultMessaging(),
	}
}

// Load reads a config from a JSON file.
func Load(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read config: %w", err)
	}
	cfg := Default()
	if err := json.Unmarshal(data, cfg); err != nil {
		return nil, fmt.Errorf("parse config: %w", err)
	}
	return cfg, nil
}

// MarshalJSON implements custom JSON marshaling for duration fields.
func (q QueueConfig) MarshalJSON() ([]byte, error) {
	type Alias struct {
		MaxRetry   int    `json:"max_retry"`
		AckTimeout string `json:"ack_timeout"`
		Capacity   int    `json:"capacity"`
	}
	return json.Marshal(Alias{
		MaxRetry:   q.MaxRetry,
		AckTimeout: q.AckTimeout.String(),
		Capacity:   q.Capacity,
	})
}

// UnmarshalJSON implements custom JSON unmarshaling for duration fields.
func (q *QueueConfig) UnmarshalJSON(data []byte) error {
	type Alias struct {
		MaxRetry   int    `json:"max_retry"`
		AckTimeout string `json:"ack_timeout"`
		Capacity   int    `json:"capacity"`
	}
	var a Alias
	if err := json.Unmarshal(data, &a); err != nil {
		return err
	}
	q.MaxRetry = a.MaxRetry
	q.Capacity = a.Capacity
	if a.AckTimeout != "" {
		d, err := time.ParseDuration(a.AckTimeout)
		if err != nil {
			return fmt.Errorf("parse ack_timeout: %w", err)
		}
		q.AckTimeout = d
	}
	return nil
}
