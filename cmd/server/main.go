package main

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/boltq/boltq/internal/api"
	"github.com/boltq/boltq/internal/broker"
	"github.com/boltq/boltq/internal/cluster"
	"github.com/boltq/boltq/internal/config"
	"github.com/boltq/boltq/internal/metrics"
	"github.com/boltq/boltq/internal/scheduler"
	"github.com/boltq/boltq/internal/storage"
)

func main() {
	configPath := flag.String("config", "", "path to config file (JSON)")
	joinAddr := flag.String("join", "", "address of existing cluster node to join (e.g., host:9090)")
	flag.Parse()

	cfg := config.Default()
	if *configPath != "" {
		var err error
		cfg, err = config.Load(*configPath)
		if err != nil {
			log.Fatalf("failed to load config: %v", err)
		}
	}

	// Override from environment variables.
	if port := os.Getenv("BOLTQ_HTTP_PORT"); port != "" {
		fmt.Sscanf(port, "%d", &cfg.Server.HTTPPort)
	}
	if port := os.Getenv("BOLTQ_TCP_PORT"); port != "" {
		fmt.Sscanf(port, "%d", &cfg.Server.TCPPort)
	}
	if mode := os.Getenv("BOLTQ_STORAGE_MODE"); mode != "" {
		cfg.Storage.Mode = mode
	}
	if dir := os.Getenv("BOLTQ_DATA_DIR"); dir != "" {
		cfg.Storage.DataDir = dir
	}
	if key := os.Getenv("BOLTQ_API_KEY"); key != "" {
		cfg.Security.APIKey = key
	}

	// Cluster env overrides.
	if v := os.Getenv("BOLTQ_CLUSTER_ENABLED"); v == "true" || v == "1" {
		cfg.Cluster.Enabled = true
	}
	if v := os.Getenv("BOLTQ_NODE_ID"); v != "" {
		cfg.Cluster.NodeID = v
	}
	if v := os.Getenv("BOLTQ_RAFT_ADDR"); v != "" {
		cfg.Cluster.RaftAddr = v
	}
	if v := os.Getenv("BOLTQ_RAFT_DIR"); v != "" {
		cfg.Cluster.RaftDir = v
	}
	if v := os.Getenv("BOLTQ_BOOTSTRAP"); v == "true" || v == "1" {
		cfg.Cluster.Bootstrap = true
	}
	if v := os.Getenv("BOLTQ_CLUSTER_PEERS"); v != "" {
		cfg.Cluster.Peers = strings.Split(v, ",")
	}
	if v := os.Getenv("BOLTQ_SEEDS"); v != "" {
		cfg.Cluster.Seeds = strings.Split(v, ",")
	}
	if v := os.Getenv("BOLTQ_NON_VOTER"); v == "true" || v == "1" {
		cfg.Cluster.NonVoter = true
	}

	// Auto-generate node ID from hostname if not set.
	if cfg.Cluster.Enabled && cfg.Cluster.NodeID == "" {
		hostname, _ := os.Hostname()
		if hostname != "" {
			cfg.Cluster.NodeID = hostname
		} else {
			cfg.Cluster.NodeID = fmt.Sprintf("node-%d", os.Getpid())
		}
		log.Printf("[server] auto-generated node_id: %s", cfg.Cluster.NodeID)
	}

	// Initialize storage.
	var store storage.Storage
	switch cfg.Storage.Mode {
	case "disk":
		var err error
		store, err = storage.NewDiskStorage(cfg.Storage.DataDir)
		if err != nil {
			log.Fatalf("failed to init disk storage: %v", err)
		}
		log.Printf("[server] storage mode: disk (dir=%s)", cfg.Storage.DataDir)
	default:
		log.Printf("[server] storage mode: memory")
	}

	// Initialize local broker.
	b := broker.New(broker.Config{
		MaxRetry:   cfg.Queue.MaxRetry,
		AckTimeout: cfg.Queue.AckTimeout,
		QueueCap:   cfg.Queue.Capacity,
		Storage:    store,
	})

	// Recover from WAL if disk mode.
	if store != nil {
		messages, err := store.ReadAll()
		if err != nil {
			log.Printf("[server] WAL recovery warning: %v", err)
		} else if len(messages) > 0 {
			log.Printf("[server] recovering %d messages from WAL", len(messages))
			for _, msg := range messages {
				b.Publish(msg.Topic, msg)
			}
		}
	}

	// Determine the active broker (local or cluster-wrapped).
	var activeBroker broker.BrokerIface = b
	var raftNode *cluster.RaftNode

	if cfg.Cluster.Enabled {
		var err error
		raftNode, err = cluster.NewRaftNode(cfg.Cluster, b)
		if err != nil {
			log.Fatalf("[server] failed to start cluster: %v", err)
		}
		activeBroker = cluster.NewClusterBroker(raftNode, b)
		log.Printf("[server] cluster mode enabled (node=%s, raft=%s, bootstrap=%v, non_voter=%v)",
			cfg.Cluster.NodeID, cfg.Cluster.RaftAddr, cfg.Cluster.Bootstrap, cfg.Cluster.NonVoter)
	}

	// Start scheduler.
	sched := scheduler.New(activeBroker, time.Second)
	sched.Start()

	// Start servers.
	m := metrics.Global()

	// Start TCP server.
	tcpServer := api.NewTCPServer(activeBroker, m, cfg.Server, cfg.Security.APIKey)
	if raftNode != nil {
		tcpServer.SetClusterNode(raftNode)
	}
	tcpAddr := fmt.Sprintf("%s:%d", cfg.Server.Host, cfg.Server.TCPPort)
	if err := tcpServer.Start(tcpAddr); err != nil {
		log.Fatalf("[server] failed to start TCP server: %v", err)
	}

	// Start HTTP server.
	httpServer := api.NewHTTPServer(activeBroker, m, cfg.Server, cfg.Security.APIKey)
	if raftNode != nil {
		httpServer.SetClusterNode(raftNode)
	}
	httpAddr := fmt.Sprintf("%s:%d", cfg.Server.Host, cfg.Server.HTTPPort)
	go func() {
		if err := httpServer.Start(httpAddr); err != nil {
			log.Printf("[server] HTTP server stopped: %v", err)
		}
	}()

	log.Printf("[server] BoltQ started (HTTP=%s, TCP=%s)", httpAddr, tcpAddr)

	// Resolve join target: --join flag > BOLTQ_JOIN_ADDR env > seeds.
	seeds := resolveSeeds(*joinAddr, cfg.Cluster.Seeds)

	// Auto-join cluster if not bootstrap and have seeds.
	if cfg.Cluster.Enabled && !cfg.Cluster.Bootstrap && len(seeds) > 0 {
		go func() {
			time.Sleep(2 * time.Second) // give local raft time to start
			discoverAndJoin(seeds, cfg.Cluster.NodeID, cfg.Cluster.RaftAddr, cfg.Cluster.NonVoter)
		}()
	}

	// Wait for shutdown signal.
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	sig := <-sigCh
	log.Printf("[server] received signal %s, shutting down...", sig)

	// Graceful leave: remove self from cluster before shutting down.
	if raftNode != nil && !cfg.Cluster.Bootstrap && len(seeds) > 0 {
		gracefulLeave(seeds, cfg.Cluster.NodeID)
	}

	tcpServer.Shutdown()
	httpServer.Shutdown()
	sched.Stop()
	if raftNode != nil {
		raftNode.Shutdown()
	}
	b.Close()
	log.Println("[server] BoltQ stopped")
}

