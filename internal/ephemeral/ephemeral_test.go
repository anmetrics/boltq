package ephemeral

import (
	"fmt"
	"sync"
	"testing"
	"time"
)

func newHub(t *testing.T, cfg Config) *Hub {
	t.Helper()
	h := New(cfg)
	t.Cleanup(h.Close)
	return h
}

func recv(t *testing.T, sub *Subscription, timeout time.Duration) (Signal, bool) {
	t.Helper()
	select {
	case s, ok := <-sub.C:
		return s, ok
	case <-time.After(timeout):
		return Signal{}, false
	}
}

func TestPublishReachesSubscribers(t *testing.T) {
	h := newHub(t, Config{})

	sub, err := h.Subscribe("typing.conv-1", "bob")
	if err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	defer sub.Close()

	if err := h.Publish(Signal{Topic: "typing.conv-1", Sender: "alice", Kind: KindTyping}); err != nil {
		t.Fatalf("publish: %v", err)
	}

	sig, ok := recv(t, sub, time.Second)
	if !ok {
		t.Fatal("no signal received")
	}
	if sig.Sender != "alice" || sig.Kind != KindTyping {
		t.Errorf("unexpected signal: %+v", sig)
	}
	if sig.At == 0 {
		t.Error("timestamp not stamped")
	}
}

func TestSenderDoesNotReceiveOwnSignal(t *testing.T) {
	h := newHub(t, Config{})

	sub, _ := h.Subscribe("typing.conv-1", "alice")
	defer sub.Close()

	h.Publish(Signal{Topic: "typing.conv-1", Sender: "alice", Kind: KindTyping})

	if sig, ok := recv(t, sub, 200*time.Millisecond); ok {
		t.Errorf("sender received their own signal echoed back: %+v", sig)
	}
}

func TestPublishToEmptyTopicIsNotAnError(t *testing.T) {
	h := newHub(t, Config{})
	if err := h.Publish(Signal{Topic: "typing.nobody", Sender: "alice"}); err != nil {
		t.Errorf("publish to an unwatched topic failed: %v", err)
	}
}

func TestSlowSubscriberIsDroppedNotBlocking(t *testing.T) {
	h := newHub(t, Config{SubscriberBuffer: 4, RatePerSecond: 1e9, Burst: 1e9})

	slow, _ := h.Subscribe("typing.c", "slow") // never drained
	defer slow.Close()
	fast, _ := h.Subscribe("typing.c", "fast")
	defer fast.Close()

	go func() {
		for range fast.C {
		}
	}()

	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; i < 500; i++ {
			h.Publish(Signal{Topic: "typing.c", Sender: "alice", Kind: KindTyping})
		}
	}()

	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("a stalled subscriber blocked the publisher")
	}

	if slow.Dropped() == 0 {
		t.Error("expected drops for the subscriber that never read")
	}
	if s := h.Stats(); s.Dropped == 0 {
		t.Error("hub did not record any drops")
	}
}

func TestRateLimiting(t *testing.T) {
	h := newHub(t, Config{RatePerSecond: 10, Burst: 3})

	sub, _ := h.Subscribe("typing.c", "bob")
	defer sub.Close()

	allowed := 0
	limited := 0
	for i := 0; i < 20; i++ {
		err := h.Publish(Signal{Topic: "typing.c", Sender: "alice", Kind: KindTyping})
		switch err {
		case nil:
			allowed++
		case ErrRateLimited:
			limited++
		default:
			t.Fatalf("unexpected error: %v", err)
		}
	}

	if allowed > 5 {
		t.Errorf("%d signals allowed against a burst of 3 — limiter is too loose", allowed)
	}
	if limited == 0 {
		t.Error("no signals were rate limited")
	}
	if s := h.Stats(); s.RateLimited == 0 {
		t.Error("hub did not count rate-limited publishes")
	}
}

func TestRateLimitRefills(t *testing.T) {
	h := newHub(t, Config{RatePerSecond: 100, Burst: 2})

	for i := 0; i < 5; i++ {
		h.Publish(Signal{Topic: "typing.c", Sender: "alice"})
	}
	if err := h.Publish(Signal{Topic: "typing.c", Sender: "alice"}); err != ErrRateLimited {
		t.Fatalf("expected the bucket to be empty, got %v", err)
	}

	time.Sleep(60 * time.Millisecond) // ~6 tokens at 100/s
	if err := h.Publish(Signal{Topic: "typing.c", Sender: "alice"}); err != nil {
		t.Errorf("bucket did not refill: %v", err)
	}
}

func TestRateLimitIsPerPublisher(t *testing.T) {
	h := newHub(t, Config{RatePerSecond: 1, Burst: 2})

	for i := 0; i < 5; i++ {
		h.Publish(Signal{Topic: "typing.c", Sender: "spammer"})
	}
	// One noisy client must not consume everyone else's budget.
	if err := h.Publish(Signal{Topic: "typing.c", Sender: "innocent"}); err != nil {
		t.Errorf("an unrelated publisher was rate limited: %v", err)
	}
}

