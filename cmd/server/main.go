package main

import (
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/boltq/boltq/internal/api"
	"github.com/boltq/boltq/internal/broker"
	"github.com/boltq/boltq/internal/config"
	"github.com/boltq/boltq/internal/metrics"
	"github.com/boltq/boltq/internal/scheduler"
	"github.com/boltq/boltq/internal/storage"
)

func main() {
	configPath := flag.String("config", "", "path to config file (JSON)")
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

	// Initialize broker.
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

	// Start scheduler.
	sched := scheduler.New(b, time.Second)
	sched.Start()

	// Start servers.
	m := metrics.Global()

	// Start TCP server.
	tcpServer := api.NewTCPServer(b, m, cfg.Security.APIKey)
	tcpAddr := fmt.Sprintf("%s:%d", cfg.Server.Host, cfg.Server.TCPPort)
	if err := tcpServer.Start(tcpAddr); err != nil {
		log.Fatalf("[server] failed to start TCP server: %v", err)
	}

	// Start HTTP server.
	httpServer := api.NewHTTPServer(b, m, cfg.Security.APIKey)
	httpAddr := fmt.Sprintf("%s:%d", cfg.Server.Host, cfg.Server.HTTPPort)
	go func() {
		if err := httpServer.Start(httpAddr); err != nil {
			log.Printf("[server] HTTP server stopped: %v", err)
		}
	}()

	log.Printf("[server] BoltQ started (HTTP=%s, TCP=%s)", httpAddr, tcpAddr)

	// Wait for shutdown signal.
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	sig := <-sigCh
	log.Printf("[server] received signal %s, shutting down...", sig)

	tcpServer.Shutdown()
	httpServer.Shutdown()
	sched.Stop()
	b.Close()
	log.Println("[server] BoltQ stopped")
}