// resolveSeeds builds a list of seed addresses from --join flag, BOLTQ_JOIN_ADDR env, and config seeds.
func resolveSeeds(joinFlag string, configSeeds []string) []string {
	seen := make(map[string]bool)
	var seeds []string

	add := func(addr string) {
		addr = strings.TrimSpace(addr)
		if addr != "" && !seen[addr] {
			seen[addr] = true
			seeds = append(seeds, addr)
		}
	}

	// --join flag has highest priority.
	add(joinFlag)

	// BOLTQ_JOIN_ADDR env.
	if v := os.Getenv("BOLTQ_JOIN_ADDR"); v != "" {
		for _, s := range strings.Split(v, ",") {
			add(s)
		}
	}

	// Config seeds.
	for _, s := range configSeeds {
		add(s)
	}

	return seeds
}

// discoverAndJoin tries each seed address to find the leader and join the cluster.
// Retries with exponential backoff — essential for orchestrated environments where
// leader may not be ready yet.
func discoverAndJoin(seeds []string, nodeID, raftAddr string, nonVoter bool) {
	payload := map[string]interface{}{
		"node_id":   nodeID,
		"addr":      raftAddr,
		"non_voter": nonVoter,
	}
	body, _ := json.Marshal(payload)

	role := "voter"
	if nonVoter {
		role = "non-voter"
	}

	maxAttempts := 15
	backoff := 2 * time.Second
	client := &http.Client{Timeout: 5 * time.Second}

	for attempt := 1; attempt <= maxAttempts; attempt++ {
		for _, seed := range seeds {
			url := fmt.Sprintf("http://%s/cluster/join", seed)
			resp, err := client.Post(url, "application/json", bytes.NewReader(body))
			if err != nil {
				continue // seed unreachable, try next
			}
			resp.Body.Close()

			if resp.StatusCode == http.StatusOK {
				log.Printf("[cluster] joined cluster via %s as %s (node=%s)", seed, role, nodeID)
				return
			}
		}

		log.Printf("[cluster] join attempt %d/%d failed on all seeds, retry in %s", attempt, maxAttempts, backoff)
		time.Sleep(backoff)
		backoff = backoff * 2
		if backoff > 30*time.Second {
			backoff = 30 * time.Second
		}
	}

	log.Printf("[cluster] WARNING: failed to join cluster after %d attempts — node is running standalone", maxAttempts)
}

// gracefulLeave notifies the leader to remove this node before shutdown.
func gracefulLeave(seeds []string, nodeID string) {
	body, _ := json.Marshal(map[string]string{"node_id": nodeID})
	client := &http.Client{Timeout: 5 * time.Second}

	for _, seed := range seeds {
		url := fmt.Sprintf("http://%s/cluster/leave", seed)
		resp, err := client.Post(url, "application/json", bytes.NewReader(body))
		if err != nil {
			continue
		}
		resp.Body.Close()
		if resp.StatusCode == http.StatusOK {
			log.Printf("[cluster] gracefully left cluster via %s", seed)
			return
		}
	}
	log.Printf("[cluster] graceful leave failed — leader may need to remove node %s manually", nodeID)
}
