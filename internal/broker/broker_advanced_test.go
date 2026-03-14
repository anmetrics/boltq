package broker

import (
	"testing"
	"time"

	"github.com/boltq/boltq/pkg/protocol"
)

func TestMessageTTL(t *testing.T) {
	b := newTestBroker()
	defer b.Close()

	// 1. Message with very short TTL
	msg := protocol.NewMessage("test_ttl", []byte(`"expired"`), nil)
	msg.TTL = int64(time.Millisecond * 10)
	b.Publish("test_ttl", msg)

	// Wait for expiration
	time.Sleep(time.Millisecond * 50)

	// Should be skipped by TryConsume
	got := b.TryConsume("test_ttl")
	if got != nil {
		t.Fatal("expected message to be expired and skipped")
	}
}

func TestDelayedMessage(t *testing.T) {
	b := newTestBroker()
	defer b.Close()

	// 1. Message with delay
	msg := protocol.NewMessage("test_delay", []byte(`"delayed"`), nil)
	msg.Delay = int64(time.Millisecond * 100)
	b.Publish("test_delay", msg)

	// Should not be available immediately
	got := b.TryConsume("test_delay")
	if got != nil {
		t.Fatal("expected message to be delayed and not available")
	}

	// Manually process advanced features (simulating scheduler)
	time.Sleep(time.Millisecond * 150)
	b.ProcessAdvancedFeatures()

	// Now it should be available
	got = b.TryConsume("test_delay")
	if got == nil {
		t.Fatal("expected message to be available after delay")
	}
	if got.ID != msg.ID {
		t.Fatalf("expected id %s, got %s", msg.ID, got.ID)
	}
}
