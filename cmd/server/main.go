package main

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/boltq/boltq/internal/api"
	"github.com/boltq/boltq/internal/broker"
	"github.com/boltq/boltq/internal/cache"
	"github.com/boltq/boltq/internal/cluster"
	"github.com/boltq/boltq/internal/config"
	"github.com/boltq/boltq/internal/metrics"
	"github.com/boltq/boltq/internal/scheduler"
	"github.com/boltq/boltq/internal/storage"
	"github.com/boltq/boltq/internal/streamctl"
	"github.com/boltq/boltq/internal/wal"
	"github.com/boltq/boltq/pkg/protocol"
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
	if v := os.Getenv("BOLTQ_STORAGE_COMPACTION_THRESHOLD"); v != "" {
		var threshold int64
		if _, err := fmt.Sscanf(v, "%d", &threshold); err == nil {
			cfg.Storage.CompactionThreshold = threshold
		}
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
	// BOLTQ_BOOTSTRAP_NODE_ID names the one node that may bootstrap, so every
	// replica in a set can share an identical environment and still have
	// exactly one of them form the cluster.
	//
	// The alternative — a shell conditional in the container command — needs a
	// shell in the image, which rules out a distroless base, and puts a
	// correctness rule that decides whether the cluster splits into a YAML
	// string comparison nobody tests.
	if v := os.Getenv("BOLTQ_BOOTSTRAP_NODE_ID"); v != "" {
		hostname, _ := os.Hostname()
		self := cfg.Cluster.NodeID
		if self == "" {
			self = hostname
		}
		cfg.Cluster.Bootstrap = v == self
		if cfg.Cluster.Bootstrap {
			log.Printf("[server] this node (%s) is the designated bootstrap node", self)
		}
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
	if v := os.Getenv("BOLTQ_META_RAFT_ADDR"); v != "" {
		cfg.Cluster.MetaRaftAddr = v
	}
	if v := os.Getenv("BOLTQ_QUEUE_PLANE"); v == "true" || v == "1" {
		cfg.Cluster.QueuePlane = true
	}
	if v := os.Getenv("BOLTQ_REBALANCE"); v == "true" || v == "1" {
		cfg.Cluster.Rebalance = true
	}
	if v := os.Getenv("BOLTQ_REPLICATION_FACTOR"); v != "" {
		fmt.Sscanf(v, "%d", &cfg.Cluster.ReplicationFactor)
	}
	if v := os.Getenv("BOLTQ_SESSION_TIMEOUT_SECONDS"); v != "" {
		fmt.Sscanf(v, "%d", &cfg.Cluster.SessionTimeoutSeconds)
	}

	// Cache env overrides.
	if v := os.Getenv("BOLTQ_CACHE_ENABLED"); v == "true" || v == "1" {
		cfg.Cache.Enabled = true
	} else if v == "false" || v == "0" {
		cfg.Cache.Enabled = false
	}
	if v := os.Getenv("BOLTQ_CACHE_MAX_KEYS"); v != "" {
		var maxKeys int
		if _, err := fmt.Sscanf(v, "%d", &maxKeys); err == nil {
			cfg.Cache.MaxKeys = maxKeys
		}
	}

	// Messaging (stream/gateway/presence/push) env overrides.
	config.ApplyMessagingEnv(&cfg.Messaging)

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
		s, err := storage.NewDiskStorage(cfg.Storage.DataDir)
		if err != nil {
			log.Fatalf("failed to init disk storage: %v", err)
		}
		store = s
		log.Printf("[server] storage mode: disk (dir=%s)", cfg.Storage.DataDir)
	default:
		log.Printf("[server] storage mode: memory")
	}

	// Initialize local broker.
	b := broker.New(broker.Config{
		MaxRetry:            cfg.Queue.MaxRetry,
		AckTimeout:          cfg.Queue.AckTimeout,
		QueueCap:            cfg.Queue.Capacity,
		CompactionThreshold: cfg.Storage.CompactionThreshold,
		Storage:             store,
	})

	// Recover from WAL if disk mode.
	if store != nil {
		records, err := store.ReadAllRecords()
		if err != nil {
			log.Printf("[server] WAL recovery warning: %v", err)
		} else if len(records) > 0 {
			log.Printf("[server] processsing %d records from WAL for recovery", len(records))

			// Process records in order, keeping track of what's still pending
			msgs := make(map[string]*protocol.Message)
			order := []string{}

			for _, rec := range records {
				switch rec.Type {
				case wal.RecordPublish:
					msgs[rec.Message.ID] = rec.Message
					order = append(order, rec.Message.ID)
				case wal.RecordAck:
					delete(msgs, rec.MsgID)
				default:
					// Metadata records (exchange/binding) - replay to restore state
					b.ReplayMetadata(rec.Type, rec.Metadata)
				}
			}

			recoveredCount := 0
			for _, id := range order {
				if msg, ok := msgs[id]; ok {
					b.IngestRecovered(msg)
					recoveredCount++
				}
			}
			log.Printf("[server] recovered %d messages from WAL (order preserved)", recoveredCount)

			// Perform initial compaction to remove ACKs from WAL
			if err := b.Checkpoint(); err != nil {
				log.Printf("[server] initial WAL compaction failed: %v", err)
			} else {
				log.Printf("[server] WAL compacted after recovery")
			}
		}
	}

	// Determine the active broker (local or cluster-wrapped).
	var activeBroker broker.BrokerIface = b
	var raftNode *cluster.RaftNode
	var controller *cluster.Controller

	var metaNode *cluster.MetadataNode

	if cfg.Cluster.Enabled {
		// The control plane comes up first and on every node. It is the group
		// that says which partitions this node leads, so nothing else can be
		// decided without it.
		metaAddr := cfg.Cluster.MetaRaftAddr
		if metaAddr == "" {
			metaAddr = derivedMetaAddr(cfg.Cluster.RaftAddr)
		}
		var err error
		metaNode, err = cluster.NewMetadataNode(cfg.Cluster, metaAddr)
		if err != nil {
			log.Fatalf("[server] failed to start control plane: %v", err)
		}
		log.Printf("[server] control plane enabled (node=%s, meta_raft=%s, bootstrap=%v)",
			cfg.Cluster.NodeID, metaAddr, cfg.Cluster.Bootstrap)

		// The queue plane is a separate group, and opt-in. A data node that
		// serves only the messaging plane has no reason to replicate, store and
		// apply every queue write committed anywhere in the cluster.
		if cfg.Cluster.QueuePlane {
			raftNode, err = cluster.NewRaftNode(cfg.Cluster, b)
			if err != nil {
				log.Fatalf("[server] failed to start queue plane: %v", err)
			}
			activeBroker = cluster.NewClusterBroker(raftNode, b)
			log.Printf("[server] queue plane clustered (raft=%s)", cfg.Cluster.RaftAddr)
		} else {
			log.Printf("[server] queue plane is local to this node (set cluster.queue_plane to replicate it)")
		}

		// Every node starts a controller; only the one that is Raft leader acts.
		// Starting it everywhere is what makes controller failover instant —
		// there is no election to run and no process to start, the new leader's
		// controller simply finds itself in charge on its next sweep.
		controller = cluster.NewController(metaNode, cluster.ControllerConfig{
			SessionTimeout:           cfg.Cluster.SessionTimeout(15 * time.Second),
			PreferredLeaderRebalance: true,
			Rebalance:                cfg.Cluster.Rebalance,
			ReplicationFactor:        cfg.Cluster.ReplicationFactor,
			MaxConcurrentMoves:       cfg.Cluster.MaxConcurrentMoves,
		})
		controller.Start()
		// Cluster health is exposed on /metrics from here: partitions offline,
		// under-replicated, and how leadership is spread. These are the numbers
		// worth alerting on.
		cluster.RegisterMetrics(metaNode.Metadata(), cfg.Cluster.NodeID)
		if cfg.Cluster.Rebalance {
			log.Printf("[server] replica rebalancing enabled (rf=%d, max_concurrent_moves=%d)",
				cfg.Cluster.ReplicationFactor, cfg.Cluster.MaxConcurrentMoves)
		}
	}

	// Build the messaging stack (partitioned log, presence, gateway, push).
	// It is independent of the queue broker above: a deployment may run either,
	// or both, and a failure here must not silently degrade into a server that
	// looks healthy while serving no chat traffic.
	nodeID := cfg.Cluster.NodeID
	if nodeID == "" {
		hostname, _ := os.Hostname()
		nodeID = hostname
	}
	// The agent is built before the messaging stack because the stack's
	// reconciler submits ISR reports through it.
	var agent *cluster.Agent
	if metaNode != nil {
		agent = cluster.NewAgent(metaNode, cluster.AgentConfig{
			NodeID:      cfg.Cluster.NodeID,
			AdminAddr:   advertisedAddr(cfg.Server.Host, cfg.Server.HTTPPort),
			RaftAddr:    cfg.Cluster.RaftAddr,
			StreamAddr:  advertisedListen(cfg.Messaging.Replication.Listen),
			GatewayAddr: advertisedAddr(cfg.Server.Host, cfg.Messaging.Gateway.Port),
			Rack:        cfg.Messaging.Presence.Region,
			Seeds:       resolveSeeds(*joinAddr, cfg.Cluster.Seeds),
			APIKey:      cfg.Security.APIKey,
			Interval:    cfg.Cluster.SessionTimeout(15*time.Second) / 3,
		})
	}

	var metaApplier streamctl.Applier
	if agent != nil {
		metaApplier = agent
	}
	messaging, err := buildMessaging(cfg, nodeID, metaNode, metaApplier)
	if err != nil {
		log.Fatalf("[server] failed to start messaging subsystem: %v", err)
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

	// Initialize cache/KV store.
	var kvStore *cache.Store
	cacheQuit := make(chan struct{})
	if cfg.Cache.Enabled {
		kvStore = cache.NewStore(cfg.Cache.MaxKeys)
		log.Printf("[server] cache enabled (max_keys=%d, default_ttl=%dms, cleanup=%ds)",
			cfg.Cache.MaxKeys, cfg.Cache.DefaultTTL, cfg.Cache.CleanupSec)

		// Start cache cleanup goroutine.
		cleanupInterval := time.Duration(cfg.Cache.CleanupSec) * time.Second
		if cleanupInterval <= 0 {
			cleanupInterval = 10 * time.Second
		}
		go func() {
			ticker := time.NewTicker(cleanupInterval)
			defer ticker.Stop()
			for {
				select {
				case <-ticker.C:
					kvStore.CleanExpired()
				case <-cacheQuit:
					return
				}
			}
		}()
	}

	// Start HTTP server.
	httpServer := api.NewHTTPServer(activeBroker, m, cfg.Server, cfg.Security.APIKey)
	if raftNode != nil {
		httpServer.SetClusterNode(raftNode)
	}
	if metaNode != nil {
		httpServer.SetMetadataNode(metaNode)
		httpServer.SetController(controller)
	}
	if kvStore != nil {
		httpServer.SetCache(kvStore, cfg.Cache.DefaultTTL)
	}
	if messaging != nil {
		httpServer.SetMessagingStats(&messagingStatsAdapter{st: messaging})
		// Lets peers deliver writes for partitions this node leads.
		httpServer.SetStreamLog(messaging.Log)
		// Lets peers read and report presence for shards this node owns.
		httpServer.SetPresenceRegistry(messaging.Presence)
		// Without a dedicated gateway port, share the admin listener. Fine for
		// development; production should separate the two planes.
		if cfg.Messaging.Gateway.Enabled && cfg.Messaging.Gateway.Port == 0 {
			path := cfg.Messaging.Gateway.Path
			if path == "" {
				path = "/ws"
			}
			httpServer.Handle(path, messaging.Gateway)
			log.Printf("[server] gateway mounted on admin HTTP server at %s", path)
		}
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
		queueAddr := ""
		if cfg.Cluster.QueuePlane {
			queueAddr = cfg.Cluster.RaftAddr
		}
		metaAddr := cfg.Cluster.MetaRaftAddr
		if metaAddr == "" {
			metaAddr = derivedMetaAddr(cfg.Cluster.RaftAddr)
		}
		go func() {
			time.Sleep(2 * time.Second) // give local raft time to start
			discoverAndJoin(seeds, cfg.Cluster.NodeID, queueAddr, metaAddr,
				cfg.Security.APIKey, cfg.Cluster.NonVoter)
		}()
	}

	// Register with the control plane and start heartbeating.
	//
	// Joining Raft and registering as a broker are different facts. Raft
	// membership says "this node participates in consensus"; registration says
	// "this node can host partitions, and here is where to reach it". A node
	// can be the former without being the latter — a dedicated controller holds
	// no partitions at all.
	if agent != nil {
		agent.Start()
	}

	// Wait for shutdown signal.
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	sig := <-sigCh
	log.Printf("[server] received signal %s, shutting down...", sig)

	// Graceful leave: remove self from cluster before shutting down.
	if raftNode != nil && !cfg.Cluster.Bootstrap && len(seeds) > 0 {
		gracefulLeave(seeds, cfg.Cluster.NodeID, cfg.Security.APIKey)
	}

	close(cacheQuit)
	// The agent stops first: a node on its way out should stop claiming to be
	// alive before it stops being able to serve, so the controller fences it
	// promptly instead of routing to a corpse.
	if agent != nil {
		agent.Close()
	}
	if controller != nil {
		controller.Close()
	}
	if messaging != nil {
		messaging.Close()
	}
	tcpServer.Shutdown()
	httpServer.Shutdown()
	sched.Stop()
	if kvStore != nil {
		kvStore.Close()
	}
	if raftNode != nil {
		raftNode.Shutdown()
	}
	if metaNode != nil {
		metaNode.Shutdown()
	}
	b.Close()
	log.Println("[server] BoltQ stopped")
}

// advertisedAddr builds the address other nodes should use to reach a listener
// on this host.
//
// A listener bound to 0.0.0.0 accepts from everywhere but is meaningless as an
// address to dial, so the hostname is substituted — this is what a node
// publishes about itself, not what it binds to. Port 0 means the listener is
// not running, and an empty string is the honest way to say so.
func advertisedAddr(host string, port int) string {
	if port <= 0 {
		return ""
	}
	if host == "" || host == "0.0.0.0" || host == "::" {
		h, err := os.Hostname()
		if err != nil || h == "" {
			return ""
		}
		host = h
	}
	return fmt.Sprintf("%s:%d", host, port)
}

// derivedMetaAddr places the control-plane listener one port above the queue
// group's.
//
// Deriving it means an existing config keeps working without naming a port it
// never had to name before, and the relationship stays obvious in a `ss -ltn`:
// 9100 is queue consensus, 9101 is control consensus.
func derivedMetaAddr(raftAddr string) string {
	host, portStr, err := net.SplitHostPort(raftAddr)
	if err != nil {
		return "0.0.0.0:9101"
	}
	port, err := strconv.Atoi(portStr)
	if err != nil {
		return "0.0.0.0:9101"
	}
	return net.JoinHostPort(host, strconv.Itoa(port+1))
}

// advertisedListen converts a bind address into one a peer can dial.
//
// "0.0.0.0:9200" is a perfectly good thing to listen on and a useless thing to
// publish: a follower that dials it reaches itself. Only the port survives; the
// host becomes this node's own name, which is what the rest of the cluster
// resolves. Empty in, empty out — a listener that is not configured is not
// advertised.
func advertisedListen(listen string) string {
	if listen == "" {
		return ""
	}
	host, portStr, err := net.SplitHostPort(listen)
	if err != nil {
		return ""
	}
	port, err := strconv.Atoi(portStr)
	if err != nil {
		return ""
	}
	return advertisedAddr(host, port)
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
func discoverAndJoin(seeds []string, nodeID, queueAddr, metaAddr, apiKey string, nonVoter bool) {
	payload := map[string]interface{}{
		"node_id":   nodeID,
		"addr":      queueAddr,
		"meta_addr": metaAddr,
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
			req, err := http.NewRequest(http.MethodPost, url, bytes.NewReader(body))
			if err != nil {
				continue
			}
			req.Header.Set("Content-Type", "application/json")
			// /cluster/join sits behind the same API key as every other control
			// endpoint. Omitting it means every join is rejected with 401 the
			// moment a key is configured — which is to say, in any deployment
			// that is not wide open.
			if apiKey != "" {
				req.Header.Set("X-API-Key", apiKey)
			}
			resp, err := client.Do(req)
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
func gracefulLeave(seeds []string, nodeID, apiKey string) {
	body, _ := json.Marshal(map[string]string{"node_id": nodeID})
	client := &http.Client{Timeout: 5 * time.Second}

	for _, seed := range seeds {
		url := fmt.Sprintf("http://%s/cluster/leave", seed)
		req, err := http.NewRequest(http.MethodPost, url, bytes.NewReader(body))
		if err != nil {
			continue
		}
		req.Header.Set("Content-Type", "application/json")
		if apiKey != "" {
			req.Header.Set("X-API-Key", apiKey)
		}
		resp, err := client.Do(req)
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
