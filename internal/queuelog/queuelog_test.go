package queuelog

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/boltq/boltq/internal/broker"
	"github.com/boltq/boltq/internal/stream"
)

func testLog(t *testing.T) (*stream.Log, *stream.CursorStore, string) {
	t.Helper()
	dir := t.TempDir()

	l, err := stream.OpenLog(stream.DefaultLogConfig(filepath.Join(dir, "log")))
	if err != nil {
		t.Fatalf("open log: %v", err)
	}
	c, err := stream.OpenCursorStore(filepath.Join(dir, "cursors"))
	if err != nil {
		t.Fatalf("open cursors: %v", err)
	}
	t.Cleanup(func() {
		c.Close()
		l.Close()
	})
	return l, c, dir
}

func rec(payload string) *stream.Record {
	return &stream.Record{Payload: []byte(payload)}
}

func mustPublish(t *testing.T, q *Queue, n int) {
	t.Helper()
	for i := 0; i < n; i++ {
		if _, err := q.Publish(context.Background(), rec(fmt.Sprintf("m%d", i))); err != nil {
			t.Fatalf("publish %d: %v", i, err)
		}
	}
}

func TestConsumeAckAdvancesWindow(t *testing.T) {
	l, c, _ := testLog(t)
	q, err := Open(l, c, "jobs", Config{Partitions: 1})
	if err != nil {
		t.Fatal(err)
	}
	defer q.Close()

	mustPublish(t, q, 3)

	ds, err := q.TryConsume("w1", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(ds) != 3 {
		t.Fatalf("got %d deliveries, want 3", len(ds))
	}
	for _, d := range ds {
		if d.Count != 1 {
			t.Errorf("seq %d: delivery count %d, want 1", d.Seq, d.Count)
		}
	}

	// Nothing else is available while the records are leased.
	if again, _ := q.TryConsume("w2", 10); len(again) != 0 {
		t.Fatalf("leased records were handed out twice: %d", len(again))
	}

	if err := q.AckAll("w1", ds); err != nil {
		t.Fatalf("ack: %v", err)
	}
	if got := q.Stats().Backlog; got != 0 {
		t.Errorf("backlog %d after acking everything, want 0", got)
	}
	if got := q.parts[0].base; got != 4 {
		t.Errorf("window base %d, want 4", got)
	}
}

func TestCompetingConsumersNeverOverlap(t *testing.T) {
	l, c, _ := testLog(t)
	q, err := Open(l, c, "jobs", Config{Partitions: 4})
	if err != nil {
		t.Fatal(err)
	}
	defer q.Close()

	const total = 400
	for i := 0; i < total; i++ {
		r := rec(fmt.Sprintf("m%d", i))
		r.Key = []byte(fmt.Sprintf("k%d", i)) // spread across partitions
		if _, err := q.Publish(context.Background(), r); err != nil {
			t.Fatal(err)
		}
	}

	var (
		mu   sync.Mutex
		seen = make(map[string]int)
		wg   sync.WaitGroup
	)
	for w := 0; w < 8; w++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			name := fmt.Sprintf("w%d", id)
			for {
				ds, err := q.TryConsume(name, 5)
				if err != nil || len(ds) == 0 {
					return
				}
				mu.Lock()
				for _, d := range ds {
					seen[fmt.Sprintf("%d:%d", d.Partition, d.Seq)]++
				}
				mu.Unlock()
				if err := q.AckAll(name, ds); err != nil {
					t.Errorf("ack: %v", err)
					return
				}
			}
		}(w)
	}
	wg.Wait()

	if len(seen) != total {
		t.Fatalf("delivered %d distinct records, want %d", len(seen), total)
	}
	for k, n := range seen {
		if n != 1 {
			t.Fatalf("record %s delivered %d times", k, n)
		}
	}
}

