package broker

import (
	"os"
	"testing"
	"fmt"
	"time"
	"strings"
	"sync"
	"sync/atomic"
	"math/rand"

	"github.com/boltq/boltq/internal/storage"
	"github.com/boltq/boltq/internal/wal"
	"github.com/boltq/boltq/pkg/protocol"
)

func TestDiskPersistenceAndOrder(t *testing.T) {
	dataDir := "./temp_wal_test"
	os.RemoveAll(dataDir)
	defer os.RemoveAll(dataDir)

	// --- Stage 1: Publish 3 messages ---
	s, _ := storage.NewDiskStorage(dataDir)
	store := storage.Storage(s)
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
	s2, _ := storage.NewDiskStorage(dataDir)
	store2 := storage.Storage(s2)
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
	s3, _ := storage.NewDiskStorage(dataDir)
	store3 := storage.Storage(s3)
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

func TestWALCompaction(t *testing.T) {
	fmt.Println("--- Starting TestWALCompaction ---")
	dataDir, _ := os.MkdirTemp("", "boltq-compaction-test")
	defer os.RemoveAll(dataDir)

	s, _ := storage.NewDiskStorage(dataDir)
	store := storage.Storage(s)
	b := New(Config{Storage: store, QueueCap: 1000})

	// 1. Publish 100 messages
	fmt.Println("Publishing 100 messages...")
	for i := 0; i < 100; i++ {
		b.Publish("test", protocol.NewMessage("test", []byte(fmt.Sprintf("msg-%d", i)), nil))
	}

	// 2. Ack 90 messages
	fmt.Println("Acking 90 messages...")
	// Note: We need to consume to Ack
	for i := 0; i < 90; i++ {
		m := b.TryConsume("test")
		if m != nil {
			b.Ack(m.ID)
		}
	}

	if b.Stats().Queues["test"] != 10 {
		t.Fatalf("expected 10 messages left, got %d", b.Stats().Queues["test"])
	}

	walPath := dataDir + "/queue.wal"
	infoBefore, _ := os.Stat(walPath)
	sizeBefore := infoBefore.Size()
	fmt.Printf("WAL size before compaction: %d bytes\n", sizeBefore)

	// 3. Trigger Compaction (Checkpoint)
	fmt.Println("Triggering Checkpoint...")
	if err := b.Checkpoint(); err != nil {
		t.Fatalf("Checkpoint failed: %v", err)
	}

	infoAfter, _ := os.Stat(walPath)
	sizeAfter := infoAfter.Size()
	fmt.Printf("WAL size after compaction: %d bytes\n", sizeAfter)

	if sizeAfter >= sizeBefore {
		t.Errorf("WAL failed to shrink: before=%d, after=%d", sizeBefore, sizeAfter)
	}

	// 4. Verify data integrity after reload
	b.Close()
	
	s2, _ := storage.NewDiskStorage(dataDir)
	store2 := storage.Storage(s2)
	b2 := New(Config{Storage: store2, QueueCap: 1000})
	
	// Simulation of main.go recovery (not strictly needed for b2 if we just use b2, 
	// but let's test absolute recovery from the compacted file)
	records, _ := store2.ReadAllRecords()
	for _, r := range records {
		if r.Type == wal.RecordPublish {
			b2.IngestRecovered(r.Message)
		}
	}
	
	statsFinal := b2.Stats()
	if statsFinal.Queues["test"] != 10 {
		t.Errorf("integrity lost after compaction: expected 10 messages, got %d", statsFinal.Queues["test"])
	}
	fmt.Println("WAL Compaction test PASSED")
}

func TestAutoCompaction(t *testing.T) {
	fmt.Println("--- Starting TestAutoCompaction ---")
	dataDir, err := os.MkdirTemp("", "boltq-auto-compaction")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dataDir)

	// Set a small threshold to encourage multiple compactions in test
	threshold := int64(1000) 
	s, _ := storage.NewDiskStorage(dataDir)
	store := storage.Storage(s)
	b := New(Config{
		Storage:             store,
		QueueCap:            1000,
		CompactionThreshold: threshold,
	})

	// 1. Publish messages until we exceed threshold
	fmt.Println("Publishing messages to exceed threshold...")
	for i := 0; i < 50; i++ {
		// Use a larger payload to make size differences more obvious
		payload := fmt.Sprintf("auto-msg-%d-payload-padding-padding-padding-padding-padding-padding-padding-padding-padding-padding", i)
		if err := b.Publish("test", protocol.NewMessage("test", []byte(payload), nil)); err != nil {
			t.Fatalf("publish %d failed: %v", i, err)
		}
	}

	sizeOriginal := b.store.Size()
	fmt.Printf("Initial WAL size: %d bytes (Threshold: %d)\n", sizeOriginal, threshold)
	if sizeOriginal == 0 {
		t.Fatal("Initial WAL size is 0! Something is wrong.")
	}

	// 2. Ack almost all messages
	fmt.Println("Acking messages...")
	for i := 0; i < 45; i++ {
		m := b.TryConsume("test")
		if m != nil {
			b.Ack(m.ID)
		}
	}

	// 3. Publish one more message to trigger AutoCheckpoint
	fmt.Println("Triggering AutoCheckpoint with a new publish...")
	triggerPayload := "trigger-compaction"
	b.Publish("test", protocol.NewMessage("test", []byte(triggerPayload), nil))

	// 4. Wait for background compaction (async)
	fmt.Println("Waiting for background compaction to reach bottom...")
	success := false
	var sizeAfter int64
	// Threshold for 6 msgs with ~100 bytes each should be < 2000 bytes.
	for i := 0; i < 50; i++ {
		time.Sleep(100 * time.Millisecond)
		sizeAfter = b.store.Size()
		// We expect 6 messages. (5 remaining + 1 trigger). 
		// Each should be approx 150-200 bytes. Total < 1500.
		if sizeAfter < 2000 && sizeAfter > 0 {
			fmt.Printf("WAL successfully compacted to final size: %d bytes\n", sizeAfter)
			success = true
			// Wait a bit more to ensure background goroutine finishes its loop if any
			time.Sleep(200 * time.Millisecond) 
			break
		}
	}

	if !success {
		t.Errorf("Automatic compaction failed to reach final state. Original: %d, Final: %d", sizeOriginal, sizeAfter)
	}

	// 5. Final integrity check
	b.Close()
	s2_new, _ := storage.NewDiskStorage(dataDir)
	store2 := storage.Storage(s2_new)
	
	records, err := store2.ReadAllRecords()
	if err != nil {
		t.Fatal(err)
	}
	recoveredCount := 0
	for _, r := range records {
		if r.Type == wal.RecordPublish {
			payload := string(r.Message.Payload)
			if strings.Contains(payload, "auto-msg-") || strings.Contains(payload, "trigger-compaction") {
				recoveredCount++
			}
		}
	}
	
	// Should have 5 (remaining from 50-45) + 1 (trigger) = 6 messages
	if recoveredCount != 6 {
		t.Errorf("Data integrity loss! Expected 6 messages, got %d", recoveredCount)
	}

	fmt.Println("Auto Compaction test PASSED")
}

