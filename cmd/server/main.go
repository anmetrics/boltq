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
		log.Printf("[server] cluster mode enabled (node=%s, raft=%s, bootstrap=%v)",
			cfg.Cluster.NodeID, cfg.Cluster.RaftAddr, cfg.Cluster.Bootstrap)
	}

	// Start scheduler.
	sched := scheduler.New(activeBroker, time.Second)
	sched.Start()

	// Start servers.
	m := metrics.Global()

	// Start TCP server.
	tcpServer := api.NewTCPServer(activeBroker, m, cfg.Security.APIKey)
	if raftNode != nil {
		tcpServer.SetClusterNode(raftNode)
	}
	tcpAddr := fmt.Sprintf("%s:%d", cfg.Server.Host, cfg.Server.TCPPort)
	if err := tcpServer.Start(tcpAddr); err != nil {
		log.Fatalf("[server] failed to start TCP server: %v", err)
	}

	// Start HTTP server.
	httpServer := api.NewHTTPServer(activeBroker, m, cfg.Security.APIKey)
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

	// Join an existing cluster if --join is specified.
	if *joinAddr != "" && cfg.Cluster.Enabled {
		go func() {
			time.Sleep(2 * time.Second) // give cluster time to start
			joinCluster(*joinAddr, cfg.Cluster.NodeID, cfg.Cluster.RaftAddr)
		}()
	}

	// Wait for shutdown signal.
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	sig := <-sigCh
	log.Printf("[server] received signal %s, shutting down...", sig)

	tcpServer.Shutdown()
	httpServer.Shutdown()
	sched.Stop()
	if raftNode != nil {
		raftNode.Shutdown()
	}
	b.Close()
	log.Println("[server] BoltQ stopped")
}

// joinCluster sends a join request to an existing cluster node's HTTP API.
func joinCluster(leaderHTTP, nodeID, raftAddr string) {
	body, _ := json.Marshal(map[string]string{
		"node_id": nodeID,
		"addr":    raftAddr,
	})
	url := fmt.Sprintf("http://%s/cluster/join", leaderHTTP)
	resp, err := http.Post(url, "application/json", bytes.NewReader(body))
	if err != nil {
		log.Printf("[cluster] failed to join cluster via %s: %v", leaderHTTP, err)
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusOK {
		log.Printf("[cluster] successfully joined cluster via %s", leaderHTTP)
	} else {
		log.Printf("[cluster] join request returned status %d", resp.StatusCode)
	}
}