func TestNackRequeuesAndCountsDeliveries(t *testing.T) {
	l, c, _ := testLog(t)
	q, err := Open(l, c, "jobs", Config{Partitions: 1, MaxDelivery: 3})
	if err != nil {
		t.Fatal(err)
	}
	defer q.Close()

	mustPublish(t, q, 1)

	for want := 1; want <= 3; want++ {
		ds, err := q.TryConsume("w1", 1)
		if err != nil {
			t.Fatal(err)
		}
		if len(ds) != 1 {
			t.Fatalf("attempt %d: got %d deliveries, want 1", want, len(ds))
		}
		if ds[0].Count != want {
			t.Errorf("attempt %d: delivery count %d", want, ds[0].Count)
		}
		if err := q.Nack("w1", ds[0].Partition, ds[0].Seq, true); err != nil {
			t.Fatalf("nack: %v", err)
		}
	}

	// The third nack hit MaxDelivery. With no dead-letter route declared the
	// record is discarded and the window advances — keeping it would pin the
	// base and stall every record behind it. Declaring a dead-letter exchange is
	// how an operator opts out of that, which is covered by
	// TestRouterDeadLettersThroughExchange.
	if ds, _ := q.TryConsume("w1", 1); len(ds) != 0 {
		t.Fatalf("record survived MaxDelivery with no dead-letter route")
	}
	if got := q.parts[0].base; got != 2 {
		t.Errorf("window base %d after discarding the poison record, want 2", got)
	}
	if got := q.Stats().Partitions[0].DeadLettered; got != 1 {
		t.Errorf("dead-lettered count %d, want 1", got)
	}
}

