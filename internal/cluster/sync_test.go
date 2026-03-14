package cluster

import (
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/boltq/boltq/internal/broker"
	"github.com/boltq/boltq/internal/config"
	"github.com/boltq/boltq/pkg/protocol"
)

func TestClusterSync(t *testing.T) {
	fmt.Println("--- Starting TestClusterSync ---")
	dir1, err := os.MkdirTemp("", "boltq-sync-1")
	if err != nil {
		t.Fatal(err)
	}
	dir2, err := os.MkdirTemp("", "boltq-sync-2")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir1)
	defer os.RemoveAll(dir2)

	cfg1 := config.ClusterConfig{
		NodeID:    "node1",
		RaftAddr:  "127.0.0.1:9121",
		RaftDir:   dir1,
		Bootstrap: true,
	}
	cfg2 := config.ClusterConfig{
		NodeID:    "node2",
		RaftAddr:  "127.0.0.1:9122",
		RaftDir:   dir2,
		Bootstrap: false,
	}

	b1 := broker.New(broker.Config{})
	node1, err := NewRaftNode(cfg1, b1)
	if err != nil {
		t.Fatal(err)
	}
	defer node1.Shutdown()

	b2 := broker.New(broker.Config{})
	node2, err := NewRaftNode(cfg2, b2)
	if err != nil {
		t.Fatal(err)
	}
	defer node2.Shutdown()

	cb1 := NewClusterBroker(node1, b1)
	cb2 := NewClusterBroker(node2, b2)
	_ = cb2

	fmt.Println("Waiting for leader...")
	time.Sleep(3 * time.Second)
	if !node1.IsLeader() {
		t.Fatal("node1 should be leader")
	}
	fmt.Println("Leader elected: node1")

	fmt.Println("Joining node2...")
	if err := node1.Join("node2", "127.0.0.1:9122"); err != nil {
		t.Fatal(err)
	}
	fmt.Println("Node2 joined. Waiting for replication...")
	time.Sleep(2 * time.Second)

	// 1. Test Purge Sync
	fmt.Println("Testing Purge Sync...")
	if err := cb1.Publish("test-purge", protocol.NewMessage("test-purge", []byte("data"), nil)); err != nil {
		t.Fatalf("publish failed: %v", err)
	}
	fmt.Println("Published message to test-purge. Waiting for replication...")
	time.Sleep(1 * time.Second)
	
	fmt.Println("Calling PurgeQueue on cb1...")
	count, err := cb1.PurgeQueue("test-purge")
	if err != nil {
		t.Fatalf("purge failed: %v", err)
	}
	fmt.Printf("Purged %d messages. Waiting for purge replication...\n", count)
	if count != 1 {
		t.Fatalf("expected 1 purged message, got %d", count)
	}
	
	time.Sleep(1 * time.Second)
	stats2 := b2.Stats()
	if stats2.Queues["test-purge"] != 0 {
		t.Fatalf("node2 stats not synced after purge: expected 0, got %d", stats2.Queues["test-purge"])
	}
	fmt.Println("Purge sync test PASSED")

	// 2. Test Delayed Message Sync & Promotion
	fmt.Println("Testing Delayed Message Sync & Promotion...")
	msg := protocol.NewMessage("test-delay", []byte("delayed"), nil)
	msg.Delay = int64(time.Second)
	if err := cb1.Publish("test-delay", msg); err != nil {
		t.Fatal(err)
	}
	
	fmt.Println("Published delayed message. Waiting...")
	time.Sleep(1 * time.Second)
	
	state1 := b1.SnapshotFullState()
	state2 := b2.SnapshotFullState()
	fmt.Printf("Delayed: b1=%d, b2=%d\n", len(state1.Delayed), len(state2.Delayed))
	if len(state1.Delayed) != 1 || len(state2.Delayed) != 1 {
		t.Fatalf("delayed msg not replicated: b1=%d, b2=%d", len(state1.Delayed), len(state2.Delayed))
	}

	fmt.Println("Calling ProcessAdvancedFeatures for promotion...")
	cb1.ProcessAdvancedFeatures()
	
	fmt.Println("Waiting for promotion replication...")
	time.Sleep(1 * time.Second)
	
	stats1 := b1.Stats()
	stats2 = b2.Stats()
	fmt.Printf("Active: b1=%d, b2=%d\n", stats1.Queues["test-delay"], stats2.Queues["test-delay"])
	if stats1.Queues["test-delay"] != 1 || stats2.Queues["test-delay"] != 1 {
		t.Fatalf("promotion not synced: stats1=%d, stats2=%d", stats1.Queues["test-delay"], stats2.Queues["test-delay"])
	}
	fmt.Println("Delayed promotion test PASSED")

	// 3. Test Snapshot/Restore
	fmt.Println("Testing SnapshotFullState...")
	msg2 := protocol.NewMessage("test-snap", []byte("snap"), nil)
	msg2.Delay = int64(10 * time.Second)
	cb1.Publish("test-snap", msg2)
	time.Sleep(500 * time.Millisecond)

	fullState := b1.SnapshotFullState()
	if len(fullState.Delayed) != 1 {
		t.Fatalf("snapshot should include 1 delayed message, got %d", len(fullState.Delayed))
	}
	fmt.Println("Snapshot test PASSED")
	fmt.Println("--- TestClusterSync Finished ---")
}

