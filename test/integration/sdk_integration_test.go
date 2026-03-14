package integration

import (
	"fmt"
	"testing"
	"time"

	boltq "github.com/boltq/boltq/client/golang"
	"github.com/boltq/boltq/internal/api"
	"github.com/boltq/boltq/internal/broker"
	"github.com/boltq/boltq/internal/config"
	"github.com/boltq/boltq/internal/metrics"
)

func TestSDKEndToEnd(t *testing.T) {
	// 1. Setup Server
	b := broker.New(broker.Config{QueueCap: 100})
	m := metrics.Global()
	server := api.NewTCPServer(b, m, config.ServerConfig{}, "")
	
	addr := "127.0.0.1:9095"
	if err := server.Start(addr); err != nil {
		t.Fatal(err)
	}
	defer server.Shutdown()

	// 2. Setup Client
	client := boltq.New(addr)
	if err := client.Connect(); err != nil {
		t.Fatal(err)
	}
	defer client.Close()

	t.Run("DelayedAndTTL", func(t *testing.T) {
		client := boltq.New(addr)
		if err := client.Connect(); err != nil { t.Fatal(err) }
		defer client.Close()

		// Publish with 200ms delay and 1s TTL
		opts := &boltq.PublishOptions{
			Delay: 200 * time.Millisecond,
			TTL:   1 * time.Second,
		}
		fmt.Printf("Publishing delayed message...\n")
		id, err := client.Publish("sdk_test", map[string]string{"msg": "delayed"}, nil, opts)
		if err != nil { t.Fatal(err) }
		fmt.Printf("Published ID: %s\n", id)

		// Try consume immediately - should be empty
		msg, _ := client.Consume("sdk_test")
		if msg != nil {
			t.Fatalf("expected no message yet due to delay, got msg id %s", msg.ID)
		}
		fmt.Printf("Immediate consume returned empty (correct)\n")

		// Wait for delay
		time.Sleep(300 * time.Millisecond)
		fmt.Printf("Running background processing...\n")
		b.ProcessAdvancedFeatures() 

		msg, err = client.Consume("sdk_test")
		if err != nil { t.Fatalf("consume error: %v", err) }
		if msg == nil { t.Fatal("expected message after delay, got nil") }
		fmt.Printf("Consumed message after delay: %s\n", msg.ID)
	})

	t.Run("Prefetch", func(t *testing.T) {
		client := boltq.New(addr)
		if err := client.Connect(); err != nil { t.Fatal(err) }
		defer client.Close()

		fmt.Printf("Setting prefetch to 1...\n")
		err := client.SetPrefetch(1)
		if err != nil { t.Fatalf("set prefetch error: %v", err) }
		
		client.Publish("prefetch_test", "1", nil, nil)
		client.Publish("prefetch_test", "2", nil, nil)

		fmt.Printf("Consuming first message...\n")
		msg1, err := client.Consume("prefetch_test")
		if err != nil { t.Fatalf("consume 1 error: %v", err) }
		if msg1 == nil { t.Fatal("expected msg1") }
		fmt.Printf("Consumed msg1: %s\n", msg1.ID)

		// Second consume should fail due to prefetch
		fmt.Printf("Attempting second consume (should fail)...\n")
		msg2, err := client.Consume("prefetch_test")
		if err == nil { t.Fatal("expected error due to prefetch limit, but got no error") }
		fmt.Printf("Second consume failed as expected with error: %v\n", err)
		if msg2 != nil { t.Fatalf("expected no message due to prefetch, got %s", msg2.ID) }

		fmt.Printf("Acking first message...\n")
		err = client.Ack(msg1.ID)
		if err != nil { t.Fatalf("ack error: %v", err) }

		// Now should succeed
		fmt.Printf("Consuming second message...\n")
		msg2, err = client.Consume("prefetch_test")
		if err != nil { t.Fatalf("consume 2 error: %v", err) }
		if msg2 == nil { t.Fatal("expected msg2 after ACK, got nil") }
		fmt.Printf("Consumed msg2: %s\n", msg2.ID)
	})

	t.Run("DurableSubscribe", func(t *testing.T) {
		client := boltq.New(addr)
		if err := client.Connect(); err != nil { t.Fatal(err) }
		defer client.Close()

		fmt.Printf("Testing Durable Subscribe...\n")
		ch, err := client.Subscribe("pubsub_test", "sub1", true)
		if err != nil { t.Fatalf("subscribe error: %v", err) }

		fmt.Printf("Publishing to topic...\n")
		client.PublishTopic("pubsub_test", "hello world", nil, nil)

		select {
		case msg := <-ch:
			if msg == nil { t.Fatal("expected message, got nil") }
			fmt.Printf("Received message via subscription: %s\n", string(msg.Payload))
		case <-time.After(2 * time.Second):
			t.Fatal("timed out waiting for pubsub message")
		}
	})
}