func TestLeaseExpiryRedelivers(t *testing.T) {
	l, c, _ := testLog(t)
	q, err := Open(l, c, "jobs", Config{
		Partitions: 1, AckTimeout: 40 * time.Millisecond, MaxDelivery: 10,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer q.Close()

	mustPublish(t, q, 1)

	first, err := q.TryConsume("crashed", 1)
	if err != nil || len(first) != 1 {
		t.Fatalf("first consume: %v (%d)", err, len(first))
	}

	// A consumer that dies without acking must not hold the record forever.
	time.Sleep(80 * time.Millisecond)

	second, err := q.TryConsume("healthy", 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(second) != 1 {
		t.Fatalf("expired lease was not reclaimed")
	}
	if second[0].Count != 2 {
		t.Errorf("delivery count %d after redelivery, want 2", second[0].Count)
	}

	// The original holder must not be able to retire a record it no longer owns.
	if err := q.Ack("crashed", first[0].Partition, first[0].Seq); !errors.Is(err, ErrNotHolder) {
		t.Errorf("stale ack returned %v, want ErrNotHolder", err)
	}
	if err := q.Ack("healthy", second[0].Partition, second[0].Seq); err != nil {
		t.Errorf("ack by current holder: %v", err)
	}
}

func TestReleaseConsumerReturnsLeasesImmediately(t *testing.T) {
	l, c, _ := testLog(t)
	q, err := Open(l, c, "jobs", Config{Partitions: 2, AckTimeout: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	defer q.Close()

	mustPublish(t, q, 6)
	ds, err := q.TryConsume("w1", 6)
	if err != nil || len(ds) == 0 {
		t.Fatalf("consume: %v (%d)", err, len(ds))
	}

	if n := q.ReleaseConsumer("w1"); n != len(ds) {
		t.Fatalf("released %d leases, want %d", n, len(ds))
	}
	back, err := q.TryConsume("w2", 6)
	if err != nil {
		t.Fatal(err)
	}
	if len(back) != len(ds) {
		t.Fatalf("got %d records back, want %d", len(back), len(ds))
	}
}

func TestOutOfOrderAckHoldsWindowBase(t *testing.T) {
	l, c, _ := testLog(t)
	q, err := Open(l, c, "jobs", Config{Partitions: 1, AckTimeout: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	defer q.Close()

	mustPublish(t, q, 3)
	ds, err := q.TryConsume("w1", 3)
	if err != nil || len(ds) != 3 {
		t.Fatalf("consume: %v (%d)", err, len(ds))
	}

	// Ack the newest two. The base cannot move past the oldest un-acked record,
	// because a cursor is a single number and skipping it would lose it.
	if err := q.Ack("w1", ds[2].Partition, ds[2].Seq); err != nil {
		t.Fatal(err)
	}
	if err := q.Ack("w1", ds[1].Partition, ds[1].Seq); err != nil {
		t.Fatal(err)
	}
	if got := q.parts[0].base; got != 1 {
		t.Fatalf("base moved to %d with seq 1 still in flight", got)
	}

	if err := q.Ack("w1", ds[0].Partition, ds[0].Seq); err != nil {
		t.Fatal(err)
	}
	if got := q.parts[0].base; got != 4 {
		t.Fatalf("base %d after acking the whole run, want 4", got)
	}
}

func TestMaxInFlightBoundsTheWindow(t *testing.T) {
	l, c, _ := testLog(t)
	q, err := Open(l, c, "jobs", Config{
		Partitions: 1, MaxInFlight: 5, AckTimeout: time.Hour,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer q.Close()

	mustPublish(t, q, 50)

	ds, err := q.TryConsume("w1", 50)
	if err != nil {
		t.Fatal(err)
	}
	if len(ds) != 5 {
		t.Fatalf("leased %d records, want the window bound of 5", len(ds))
	}
	if err := q.AckAll("w1", ds); err != nil {
		t.Fatal(err)
	}
	next, err := q.TryConsume("w1", 50)
	if err != nil {
		t.Fatal(err)
	}
	if len(next) != 5 {
		t.Fatalf("window did not reopen after acks: got %d", len(next))
	}
}

func TestCursorSurvivesReopen(t *testing.T) {
	l, c, _ := testLog(t)

	q, err := Open(l, c, "jobs", Config{Partitions: 1})
	if err != nil {
		t.Fatal(err)
	}
	mustPublish(t, q, 5)

	ds, err := q.TryConsume("w1", 3)
	if err != nil || len(ds) != 3 {
		t.Fatalf("consume: %v (%d)", err, len(ds))
	}
	if err := q.AckAll("w1", ds); err != nil {
		t.Fatal(err)
	}
	q.Close()

	// A new queue over the same topic and group resumes above the committed
	// base: acknowledged work is never redelivered.
	q2, err := Open(l, c, "jobs", Config{Partitions: 1})
	if err != nil {
		t.Fatal(err)
	}
	defer q2.Close()

	again, err := q2.TryConsume("w1", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(again) != 2 {
		t.Fatalf("resumed with %d records, want the 2 that were never acked", len(again))
	}
	if again[0].Seq != 4 {
		t.Errorf("resumed at seq %d, want 4", again[0].Seq)
	}
}

func TestRouterDeadLettersThroughExchange(t *testing.T) {
	l, c, _ := testLog(t)
	r, err := NewRouter(l, c)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()

	if _, err := r.DeclareExchange("dlx", broker.ExchangeFanout, true); err != nil {
		t.Fatal(err)
	}
	dead, err := r.DeclareQueue(QueueSpec{Name: "failed", Partitions: 1})
	if err != nil {
		t.Fatal(err)
	}
	work, err := r.DeclareQueue(QueueSpec{
		Name: "work", Partitions: 1, MaxDelivery: 2, DeadLetterExchange: "dlx",
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := r.Bind("dlx", "failed", "", nil, false); err != nil {
		t.Fatal(err)
	}

	if _, err := r.Publish(context.Background(), "", "work", rec("poison")); err != nil {
		t.Fatalf("publish: %v", err)
	}

	for attempt := 1; attempt <= 2; attempt++ {
		ds, err := work.TryConsume("w1", 1)
		if err != nil {
			t.Fatal(err)
		}
		if len(ds) != 1 {
			t.Fatalf("attempt %d: got %d deliveries", attempt, len(ds))
		}
		if err := work.Nack("w1", ds[0].Partition, ds[0].Seq, true); err != nil {
			t.Fatal(err)
		}
	}

	if left, _ := work.TryConsume("w1", 1); len(left) != 0 {
		t.Fatalf("record still in the work queue after exhausting MaxDelivery")
	}

	ds, err := dead.TryConsume("dlq", 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(ds) != 1 {
		t.Fatalf("dead letter never arrived")
	}
	if string(ds[0].Record.Payload) != "poison" {
		t.Errorf("dead letter payload %q", ds[0].Record.Payload)
	}
	if got := ds[0].Record.Headers[HeaderDeathQueue]; got != "work" {
		t.Errorf("%s = %q, want \"work\"", HeaderDeathQueue, got)
	}
	if got := ds[0].Record.Headers[HeaderDeathCount]; got != "1" {
		t.Errorf("%s = %q, want \"1\"", HeaderDeathCount, got)
	}
}

func TestRouterRejectRoutesImmediately(t *testing.T) {
	l, c, _ := testLog(t)
	r, err := NewRouter(l, c)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()

	if _, err := r.DeclareExchange("dlx", broker.ExchangeFanout, true); err != nil {
		t.Fatal(err)
	}
	dead, _ := r.DeclareQueue(QueueSpec{Name: "failed", Partitions: 1})
	work, _ := r.DeclareQueue(QueueSpec{
		Name: "work", Partitions: 1, MaxDelivery: 100, DeadLetterExchange: "dlx",
	})
	if err := r.Bind("dlx", "failed", "", nil, false); err != nil {
		t.Fatal(err)
	}

	if _, err := r.Publish(context.Background(), "", "work", rec("bad")); err != nil {
		t.Fatal(err)
	}
	ds, err := work.TryConsume("w1", 1)
	if err != nil || len(ds) != 1 {
		t.Fatalf("consume: %v (%d)", err, len(ds))
	}

	// requeue=false must bypass MaxDelivery entirely.
	if err := work.Nack("w1", ds[0].Partition, ds[0].Seq, false); err != nil {
		t.Fatal(err)
	}
	if got, _ := dead.TryConsume("dlq", 1); len(got) != 1 {
		t.Fatalf("rejected record did not reach the dead-letter queue")
	}
}

func TestRouterTopicRoutingAndUnroutable(t *testing.T) {
	l, c, _ := testLog(t)
	r, err := NewRouter(l, c)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()

	if _, err := r.DeclareExchange("events", broker.ExchangeTopic, true); err != nil {
		t.Fatal(err)
	}
	orders, _ := r.DeclareQueue(QueueSpec{Name: "orders", Partitions: 1})
	audit, _ := r.DeclareQueue(QueueSpec{Name: "audit", Partitions: 1})

	if err := r.Bind("events", "orders", "order.*", nil, false); err != nil {
		t.Fatal(err)
	}
	if err := r.Bind("events", "audit", "#", nil, false); err != nil {
		t.Fatal(err)
	}

	res, err := r.Publish(context.Background(), "events", "order.created", rec("o1"))
	if err != nil {
		t.Fatal(err)
	}
	if len(res) != 2 {
		t.Fatalf("routed to %d queues, want 2", len(res))
	}
	if got, _ := orders.TryConsume("a", 1); len(got) != 1 {
		t.Error("orders queue did not receive the message")
	}
	if got, _ := audit.TryConsume("a", 1); len(got) != 1 {
		t.Error("audit queue did not receive the message")
	}

	// An unroutable publish must be reported, not silently dropped.
	_, err = r.Publish(context.Background(), "", "nowhere", rec("x"))
	if !errors.Is(err, ErrUnroutable) {
		t.Errorf("unroutable publish returned %v, want ErrUnroutable", err)
	}
}

func TestConsumeBlocksUntilPublished(t *testing.T) {
	l, c, _ := testLog(t)
	q, err := Open(l, c, "jobs", Config{Partitions: 2})
	if err != nil {
		t.Fatal(err)
	}
	defer q.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	done := make(chan []Delivery, 1)
	go func() {
		ds, err := q.Consume(ctx, "w1", 1)
		if err != nil {
			done <- nil
			return
		}
		done <- ds
	}()

	time.Sleep(50 * time.Millisecond)
	mustPublish(t, q, 1)

	select {
	case ds := <-done:
		if len(ds) != 1 {
			t.Fatalf("blocked consumer got %d records", len(ds))
		}
	case <-ctx.Done():
		t.Fatal("blocked consumer was never woken by the append")
	}
}

func TestConsumeRespectsCancellation(t *testing.T) {
	l, c, _ := testLog(t)
	q, err := Open(l, c, "jobs", Config{Partitions: 1})
	if err != nil {
		t.Fatal(err)
	}
	defer q.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()

	if _, err := q.Consume(ctx, "w1", 1); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Consume returned %v, want DeadlineExceeded", err)
	}
}
