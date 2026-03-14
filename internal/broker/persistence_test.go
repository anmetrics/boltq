package broker

import (
	"os"
	"testing"
	"fmt"

	"github.com/boltq/boltq/internal/storage"
	"github.com/boltq/boltq/internal/wal"
	"github.com/boltq/boltq/pkg/protocol"
)

func TestDiskPersistenceAndOrder(t *testing.T) {
	dataDir := "./temp_wal_test"
	os.RemoveAll(dataDir)
	defer os.RemoveAll(dataDir)

	// --- Stage 1: Publish 3 messages ---
	store, _ := storage.NewDiskStorage(dataDir)
	b := New(Config{Storage: store})

	for i := 1; i <= 3; i++ {
		id := fmt.Sprintf("msg%d", i)
		b.Publish("test", &protocol.Message{ID: id, Topic: "test", Payload: []byte(id)})
	}
	
	if b.Stats().Queues["test"] != 3 {
		t.Errorf("expected 3 messages, got %d", b.Stats().Queues["test"])
	}
	b.Close()

	// --- Stage 2: Recovery & Verify Order ---
	store2, _ := storage.NewDiskStorage(dataDir)
	b2 := New(Config{Storage: store2})
	
	records, _ := store2.ReadAllRecords()
	// Simulate main.go recovery logic
	msgs := make(map[string]*protocol.Message)
	order := []string{}
	for _, rec := range records {
		if rec.Type == wal.RecordPublish {
			msgs[rec.Message.ID] = rec.Message
			order = append(order, rec.Message.ID)
		} else if rec.Type == wal.RecordAck {
			delete(msgs, rec.MsgID)
		}
	}
	for _, id := range order {
		if m, ok := msgs[id]; ok {
			b2.IngestRecovered(m)
		}
	}
	
	if b2.Stats().Queues["test"] != 3 {
		t.Errorf("recovery failed: expected 3 messages, got %d", b2.Stats().Queues["test"])
	}

	// Verify order
	for i := 1; i <= 3; i++ {
		m := b2.TryConsume("test")
		expected := fmt.Sprintf("msg%d", i)
		if m == nil || m.ID != expected {
			t.Errorf("Order broken at index %d: expected %s, got %v", i, expected, m)
		}
		b2.Ack(m.ID)
	}
	b2.Close()

	// --- Stage 3: Verify Persistence (ACKed messages should NOT be re-recovered) ---
	store3, _ := storage.NewDiskStorage(dataDir)
	b3 := New(Config{Storage: store3})
	
	records3, _ := store3.ReadAllRecords()
	msgs3 := make(map[string]*protocol.Message)
	order3 := []string{}
	for _, rec := range records3 {
		if rec.Type == wal.RecordPublish {
			msgs3[rec.Message.ID] = rec.Message
			order3 = append(order3, rec.Message.ID)
		} else if rec.Type == wal.RecordAck {
			delete(msgs3, rec.MsgID)
		}
	}
	for _, id := range order3 {
		if m, ok := msgs3[id]; ok {
			b3.IngestRecovered(m)
		}
	}
	
	finalCount := b3.Stats().Queues["test"]
	if finalCount != 0 {
		t.Errorf("BUG: ACKed messages were reloaded! Got %d messages, expected 0.", finalCount)
	}
	b3.Close()
}
