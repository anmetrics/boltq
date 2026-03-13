package queue

import (
	"fmt"
	"sync"
	"testing"

	"github.com/boltq/boltq/pkg/protocol"
)

func TestQueuePushPop(t *testing.T) {
	q := New("test", 1024)

	msg := protocol.NewMessage("test", []byte("hello"), nil)
	if !q.Push(msg) {
		t.Fatal("push should succeed")
	}

	if q.Size() != 1 {
		t.Fatalf("expected size 1, got %d", q.Size())
	}

	got := q.TryPop()
	if got == nil {
		t.Fatal("pop should return a message")
	}
	if got.ID != msg.ID {
		t.Fatalf("expected id %s, got %s", msg.ID, got.ID)
	}

	if q.Size() != 0 {
		t.Fatalf("expected size 0, got %d", q.Size())
	}
}

func TestQueueFull(t *testing.T) {
	q := New("test", 4)

	for i := 0; i < 4; i++ {
		if !q.Push(protocol.NewMessage("test", []byte("msg"), nil)) {
			t.Fatalf("push %d should succeed", i)
		}
	}

	if q.Push(protocol.NewMessage("test", []byte("overflow"), nil)) {
		t.Fatal("push should fail when full")
	}
}

func TestQueueConcurrent(t *testing.T) {
	q := New("test", 1<<16)
	count := 10000
	var wg sync.WaitGroup

	// Concurrent producers.
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < count/4; j++ {
				q.Push(protocol.NewMessage("test", []byte("data"), nil))
			}
		}()
	}

	wg.Wait()

	if q.Size() != int64(count) {
		t.Fatalf("expected size %d, got %d", count, q.Size())
	}

	// Concurrent consumers.
	consumed := int64(0)
	var mu sync.Mutex
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			localCount := 0
			for {
				msg := q.TryPop()
				if msg == nil {
					break
				}
				localCount++
			}
			mu.Lock()
			consumed += int64(localCount)
			mu.Unlock()
		}()
	}

	wg.Wait()

	if consumed != int64(count) {
		t.Fatalf("expected consumed %d, got %d", count, consumed)
	}
}

func TestQueueClose(t *testing.T) {
	q := New("test", 1024)
	done := make(chan struct{})

	go func() {
		msg := q.Pop() // blocking
		if msg != nil {
			t.Error("expected nil after close")
		}
		close(done)
	}()

	q.Close()
	<-done
}

// --- Benchmarks ---

func BenchmarkQueuePush(b *testing.B) {
	q := New("bench", 1<<20)
	msg := protocol.NewMessage("bench", []byte("benchmark payload data"), nil)

	b.ResetTimer()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		q.Push(msg)
		// Drain periodically to avoid full.
		if i%1000 == 999 {
			for j := 0; j < 1000; j++ {
				q.TryPop()
			}
		}
	}
}

func BenchmarkQueuePop(b *testing.B) {
	q := New("bench", 1<<20)
	msg := protocol.NewMessage("bench", []byte("benchmark payload data"), nil)

	// Pre-fill.
	for i := 0; i < 1<<20-1; i++ {
		q.Push(msg)
	}

	b.ResetTimer()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		m := q.TryPop()
		if m == nil {
			// Refill.
			for j := 0; j < 1<<19; j++ {
				q.Push(msg)
			}
		}
	}
}

func BenchmarkQueuePushPopParallel(b *testing.B) {
	q := New("bench", 1<<20)

	b.ResetTimer()
	b.ReportAllocs()
	b.RunParallel(func(pb *testing.PB) {
		msg := protocol.NewMessage("bench", []byte("benchmark payload data"), nil)
		i := 0
		for pb.Next() {
			if i%2 == 0 {
				q.Push(msg)
			} else {
				q.TryPop()
			}
			i++
		}
	})
}

func BenchmarkQueueThroughput(b *testing.B) {
	for _, workers := range []int{1, 4, 8, 16} {
		b.Run(fmt.Sprintf("workers-%d", workers), func(b *testing.B) {
			q := New("bench", 1<<20)
			var wg sync.WaitGroup
			perWorker := b.N / workers
			if perWorker == 0 {
				perWorker = 1
			}

			b.ResetTimer()
			b.ReportAllocs()

			// Producers.
			for w := 0; w < workers; w++ {
				wg.Add(1)
				go func() {
					defer wg.Done()
					msg := protocol.NewMessage("bench", []byte("payload"), nil)
					for i := 0; i < perWorker; i++ {
						q.Push(msg)
					}
				}()
			}

			// Consumers.
			for w := 0; w < workers; w++ {
				wg.Add(1)
				go func() {
					defer wg.Done()
					for i := 0; i < perWorker; i++ {
						q.TryPop()
					}
				}()
			}

			wg.Wait()
		})
	}
}
