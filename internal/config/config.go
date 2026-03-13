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
	Performance PerformanceConfig `json:"performance"`
	Security    SecurityConfig    `json:"security"`
	Cluster     ClusterConfig     `json:"cluster"`
}

// ClusterConfig holds Raft clustering configuration.
type ClusterConfig struct {
	Enabled           bool     `json:"enabled"`
	NodeID            string   `json:"node_id"`
	RaftAddr          string   `json:"raft_addr"`
	RaftDir           string   `json:"raft_dir"`
	Bootstrap         bool     `json:"bootstrap"`
	Peers             []string `json:"peers"`
	Seeds             []string `json:"seeds"`               // Seed node HTTP addresses for auto-discovery (e.g., ["10.0.0.1:9090","10.0.0.2:9090"])
	NonVoter          bool     `json:"non_voter"`            // Join as non-voter (read replica) — scales without affecting consensus
	SnapshotThreshold uint64   `json:"snapshot_threshold"`
}

type ServerConfig struct {
	HTTPPort int    `json:"http_port"`
	TCPPort  int    `json:"tcp_port"`
	GRPCPort int    `json:"grpc_port"`
	Host     string `json:"host"`
}

type StorageConfig struct {
	Mode    string `json:"mode"` // "memory" or "disk"
	DataDir string `json:"data_dir"`
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
		},
		Storage: StorageConfig{
			Mode:    "memory",
			DataDir: "./data",
		},
		Queue: QueueConfig{
			MaxRetry:   5,
			AckTimeout: 30 * time.Second,
			Capacity:   1 << 20,
		},
		Performance: PerformanceConfig{
			WorkerPool: 16,
		},
		Cluster: ClusterConfig{
			Enabled:           false,
			RaftAddr:          "0.0.0.0:9100",
			RaftDir:           "./data/raft",
			SnapshotThreshold: 8192,
		},
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