func TestIdempotency(t *testing.T) {
	fmt.Println("--- Starting TestIdempotency ---")
	dir1, _ := os.MkdirTemp("", "boltq-idemp-1")
	dir2, _ := os.MkdirTemp("", "boltq-idemp-2")
	defer os.RemoveAll(dir1)
	defer os.RemoveAll(dir2)

	b1 := broker.New(broker.Config{})
	node1, _ := NewRaftNode(config.ClusterConfig{NodeID: "node1", RaftAddr: "127.0.0.1:9131", RaftDir: dir1, Bootstrap: true}, b1)
	defer node1.Shutdown()

	b2 := broker.New(broker.Config{})
	node2, _ := NewRaftNode(config.ClusterConfig{NodeID: "node2", RaftAddr: "127.0.0.1:9132", RaftDir: dir2}, b2)
	defer node2.Shutdown()

	cb1 := NewClusterBroker(node1, b1)
	time.Sleep(2 * time.Second) // wait for leader

	node1.Join("node2", "127.0.0.1:9132")
	time.Sleep(1 * time.Second)

	// 1. Double Publish Test
	fmt.Println("Testing Double Publish Idempotency...")
	msgID := "idempotent-msg-1"
	msg := &protocol.Message{ID: msgID, Payload: []byte("data")}
	
	// Apply first time
	cb1.Publish("test-idemp", msg)
	// Apply second time (same ID)
	cb1.Publish("test-idemp", msg)
	
	time.Sleep(1 * time.Second)
	stats1 := b1.Stats()
	fmt.Printf("Message count after double publish: %d\n", stats1.Queues["test-idemp"])
	if stats1.Queues["test-idemp"] != 1 {
		t.Fatalf("idempotency failed: expected 1 message, got %d", stats1.Queues["test-idemp"])
	}

	// 2. Double Promotion Test
	fmt.Println("Testing Double Promotion Idempotency...")
	delayMsg := &protocol.Message{ID: "delay-id-1", Payload: []byte("delayed")}
	delayMsg.Delay = int64(time.Hour) // long delay
	cb1.Publish("test-delay-idemp", delayMsg)
	time.Sleep(500 * time.Millisecond)

	// Simulate double promotion by applying the command twice
	cmd := &RaftCommand{Type: CmdRaftPromote, MessageID: "delay-id-1"}
	node1.Apply(cmd, time.Second)
	node1.Apply(cmd, time.Second)

	time.Sleep(500 * time.Millisecond)
	stats1 = b1.Stats()
	if stats1.Queues["test-delay-idemp"] != 1 {
		t.Fatalf("promotion idempotency failed: expected 1 message, got %d", stats1.Queues["test-delay-idemp"])
	}

	fmt.Println("Idempotency tests PASSED")
}
