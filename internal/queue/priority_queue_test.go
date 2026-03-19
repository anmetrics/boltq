package queue

import (
	"fmt"
	"sync"
	"testing"

	"github.com/boltq/boltq/pkg/protocol"
)

func msgWithPriority(topic string, priority int) *protocol.Message {
	msg := protocol.NewMessage(topic, []byte(`"test"`), nil)
	msg.Priority = priority
	return msg
}

func TestPriorityQueueOrdering(t *testing.T) {
	pq := NewPriority("test", 1024)
	defer pq.Close()

	// Push messages with priorities 0, 5, 9
	pq.Push(msgWithPriority("q", 0))
	pq.Push(msgWithPriority("q", 5))
	pq.Push(msgWithPriority("q", 9))

	// Pop order should be 9, 5, 0
	m1 := pq.TryPop()
	m2 := pq.TryPop()
	m3 := pq.TryPop()

	if m1.Priority != 9 {
		t.Errorf("expected priority 9, got %d", m1.Priority)
	}
	if m2.Priority != 5 {
		t.Errorf("expected priority 5, got %d", m2.Priority)
	}
	if m3.Priority != 0 {
		t.Errorf("expected priority 0, got %d", m3.Priority)
	}
}

func TestPriorityQueueFIFOWithinLevel(t *testing.T) {
	pq := NewPriority("test", 1024)
	defer pq.Close()

	msg1 := msgWithPriority("q", 5)
	msg2 := msgWithPriority("q", 5)
	msg3 := msgWithPriority("q", 5)

	pq.Push(msg1)
	pq.Push(msg2)
	pq.Push(msg3)

	got1 := pq.TryPop()
	got2 := pq.TryPop()
	got3 := pq.TryPop()

	if got1.ID != msg1.ID || got2.ID != msg2.ID || got3.ID != msg3.ID {
		t.Error("FIFO order not preserved within same priority level")
	}
}

func TestPriorityQueueHighPriorityFirst(t *testing.T) {
	pq := NewPriority("test", 1024)
	defer pq.Close()

	// Push 100 low priority, then 1 high priority
	for i := 0; i < 100; i++ {
		pq.Push(msgWithPriority("q", 0))
	}
	high := msgWithPriority("q", 9)
	pq.Push(high)

	// First pop should be the high priority message
	got := pq.TryPop()
	if got.ID != high.ID {
		t.Errorf("expected high priority message first, got priority %d", got.Priority)
	}
}

func TestPriorityQueueEmptyPop(t *testing.T) {
	pq := NewPriority("test", 1024)
	defer pq.Close()

	msg := pq.TryPop()
	if msg != nil {
		t.Error("expected nil from empty queue")
	}
}

func TestPriorityQueueSize(t *testing.T) {
	pq := NewPriority("test", 1024)
	defer pq.Close()

	pq.Push(msgWithPriority("q", 0))
	pq.Push(msgWithPriority("q", 5))
	pq.Push(msgWithPriority("q", 9))

	if pq.Size() != 3 {
		t.Errorf("expected size 3, got %d", pq.Size())
	}

	pq.TryPop()
	if pq.Size() != 2 {
		t.Errorf("expected size 2, got %d", pq.Size())
	}
}

func TestPriorityQueuePurge(t *testing.T) {
	pq := NewPriority("test", 1024)
	defer pq.Close()

	for i := 0; i < 10; i++ {
		pq.Push(msgWithPriority("q", i%NumPriorityLevels))
	}

	count := pq.Purge()
	if count != 10 {
		t.Errorf("expected purged 10, got %d", count)
	}
	if pq.Size() != 0 {
		t.Errorf("expected size 0 after purge, got %d", pq.Size())
	}
}

func TestPriorityQueueDrain(t *testing.T) {
	pq := NewPriority("test", 1024)
	defer pq.Close()

	pq.Push(msgWithPriority("q", 0))
	pq.Push(msgWithPriority("q", 5))
	pq.Push(msgWithPriority("q", 9))

	msgs := pq.Drain()
	if len(msgs) != 3 {
		t.Errorf("expected 3 messages from drain, got %d", len(msgs))
	}
	// Drain should return highest priority first
	if msgs[0].Priority != 9 {
		t.Errorf("drain should return highest priority first, got %d", msgs[0].Priority)
	}
	// Size should remain unchanged (drain is non-destructive)
	if pq.Size() != 3 {
		t.Errorf("drain should not remove messages, size is %d", pq.Size())
	}
}

