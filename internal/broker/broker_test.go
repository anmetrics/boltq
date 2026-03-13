package broker

import (
	"sync"
	"testing"
	"time"

	"github.com/boltq/boltq/pkg/protocol"
)

func newTestBroker() *Broker {
	return New(Config{
		MaxRetry:   3,
		AckTimeout: 100 * time.Millisecond,
		QueueCap:   1 << 16,
	})
}

func TestPublishConsume(t *testing.T) {
	b := newTestBroker()
	defer b.Close()

	msg := protocol.NewMessage("test_queue", []byte(`"hello"`), nil)
	if err := b.Publish("test_queue", msg); err != nil {
		t.Fatal(err)
	}

	got := b.TryConsume("test_queue")
	if got == nil {
		t.Fatal("expected message")
	}
	if got.ID != msg.ID {
		t.Fatalf("expected id %s, got %s", msg.ID, got.ID)
	}
}

func TestAckNack(t *testing.T) {
	b := newTestBroker()
	defer b.Close()

	msg := protocol.NewMessage("test", []byte(`"data"`), nil)
	b.Publish("test", msg)

	got := b.TryConsume("test")
	if got == nil {
		t.Fatal("expected message")
	}

	// ACK.
	if err := b.Ack(got.ID); err != nil {
		t.Fatal(err)
	}

	// ACK again should fail.
	if err := b.Ack(got.ID); err == nil {
		t.Fatal("expected error for double ack")
	}
}

func TestNackRetry(t *testing.T) {
	b := newTestBroker()
	defer b.Close()

	msg := protocol.NewMessage("test", []byte(`"data"`), nil)
	b.Publish("test", msg)

	// Consume and NACK.
	got := b.TryConsume("test")
	if err := b.Nack(got.ID); err != nil {
		t.Fatal(err)
	}

	// Should be available again with retry incremented.
	retried := b.TryConsume("test")
	if retried == nil {
		t.Fatal("expected retried message")
	}
	if retried.Retry != 1 {
		t.Fatalf("expected retry 1, got %d", retried.Retry)
	}
}

func TestDeadLetter(t *testing.T) {
	b := New(Config{MaxRetry: 1, AckTimeout: time.Second, QueueCap: 1024})
	defer b.Close()

	msg := protocol.NewMessage("test", []byte(`"data"`), nil)
	b.Publish("test", msg)

	// Exhaust retries.
	got := b.TryConsume("test")
	b.Nack(got.ID)

	got = b.TryConsume("test")
	b.Nack(got.ID) // retry > maxRetry -> dead letter

	// Queue should be empty.
	if m := b.TryConsume("test"); m != nil {
		t.Fatal("expected empty queue")
	}

	// Dead letter should have the message.
	stats := b.Stats()
	if stats.DeadLetters["test_dead_letter"] != 1 {
		t.Fatalf("expected 1 dead letter, got %d", stats.DeadLetters["test_dead_letter"])
	}
}

func TestPubSub(t *testing.T) {
	b := newTestBroker()
	defer b.Close()

	ch1 := b.Subscribe("events", "sub1", 10)
	ch2 := b.Subscribe("events", "sub2", 10)

	msg := protocol.NewMessage("events", []byte(`"event_data"`), nil)
	b.PublishTopic("events", msg)

	select {
	case got := <-ch1:
		if got.ID != msg.ID {
			t.Fatal("sub1: wrong message")
		}
	case <-time.After(time.Second):
		t.Fatal("sub1: timeout")
	}

	select {
	case got := <-ch2:
		if got.ID != msg.ID {
			t.Fatal("sub2: wrong message")
		}
	case <-time.After(time.Second):
		t.Fatal("sub2: timeout")
	}
}

func TestStats(t *testing.T) {
	b := newTestBroker()
	defer b.Close()

	b.Publish("q1", protocol.NewMessage("q1", []byte(`"a"`), nil))
	b.Publish("q1", protocol.NewMessage("q1", []byte(`"b"`), nil))
	b.Subscribe("t1", "s1", 10)

	stats := b.Stats()
	if stats.Queues["q1"] != 2 {
		t.Fatalf("expected q1 size 2, got %d", stats.Queues["q1"])
	}
	if stats.Topics["t1"] != 1 {
		t.Fatalf("expected t1 subs 1, got %d", stats.Topics["t1"])
	}
}

// --- Benchmarks ---

func BenchmarkPublish(b *testing.B) {
	br := newTestBroker()
	defer br.Close()

	b.ResetTimer()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		msg := protocol.NewMessage("bench", []byte(`"data"`), nil)
		br.Publish("bench", msg)
		// Drain to avoid full.
		if i%10000 == 9999 {
			for j := 0; j < 10000; j++ {
				br.TryConsume("bench")
			}
		}
	}
}

func BenchmarkConsume(b *testing.B) {
	br := newTestBroker()
	defer br.Close()

	// Pre-fill.
	for i := 0; i < 1<<16-1; i++ {
		br.Publish("bench", protocol.NewMessage("bench", []byte(`"data"`), nil))
	}

	b.ResetTimer()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		msg := br.TryConsume("bench")
		if msg == nil {
			for j := 0; j < 1<<15; j++ {
				br.Publish("bench", protocol.NewMessage("bench", []byte(`"data"`), nil))
			}
		}
	}
}

func BenchmarkPublishParallel(b *testing.B) {
	br := newTestBroker()
	defer br.Close()

	b.ResetTimer()
	b.ReportAllocs()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			msg := protocol.NewMessage("bench", []byte(`"data"`), nil)
			br.Publish("bench", msg)
		}
	})
}

func BenchmarkEndToEnd(b *testing.B) {
	br := newTestBroker()
	defer br.Close()

	b.ResetTimer()
	b.ReportAllocs()

	var wg sync.WaitGroup
	wg.Add(2)

	// Producer.
	go func() {
		defer wg.Done()
		for i := 0; i < b.N; i++ {
			br.Publish("bench", protocol.NewMessage("bench", []byte(`"data"`), nil))
		}
	}()

	// Consumer.
	go func() {
		defer wg.Done()
		consumed := 0
		for consumed < b.N {
			msg := br.TryConsume("bench")
			if msg != nil {
				br.Ack(msg.ID)
				consumed++
			}
		}
	}()

	wg.Wait()
}
