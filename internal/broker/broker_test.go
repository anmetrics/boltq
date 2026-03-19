package broker

import (
	"fmt"
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

	ch1 := b.Subscribe("events", "sub1", 10, false)
	ch2 := b.Subscribe("events", "sub2", 10, false)

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
	b.Subscribe("t1", "s1", 10, false)

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

// ---------------------------------------------------------------------------
// Priority Queue Tests
// ---------------------------------------------------------------------------

func TestPriorityPublishConsume(t *testing.T) {
	b := newTestBroker()
	defer b.Close()

	// Publish messages with priorities 0, 5, 9, 3 (out of order).
	prios := []int{0, 5, 9, 3}
	for _, p := range prios {
		msg := protocol.NewMessage("pq", []byte(`"p"`), nil)
		msg.Priority = p
		if err := b.Publish("pq", msg); err != nil {
			t.Fatalf("publish priority %d: %v", p, err)
		}
	}

	// Consume should return highest priority first: 9, 5, 3, 0.
	expected := []int{9, 5, 3, 0}
	for i, want := range expected {
		got := b.TryConsume("pq")
		if got == nil {
			t.Fatalf("consume %d: expected message with priority %d, got nil", i, want)
		}
		if got.Priority != want {
			t.Fatalf("consume %d: expected priority %d, got %d", i, want, got.Priority)
		}
		b.Ack(got.ID)
	}

	// Queue should be empty now.
	if m := b.TryConsume("pq"); m != nil {
		t.Fatal("expected empty queue after consuming all")
	}
}

func TestPriorityDefaultZero(t *testing.T) {
	b := newTestBroker()
	defer b.Close()

	msg := protocol.NewMessage("pq_default", []byte(`"data"`), nil)
	// Do not set Priority explicitly; it should default to 0.
	if err := b.Publish("pq_default", msg); err != nil {
		t.Fatal(err)
	}

	got := b.TryConsume("pq_default")
	if got == nil {
		t.Fatal("expected message")
	}
	if got.Priority != 0 {
		t.Fatalf("expected default priority 0, got %d", got.Priority)
	}
}

func TestPriorityFIFOWithinLevel(t *testing.T) {
	b := newTestBroker()
	defer b.Close()

	// Publish 5 messages all at priority 5 with distinct payloads.
	ids := make([]string, 5)
	for i := 0; i < 5; i++ {
		msg := protocol.NewMessage("pq_fifo", []byte(fmt.Sprintf(`"msg_%d"`, i)), nil)
		msg.Priority = 5
		b.Publish("pq_fifo", msg)
		ids[i] = msg.ID
	}

	// Consume order should match publish order (FIFO within same priority).
	for i := 0; i < 5; i++ {
		got := b.TryConsume("pq_fifo")
		if got == nil {
			t.Fatalf("consume %d: expected message, got nil", i)
		}
		if got.ID != ids[i] {
			t.Fatalf("consume %d: expected id %s, got %s (FIFO violated)", i, ids[i], got.ID)
		}
		b.Ack(got.ID)
	}
}

func TestPriorityWithRetry(t *testing.T) {
	b := newTestBroker()
	defer b.Close()

	// Publish a high-priority message and a low-priority message.
	highMsg := protocol.NewMessage("pq_retry", []byte(`"high"`), nil)
	highMsg.Priority = 9
	b.Publish("pq_retry", highMsg)

	lowMsg := protocol.NewMessage("pq_retry", []byte(`"low"`), nil)
	lowMsg.Priority = 1
	b.Publish("pq_retry", lowMsg)

	// Consume the high-priority message and NACK it.
	got := b.TryConsume("pq_retry")
	if got == nil || got.Priority != 9 {
		t.Fatalf("expected high-priority message first, got %+v", got)
	}
	b.Nack(got.ID)

	// After NACK, the message is re-queued. It should still be consumed before
	// the low-priority message because its priority is preserved.
	got = b.TryConsume("pq_retry")
	if got == nil {
		t.Fatal("expected re-queued message")
	}
	if got.Priority != 9 {
		t.Fatalf("expected re-queued message to retain priority 9, got %d", got.Priority)
	}
	if got.Retry != 1 {
		t.Fatalf("expected retry count 1, got %d", got.Retry)
	}
	b.Ack(got.ID)

	// Now the low-priority message should come.
	got = b.TryConsume("pq_retry")
	if got == nil || got.Priority != 1 {
		t.Fatalf("expected low-priority message, got %+v", got)
	}
	b.Ack(got.ID)
}

func TestPriorityDeadLetter(t *testing.T) {
	b := New(Config{MaxRetry: 1, AckTimeout: time.Second, QueueCap: 1024})
	defer b.Close()

	msg := protocol.NewMessage("pq_dl", []byte(`"die"`), nil)
	msg.Priority = 7
	b.Publish("pq_dl", msg)

	// Exhaust retries (MaxRetry=1 means original delivery + 1 retry).
	got := b.TryConsume("pq_dl")
	b.Nack(got.ID) // retry 1

	got = b.TryConsume("pq_dl")
	b.Nack(got.ID) // retry 2 > maxRetry -> dead letter

	// Queue should be empty.
	if m := b.TryConsume("pq_dl"); m != nil {
		t.Fatal("expected empty queue after dead-lettering")
	}

	stats := b.Stats()
	if stats.DeadLetters["pq_dl_dead_letter"] != 1 {
		t.Fatalf("expected 1 dead letter, got %d", stats.DeadLetters["pq_dl_dead_letter"])
	}
}

// ---------------------------------------------------------------------------
// Publisher Confirm Tests
// ---------------------------------------------------------------------------

func TestPublishConfirmBasic(t *testing.T) {
	b := newTestBroker()
	defer b.Close()

	msg := protocol.NewMessage("confirm_q", []byte(`"confirmed"`), nil)
	if err := b.PublishConfirm("confirm_q", msg); err != nil {
		t.Fatalf("PublishConfirm: %v", err)
	}

	// Message should be consumable.
	got := b.TryConsume("confirm_q")
	if got == nil {
		t.Fatal("expected message after PublishConfirm")
	}
	if got.ID != msg.ID {
		t.Fatalf("expected id %s, got %s", msg.ID, got.ID)
	}
}

func TestPublishConfirmNoStorage(t *testing.T) {
	// Broker without storage (memory mode) -- PublishConfirm should still succeed.
	b := New(Config{MaxRetry: 3, AckTimeout: 100 * time.Millisecond, QueueCap: 1024})
	defer b.Close()

	if b.StorageMode() != "memory" {
		t.Skip("broker has storage configured, skipping memory-mode test")
	}

	msg := protocol.NewMessage("confirm_mem", []byte(`"mem"`), nil)
	if err := b.PublishConfirm("confirm_mem", msg); err != nil {
		t.Fatalf("PublishConfirm in memory mode: %v", err)
	}

	got := b.TryConsume("confirm_mem")
	if got == nil {
		t.Fatal("expected message after PublishConfirm in memory mode")
	}
	if got.ID != msg.ID {
		t.Fatalf("expected id %s, got %s", msg.ID, got.ID)
	}
}

// ---------------------------------------------------------------------------
// Exchange Tests
// ---------------------------------------------------------------------------

func TestExchangeDeclareDirect(t *testing.T) {
	b := newTestBroker()
	defer b.Close()

	if err := b.ExchangeDeclare("ex.direct", ExchangeDirect, false); err != nil {
		t.Fatalf("declare direct exchange: %v", err)
	}
}

func TestExchangeDeclareFanout(t *testing.T) {
	b := newTestBroker()
	defer b.Close()

	if err := b.ExchangeDeclare("ex.fanout", ExchangeFanout, false); err != nil {
		t.Fatalf("declare fanout exchange: %v", err)
	}
}

func TestExchangeDeclareTopic(t *testing.T) {
	b := newTestBroker()
	defer b.Close()

	if err := b.ExchangeDeclare("ex.topic", ExchangeTopic, false); err != nil {
		t.Fatalf("declare topic exchange: %v", err)
	}
}

func TestExchangeDeclareHeaders(t *testing.T) {
	b := newTestBroker()
	defer b.Close()

	if err := b.ExchangeDeclare("ex.headers", ExchangeHeaders, false); err != nil {
		t.Fatalf("declare headers exchange: %v", err)
	}
}

func TestExchangeDeclareIdempotent(t *testing.T) {
	b := newTestBroker()
	defer b.Close()

	if err := b.ExchangeDeclare("ex.idem", ExchangeDirect, true); err != nil {
		t.Fatal(err)
	}
	// Second declaration with same name and type should succeed.
	if err := b.ExchangeDeclare("ex.idem", ExchangeDirect, true); err != nil {
		t.Fatalf("idempotent re-declare failed: %v", err)
	}
}

func TestExchangeDeclareConflict(t *testing.T) {
	b := newTestBroker()
	defer b.Close()

	if err := b.ExchangeDeclare("ex.conflict", ExchangeDirect, false); err != nil {
		t.Fatal(err)
	}
	// Declaring with a different type should return an error.
	if err := b.ExchangeDeclare("ex.conflict", ExchangeFanout, false); err == nil {
		t.Fatal("expected error when redeclaring exchange with different type")
	}
}

func TestExchangeDelete(t *testing.T) {
	b := newTestBroker()
	defer b.Close()

	b.ExchangeDeclare("ex.del", ExchangeDirect, false)

	if err := b.ExchangeDelete("ex.del"); err != nil {
		t.Fatalf("delete exchange: %v", err)
	}

	// Publishing to deleted exchange should fail.
	msg := protocol.NewMessage("q", []byte(`"x"`), nil)
	if err := b.PublishExchange("ex.del", "key", msg); err == nil {
		t.Fatal("expected error when publishing to deleted exchange")
	}
}

func TestExchangeDeleteDefault(t *testing.T) {
	b := newTestBroker()
	defer b.Close()

	if err := b.ExchangeDelete(""); err == nil {
		t.Fatal("expected error when deleting the default exchange")
	}
}

func TestDirectExchangeRouting(t *testing.T) {
	b := newTestBroker()
	defer b.Close()

	b.ExchangeDeclare("dx", ExchangeDirect, false)
	b.BindQueue("dx", "orders", "order.created", nil, false)
	b.BindQueue("dx", "invoices", "invoice.created", nil, false)

	// Publish with routing key "order.created" -- only "orders" queue should receive.
	msg := protocol.NewMessage("", []byte(`"order1"`), nil)
	if err := b.PublishExchange("dx", "order.created", msg); err != nil {
		t.Fatal(err)
	}

	got := b.TryConsume("orders")
	if got == nil {
		t.Fatal("expected message in 'orders' queue")
	}
	b.Ack(got.ID)

	if m := b.TryConsume("invoices"); m != nil {
		t.Fatal("'invoices' queue should be empty for routing key 'order.created'")
	}

	// Publish with routing key "invoice.created" -- only "invoices" should receive.
	msg2 := protocol.NewMessage("", []byte(`"inv1"`), nil)
	b.PublishExchange("dx", "invoice.created", msg2)

	got = b.TryConsume("invoices")
	if got == nil {
		t.Fatal("expected message in 'invoices' queue")
	}
	b.Ack(got.ID)

	if m := b.TryConsume("orders"); m != nil {
		t.Fatal("'orders' queue should be empty for routing key 'invoice.created'")
	}
}

func TestFanoutExchangeRouting(t *testing.T) {
	b := newTestBroker()
	defer b.Close()

	b.ExchangeDeclare("fx", ExchangeFanout, false)
	b.BindQueue("fx", "q1", "", nil, false)
	b.BindQueue("fx", "q2", "", nil, false)
	b.BindQueue("fx", "q3", "", nil, false)

	msg := protocol.NewMessage("", []byte(`"broadcast"`), nil)
	if err := b.PublishExchange("fx", "ignored", msg); err != nil {
		t.Fatal(err)
	}

	// All 3 queues should have received the message.
	for _, qName := range []string{"q1", "q2", "q3"} {
		got := b.TryConsume(qName)
		if got == nil {
			t.Fatalf("expected message in queue %s (fanout)", qName)
		}
		b.Ack(got.ID)
	}
}

func TestTopicExchangeRouting(t *testing.T) {
	b := newTestBroker()
	defer b.Close()

	b.ExchangeDeclare("tx", ExchangeTopic, false)
	b.BindQueue("tx", "all_orders", "order.#", nil, false)
	b.BindQueue("tx", "created_events", "*.created", nil, false)
	b.BindQueue("tx", "vn_orders", "order.*.vn", nil, false)

	t.Run("order.created matches all_orders and created_events", func(t *testing.T) {
		msg := protocol.NewMessage("", []byte(`"oc"`), nil)
		b.PublishExchange("tx", "order.created", msg)

		if m := b.TryConsume("all_orders"); m == nil {
			t.Fatal("expected message in all_orders")
		} else {
			b.Ack(m.ID)
		}
		if m := b.TryConsume("created_events"); m == nil {
			t.Fatal("expected message in created_events")
		} else {
			b.Ack(m.ID)
		}
		if m := b.TryConsume("vn_orders"); m != nil {
			t.Fatal("vn_orders should not match order.created")
		}
	})

	t.Run("order.shipped.vn matches all_orders and vn_orders", func(t *testing.T) {
		msg := protocol.NewMessage("", []byte(`"osv"`), nil)
		b.PublishExchange("tx", "order.shipped.vn", msg)

		if m := b.TryConsume("all_orders"); m == nil {
			t.Fatal("expected message in all_orders")
		} else {
			b.Ack(m.ID)
		}
		if m := b.TryConsume("vn_orders"); m == nil {
			t.Fatal("expected message in vn_orders")
		} else {
			b.Ack(m.ID)
		}
		if m := b.TryConsume("created_events"); m != nil {
			t.Fatal("created_events should not match order.shipped.vn")
		}
	})

	t.Run("user.created matches only created_events", func(t *testing.T) {
		msg := protocol.NewMessage("", []byte(`"uc"`), nil)
		b.PublishExchange("tx", "user.created", msg)

		if m := b.TryConsume("created_events"); m == nil {
			t.Fatal("expected message in created_events")
		} else {
			b.Ack(m.ID)
		}
		if m := b.TryConsume("all_orders"); m != nil {
			t.Fatal("all_orders should not match user.created")
		}
		if m := b.TryConsume("vn_orders"); m != nil {
			t.Fatal("vn_orders should not match user.created")
		}
	})
}

func TestHeadersExchangeRouting(t *testing.T) {
	b := newTestBroker()
	defer b.Close()

	b.ExchangeDeclare("hx", ExchangeHeaders, false)

	// Bind q_all: requires both format=pdf AND type=report (match-all).
	b.BindQueue("hx", "q_all", "", map[string]string{"format": "pdf", "type": "report"}, true)
	// Bind q_any: requires format=csv OR source=api (match-any).
	b.BindQueue("hx", "q_any", "", map[string]string{"format": "csv", "source": "api"}, false)

	t.Run("match-all both headers present", func(t *testing.T) {
		msg := protocol.NewMessage("", []byte(`"both"`), map[string]string{"format": "pdf", "type": "report"})
		b.PublishExchange("hx", "", msg)

		if m := b.TryConsume("q_all"); m == nil {
			t.Fatal("expected message in q_all")
		} else {
			b.Ack(m.ID)
		}
		if m := b.TryConsume("q_any"); m != nil {
			t.Fatal("q_any should not match (no csv or api header)")
		}
	})

	t.Run("match-all partial headers - no match", func(t *testing.T) {
		msg := protocol.NewMessage("", []byte(`"partial"`), map[string]string{"format": "pdf"})
		b.PublishExchange("hx", "", msg)

		if m := b.TryConsume("q_all"); m != nil {
			t.Fatal("q_all should not match with only one header")
		}
	})

	t.Run("match-any one header matches", func(t *testing.T) {
		msg := protocol.NewMessage("", []byte(`"any"`), map[string]string{"source": "api", "format": "xml"})
		b.PublishExchange("hx", "", msg)

		if m := b.TryConsume("q_any"); m == nil {
			t.Fatal("expected message in q_any (source=api matches)")
		} else {
			b.Ack(m.ID)
		}
	})

	t.Run("match-any no header matches", func(t *testing.T) {
		msg := protocol.NewMessage("", []byte(`"none"`), map[string]string{"format": "pdf", "source": "web"})
		b.PublishExchange("hx", "", msg)

		if m := b.TryConsume("q_any"); m != nil {
			t.Fatal("q_any should not match (no csv or api)")
		}
	})
}

func TestDefaultExchangeRouting(t *testing.T) {
	b := newTestBroker()
	defer b.Close()

	// The default exchange ("") routes by routing key as queue name.
	msg := protocol.NewMessage("", []byte(`"default_route"`), nil)
	if err := b.PublishExchange("", "my_queue", msg); err != nil {
		t.Fatalf("publish to default exchange: %v", err)
	}

	got := b.TryConsume("my_queue")
	if got == nil {
		t.Fatal("expected message in 'my_queue' via default exchange")
	}
	if got.ID != msg.ID {
		t.Fatalf("expected id %s, got %s", msg.ID, got.ID)
	}
}

func TestExchangeNoMatchingBinding(t *testing.T) {
	b := newTestBroker()
	defer b.Close()

	b.ExchangeDeclare("nomatch", ExchangeDirect, false)
	b.BindQueue("nomatch", "q1", "key.a", nil, false)

	// Publish with a routing key that does not match any binding.
	msg := protocol.NewMessage("", []byte(`"lost"`), nil)
	err := b.PublishExchange("nomatch", "key.b", msg)
	if err != nil {
		t.Fatalf("expected nil error for unmatched routing (message dropped), got: %v", err)
	}

	// q1 should have nothing.
	if m := b.TryConsume("q1"); m != nil {
		t.Fatal("expected q1 to be empty when routing key does not match")
	}
}

func TestExchangeUnbind(t *testing.T) {
	b := newTestBroker()
	defer b.Close()

	b.ExchangeDeclare("unbind_ex", ExchangeDirect, false)
	b.BindQueue("unbind_ex", "uq1", "events", nil, false)
	b.BindQueue("unbind_ex", "uq2", "events", nil, false)

	// Both queues receive a message.
	msg1 := protocol.NewMessage("", []byte(`"before_unbind"`), nil)
	b.PublishExchange("unbind_ex", "events", msg1)

	if m := b.TryConsume("uq1"); m == nil {
		t.Fatal("uq1 should have received message before unbind")
	} else {
		b.Ack(m.ID)
	}
	if m := b.TryConsume("uq2"); m == nil {
		t.Fatal("uq2 should have received message before unbind")
	} else {
		b.Ack(m.ID)
	}

	// Unbind uq1.
	if err := b.UnbindQueue("unbind_ex", "uq1", "events"); err != nil {
		t.Fatalf("unbind: %v", err)
	}

	// Now only uq2 should receive messages.
	msg2 := protocol.NewMessage("", []byte(`"after_unbind"`), nil)
	b.PublishExchange("unbind_ex", "events", msg2)

	if m := b.TryConsume("uq1"); m != nil {
		t.Fatal("uq1 should NOT receive messages after unbind")
	}
	if m := b.TryConsume("uq2"); m == nil {
		t.Fatal("uq2 should still receive messages after unbinding uq1")
	} else {
		b.Ack(m.ID)
	}
}

func TestExchangeSnapshotRestore(t *testing.T) {
	b := newTestBroker()
	defer b.Close()

	// Set up exchanges and bindings.
	b.ExchangeDeclare("snap_direct", ExchangeDirect, true)
	b.ExchangeDeclare("snap_fanout", ExchangeFanout, false)
	b.BindQueue("snap_direct", "sq1", "key.one", nil, false)
	b.BindQueue("snap_direct", "sq2", "key.two", nil, false)
	b.BindQueue("snap_fanout", "sq3", "", nil, false)

	// Publish a message to verify queues have content.
	msg := protocol.NewMessage("", []byte(`"snap"`), nil)
	b.PublishExchange("snap_direct", "key.one", msg)

	// Snapshot.
	state := b.SnapshotFullState()

	// Create a fresh broker and restore.
	b2 := newTestBroker()
	defer b2.Close()
	b2.RestoreFullState(state)

	// Verify exchanges exist by publishing through them.
	// snap_direct with key.one should route to sq1.
	msg2 := protocol.NewMessage("", []byte(`"post_restore"`), nil)
	if err := b2.PublishExchange("snap_direct", "key.one", msg2); err != nil {
		t.Fatalf("publish to restored direct exchange: %v", err)
	}

	// sq1 should have the original message from snapshot + the new one.
	count := 0
	for {
		m := b2.TryConsume("sq1")
		if m == nil {
			break
		}
		b2.Ack(m.ID)
		count++
	}
	if count < 1 {
		t.Fatalf("expected at least 1 message in sq1 after restore, got %d", count)
	}

	// snap_fanout should still work.
	msg3 := protocol.NewMessage("", []byte(`"fan_restore"`), nil)
	if err := b2.PublishExchange("snap_fanout", "", msg3); err != nil {
		t.Fatalf("publish to restored fanout exchange: %v", err)
	}
	if m := b2.TryConsume("sq3"); m == nil {
		t.Fatal("expected message in sq3 via restored fanout exchange")
	} else {
		b2.Ack(m.ID)
	}

	// key.two binding should be preserved.
	msg4 := protocol.NewMessage("", []byte(`"key_two"`), nil)
	b2.PublishExchange("snap_direct", "key.two", msg4)
	if m := b2.TryConsume("sq2"); m == nil {
		t.Fatal("expected message in sq2 via restored direct exchange with key.two")
	} else {
		b2.Ack(m.ID)
	}
}