func TestStressAutoCompaction(t *testing.T) {
	fmt.Println("--- Starting TestStressAutoCompaction ---")
	dataDir, _ := os.MkdirTemp("", "boltq-stress-compaction")
	defer os.RemoveAll(dataDir)

	threshold := int64(2000)
	s, _ := storage.NewDiskStorage(dataDir)
	store := storage.Storage(s)
	b := New(Config{
		Storage:             store,
		QueueCap:            5000,
		CompactionThreshold: threshold,
	})

	const numMessages = 1000
	const numPublishers = 5
	const numConsumers = 3
	const targetAcks = 700
	
	msgTrack := make(map[string]bool)
	var trackMu sync.Mutex
	
	pubWg := sync.WaitGroup{}
	// 1. Concurrent Publishers
	for p := 0; p < numPublishers; p++ {
		pubWg.Add(1)
		go func(pID int) {
			defer pubWg.Done()
			for i := 0; i < numMessages/numPublishers; i++ {
				id := fmt.Sprintf("stress-%d-%d", pID, i)
				payload := fmt.Sprintf("payload-%s-padding-padding-padding-%d", id, rand.Intn(100))
				msg := protocol.NewMessage("stress", []byte(payload), nil)
				msg.ID = id
				
				trackMu.Lock()
				msgTrack[id] = true
				trackMu.Unlock()

				if err := b.Publish("stress", msg); err != nil {
					t.Errorf("Publish failed: %v", err)
				}
				// Tiny sleep to interleave
				if i%10 == 0 {
					time.Sleep(1 * time.Millisecond)
				}
			}
		}(p)
	}

	// Wait for publishers to finish so we know EXACTLY what was sent
	pubWg.Wait()
	fmt.Printf("Publishers finished. Total published: %d\n", numMessages)

	// 2. Concurrent Consumers/Ackers
	ackCount := int32(0)
	consWg := sync.WaitGroup{}
	for c := 0; c < numConsumers; c++ {
		consWg.Add(1)
		go func() {
			defer consWg.Done()
			for {
				if atomic.LoadInt32(&ackCount) >= int32(targetAcks) {
					return
				}
				m := b.TryConsume("stress")
				if m == nil {
					time.Sleep(10 * time.Millisecond)
					continue
				}

				// Randomly Ack or Nack
				if rand.Float32() < 0.8 {
					if atomic.LoadInt32(&ackCount) < int32(targetAcks) {
						if err := b.Ack(m.ID); err == nil {
							trackMu.Lock()
							delete(msgTrack, m.ID)
							trackMu.Unlock()
							atomic.AddInt32(&ackCount, 1)
						}
					} else {
						// Reached target, just nack to put it back
						b.Nack(m.ID)
						return
					}
				} else {
					b.Nack(m.ID)
				}
			}
		}()
	}

	consWg.Wait()
	fmt.Printf("Stress test finished. Acks: %d, Expected Remaining: %d, Map Remaining: %d\n", 
		ackCount, numMessages-targetAcks, len(msgTrack))
	fmt.Printf("Current WAL size: %d bytes (Threshold: %d)\n", b.store.Size(), threshold)

	// 3. Trigger one last compaction
	b.Checkpoint()
	time.Sleep(200 * time.Millisecond)
	
	// 4. Recovery check
	b.Close()
	s2, _ := storage.NewDiskStorage(dataDir)
	store2 := storage.Storage(s2)
	records, err := store2.ReadAllRecords()
	if err != nil {
		t.Fatal(err)
	}

	recoveredMsgs := make(map[string]bool)
	for _, r := range records {
		if r.Type == wal.RecordPublish {
			if strings.HasPrefix(r.Message.ID, "stress-") {
				recoveredMsgs[r.Message.ID] = true
			}
		}
	}

	fmt.Printf("Recovered messages: %d\n", len(recoveredMsgs))

	// Verify all remaining messages in msgTrack are in recoveredMsgs
	trackMu.Lock()
	defer trackMu.Unlock()
	for id := range msgTrack {
		if !recoveredMsgs[id] {
			t.Errorf("Message %s was lost during compaction/recovery!", id)
		}
	}

	// Verify NO extra messages were recovered (that were acked)
	for id := range recoveredMsgs {
		if !msgTrack[id] {
			t.Errorf("Message %s was recovered but should have been deleted (ACKED)!", id)
		}
	}

	if len(recoveredMsgs) != len(msgTrack) {
		t.Errorf("Count mismatch! Tracked: %d, Recovered: %d", len(msgTrack), len(recoveredMsgs))
	}

	if !t.Failed() {
		fmt.Println("Stress Auto Compaction test PASSED")
	}
}