func TestPriorityQueueHasMessage(t *testing.T) {
	pq := NewPriority("test", 1024)
	defer pq.Close()

	msg := msgWithPriority("q", 7)
	pq.Push(msg)

	if !pq.HasMessage(msg.ID) {
		t.Error("expected HasMessage to return true")
	}
	if pq.HasMessage("nonexistent") {
		t.Error("expected HasMessage to return false for unknown ID")
	}
}

func TestPriorityQueueClampPriority(t *testing.T) {
	pq := NewPriority("test", 1024)
	defer pq.Close()

	// Priority below 0 should clamp to 0
	neg := msgWithPriority("q", -5)
	pq.Push(neg)

	// Priority above 9 should clamp to 9
	high := msgWithPriority("q", 99)
	pq.Push(high)

	// High priority (clamped to 9) should come first
	got := pq.TryPop()
	if got.Priority != 99 { // original value preserved, but stored in level 9
		t.Errorf("expected original priority 99, got %d", got.Priority)
	}
}

func TestPriorityQueueConcurrent(t *testing.T) {
	pq := NewPriority("test", 1<<16)
	defer pq.Close()

	const numProducers = 4
	const msgsPerProducer = 1000

	var wg sync.WaitGroup

	// Concurrent producers
	for p := 0; p < numProducers; p++ {
		wg.Add(1)
		go func(producerID int) {
			defer wg.Done()
			for i := 0; i < msgsPerProducer; i++ {
				msg := msgWithPriority("q", i%NumPriorityLevels)
				pq.Push(msg)
			}
		}(p)
	}
	wg.Wait()

	total := numProducers * msgsPerProducer
	if pq.Size() != int64(total) {
		t.Errorf("expected size %d, got %d", total, pq.Size())
	}

	// Concurrent consumers
	consumed := int64(0)
	var consumeMu sync.Mutex
	for c := 0; c < numProducers; c++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			localCount := int64(0)
			for {
				msg := pq.TryPop()
				if msg == nil {
					break
				}
				localCount++
			}
			consumeMu.Lock()
			consumed += localCount
			consumeMu.Unlock()
		}()
	}
	wg.Wait()

	if consumed != int64(total) {
		t.Errorf("expected consumed %d, got %d", total, consumed)
	}
}

func TestPriorityQueueName(t *testing.T) {
	pq := NewPriority("myqueue", 1024)
	if pq.Name() != "myqueue" {
		t.Errorf("expected name 'myqueue', got '%s'", pq.Name())
	}
}

func BenchmarkPriorityQueuePush(b *testing.B) {
	pq := NewPriority("bench", 1<<20)
	defer pq.Close()
	msg := msgWithPriority("bench", 5)

	b.ResetTimer()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		pq.Push(msg)
		if i%10000 == 9999 {
			for j := 0; j < 10000; j++ {
				pq.TryPop()
			}
		}
	}
}

func BenchmarkPriorityQueuePop(b *testing.B) {
	pq := NewPriority("bench", 1<<20)
	defer pq.Close()

	// Pre-fill with messages across priorities
	for i := 0; i < 1<<20-1; i++ {
		pq.Push(msgWithPriority("bench", i%NumPriorityLevels))
	}

	b.ResetTimer()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		msg := pq.TryPop()
		if msg == nil {
			// Refill
			for j := 0; j < 10000; j++ {
				pq.Push(msgWithPriority("bench", j%NumPriorityLevels))
			}
		}
	}
}

func BenchmarkPlainQueuePush(b *testing.B) {
	q := New("bench", 1<<20)
	defer q.Close()
	msg := protocol.NewMessage("bench", []byte(`"data"`), nil)

	b.ResetTimer()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		q.Push(msg)
		if i%10000 == 9999 {
			for j := 0; j < 10000; j++ {
				q.TryPop()
			}
		}
	}
}

func BenchmarkPlainQueuePop(b *testing.B) {
	q := New("bench", 1<<20)
	defer q.Close()
	for i := 0; i < 1<<20-1; i++ {
		q.Push(protocol.NewMessage("bench", []byte(`"data"`), nil))
	}

	b.ResetTimer()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		msg := q.TryPop()
		if msg == nil {
			for j := 0; j < 10000; j++ {
				q.Push(protocol.NewMessage("bench", []byte(fmt.Sprintf(`"%d"`, j)), nil))
			}
		}
	}
}