func TestPayloadSizeCap(t *testing.T) {
	h := newHub(t, Config{MaxPayload: 16})
	err := h.Publish(Signal{Topic: "typing.c", Sender: "alice", Payload: make([]byte, 64)})
	if err != ErrPayloadTooLarge {
		t.Errorf("oversized payload: got %v", err)
	}
}

func TestResubscribeReplacesOldSubscription(t *testing.T) {
	h := newHub(t, Config{})

	first, _ := h.Subscribe("typing.c", "bob")
	second, _ := h.Subscribe("typing.c", "bob")

	// The old channel must be closed, not left dangling with a goroutine
	// reading from it forever.
	if _, ok := recv(t, first, time.Second); ok {
		t.Error("the replaced subscription's channel is still open")
	}
	if h.SubscriberCount("typing.c") != 1 {
		t.Errorf("subscriber count = %d, want 1", h.SubscriberCount("typing.c"))
	}

	h.Publish(Signal{Topic: "typing.c", Sender: "alice"})
	if _, ok := recv(t, second, time.Second); !ok {
		t.Error("the replacement subscription received nothing")
	}
}

func TestCloseIsIdempotentAndSafeWithHubClose(t *testing.T) {
	h := New(Config{})
	sub, _ := h.Subscribe("typing.c", "bob")

	sub.Close()
	sub.Close() // must not panic on a double close

	h.Close()
	h.Close() // must not panic either
}

func TestHubCloseReleasesSubscribers(t *testing.T) {
	h := New(Config{})
	sub, _ := h.Subscribe("typing.c", "bob")

	h.Close()

	if _, ok := recv(t, sub, time.Second); ok {
		t.Error("subscriber channel still open after hub close")
	}
	if err := h.Publish(Signal{Topic: "typing.c", Sender: "alice"}); err != ErrClosed {
		t.Errorf("publish after close: got %v", err)
	}
	if _, err := h.Subscribe("typing.c", "carol"); err != ErrClosed {
		t.Errorf("subscribe after close: got %v", err)
	}
}

func TestUnsubscribeCleansUpTopic(t *testing.T) {
	h := newHub(t, Config{})

	sub, _ := h.Subscribe("typing.c", "bob")
	if len(h.TopicsWithPrefix("typing.")) != 1 {
		t.Fatal("topic not registered")
	}
	sub.Close()
	if n := len(h.TopicsWithPrefix("typing.")); n != 0 {
		t.Errorf("%d topics left after the last subscriber left", n)
	}
}

func TestPublishTyping(t *testing.T) {
	h := newHub(t, Config{})
	sub, _ := h.Subscribe(TypingTopic("conv-1"), "bob")
	defer sub.Close()

	h.PublishTyping("conv-1", "alice", true)
	sig, ok := recv(t, sub, time.Second)
	if !ok || sig.Kind != KindTyping {
		t.Fatalf("typing signal: %+v ok=%v", sig, ok)
	}

	h.PublishTyping("conv-1", "alice", false)
	sig, ok = recv(t, sub, time.Second)
	if !ok || sig.Kind != KindStopTyping {
		t.Errorf("stop-typing signal: %+v ok=%v", sig, ok)
	}
}

func TestLimiterGarbageCollection(t *testing.T) {
	h := newHub(t, Config{LimiterIdleTTL: 30 * time.Millisecond})

	for i := 0; i < 100; i++ {
		h.Publish(Signal{Topic: "typing.c", Sender: fmt.Sprintf("u%d", i)})
	}
	time.Sleep(50 * time.Millisecond)

	if removed := h.gcLimiters(); removed != 100 {
		t.Errorf("gc removed %d idle limiters, want 100", removed)
	}
}

func TestConcurrentPublishSubscribe(t *testing.T) {
	h := newHub(t, Config{RatePerSecond: 1e9, Burst: 1e9})

	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			topic := fmt.Sprintf("typing.c%d", i%5)
			sub, err := h.Subscribe(topic, fmt.Sprintf("s%d", i))
			if err != nil {
				return
			}
			go func() {
				for range sub.C {
				}
			}()
			for j := 0; j < 100; j++ {
				h.Publish(Signal{Topic: topic, Sender: fmt.Sprintf("u%d", i)})
			}
			sub.Close()
		}(i)
	}
	wg.Wait()
	h.Stats()
}

func BenchmarkPublish(b *testing.B) {
	h := New(Config{RatePerSecond: 1e9, Burst: 1e9, SubscriberBuffer: 1024})
	defer h.Close()

	for i := 0; i < 10; i++ {
		sub, _ := h.Subscribe("typing.c", fmt.Sprintf("s%d", i))
		go func() {
			for range sub.C {
			}
		}()
	}

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		h.Publish(Signal{Topic: "typing.c", Sender: "alice", Kind: KindTyping})
	}
}
