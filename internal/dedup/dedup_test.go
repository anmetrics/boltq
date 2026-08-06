package dedup

import (
	"fmt"
	"sync"
	"testing"
	"time"
)

func newTable(t *testing.T, cfg Config) *Table {
	t.Helper()
	if cfg.SweepInterval == 0 {
		cfg.SweepInterval = time.Hour // drive expiry explicitly
	}
	tbl := New(cfg)
	t.Cleanup(tbl.Close)
	return tbl
}

func TestFirstClaimSucceeds(t *testing.T) {
	tbl := newTable(t, Config{})
	k := Key{Sender: "alice", ClientMsgID: "m1"}

	_, ok, claimed := tbl.Claim(k)
	if !claimed {
		t.Fatal("first claim was refused")
	}
	if ok {
		t.Error("first claim reported a prior result")
	}
}

func TestRetryReturnsOriginalResult(t *testing.T) {
	tbl := newTable(t, Config{})
	k := Key{Sender: "alice", ClientMsgID: "m1"}

	_, _, claimed := tbl.Claim(k)
	if !claimed {
		t.Fatal("first claim refused")
	}
	want := Result{MessageID: "srv-1", Topic: "chat.direct.c1", Partition: 3, Seq: 42, Timestamp: 999}
	tbl.Complete(k, want)

	// This is the whole point: the phone's retry must resolve to the same
	// message, not create a second one.
	got, ok, claimed := tbl.Claim(k)
	if claimed {
		t.Fatal("a retry was allowed to claim and would have duplicated the message")
	}
	if !ok {
		t.Fatal("retry did not find the completed result")
	}
	if got != want {
		t.Errorf("retry got %+v, want %+v", got, want)
	}
}

func TestInFlightRetryIsDistinguishable(t *testing.T) {
	tbl := newTable(t, Config{})
	k := Key{Sender: "alice", ClientMsgID: "m1"}

	tbl.Claim(k) // claimed but not completed

	// A retry racing the original must be told "in flight", not handed a
	// zero-valued result and not allowed to duplicate.
	got, ok, claimed := tbl.Claim(k)
	if claimed {
		t.Error("in-flight key was re-claimed")
	}
	if ok {
		t.Errorf("in-flight key reported a result: %+v", got)
	}
}

func TestAbandonReleasesClaim(t *testing.T) {
	tbl := newTable(t, Config{})
	k := Key{Sender: "alice", ClientMsgID: "m1"}

	tbl.Claim(k)
	tbl.Abandon(k)

	// After a failed publish the retry must be treated as a fresh attempt,
	// otherwise the message is lost forever behind a dedup entry for a message
	// that was never stored.
	_, _, claimed := tbl.Claim(k)
	if !claimed {
		t.Error("retry after Abandon was not allowed to claim")
	}
}

func TestAbandonDoesNotDropCompletedEntry(t *testing.T) {
	tbl := newTable(t, Config{})
	k := Key{Sender: "alice", ClientMsgID: "m1"}

	tbl.Claim(k)
	tbl.Complete(k, Result{MessageID: "srv-1"})
	tbl.Abandon(k)

	if _, ok := tbl.Lookup(k); !ok {
		t.Error("Abandon erased a completed result")
	}
}

func TestSenderScoping(t *testing.T) {
	tbl := newTable(t, Config{})

	// Two users independently choosing the same client ID must not collide —
	// short client IDs and restarted counters make this likely, not exotic.
	alice := Key{Sender: "alice", ClientMsgID: "1"}
	bob := Key{Sender: "bob", ClientMsgID: "1"}

	tbl.Claim(alice)
	tbl.Complete(alice, Result{MessageID: "alice-msg"})

	_, ok, claimed := tbl.Claim(bob)
	if !claimed {
		t.Fatal("bob's message was swallowed by alice's dedup entry")
	}
	if ok {
		t.Error("bob's claim returned alice's result")
	}
}

func TestExpiry(t *testing.T) {
	tbl := newTable(t, Config{TTL: 50 * time.Millisecond})
	k := Key{Sender: "alice", ClientMsgID: "m1"}

	tbl.Claim(k)
	tbl.Complete(k, Result{MessageID: "srv-1"})

	if _, ok := tbl.Lookup(k); !ok {
		t.Fatal("result missing before expiry")
	}

	time.Sleep(80 * time.Millisecond)
	if _, ok := tbl.Lookup(k); ok {
		t.Error("result survived past its TTL")
	}
	if n := tbl.Sweep(); n != 1 {
		t.Errorf("sweep removed %d entries, want 1", n)
	}
	if tbl.Len() != 0 {
		t.Errorf("%d entries left after sweep", tbl.Len())
	}
}

func TestCompleteAfterSweepStillDeduplicates(t *testing.T) {
	tbl := newTable(t, Config{TTL: 30 * time.Millisecond})
	k := Key{Sender: "alice", ClientMsgID: "m1"}

	tbl.Claim(k)
	time.Sleep(50 * time.Millisecond)
	tbl.Sweep() // claim swept while the publish was still in flight

	tbl.Complete(k, Result{MessageID: "srv-1"})

	// A retry arriving now must still be deduplicated; the message did land.
	got, ok, claimed := tbl.Claim(k)
	if claimed {
		t.Error("retry duplicated a message whose claim had been swept")
	}
	if !ok || got.MessageID != "srv-1" {
		t.Errorf("got %+v ok=%v", got, ok)
	}
}

func TestConcurrentClaimsElectExactlyOneWinner(t *testing.T) {
	tbl := newTable(t, Config{})

	const attempts = 200
	var wg sync.WaitGroup
	winners := make([]bool, attempts)

	// Every goroutine submits the same logical message, as a client with
	// several queued retries would.
	k := Key{Sender: "alice", ClientMsgID: "contested"}
	for i := 0; i < attempts; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, _, claimed := tbl.Claim(k)
			winners[i] = claimed
		}(i)
	}
	wg.Wait()

	count := 0
	for _, w := range winners {
		if w {
			count++
		}
	}
	if count != 1 {
		t.Fatalf("%d goroutines claimed the same key — exactly one must win", count)
	}
}

func TestConcurrentDistinctKeys(t *testing.T) {
	tbl := newTable(t, Config{})

	var wg sync.WaitGroup
	for s := 0; s < 10; s++ {
		wg.Add(1)
		go func(s int) {
			defer wg.Done()
			for i := 0; i < 200; i++ {
				k := Key{Sender: fmt.Sprintf("u%d", s), ClientMsgID: fmt.Sprint(i)}
				if _, _, claimed := tbl.Claim(k); !claimed {
					t.Errorf("distinct key %v was refused", k)
					return
				}
				tbl.Complete(k, Result{MessageID: fmt.Sprintf("%d-%d", s, i)})
			}
		}(s)
	}
	wg.Wait()

	if tbl.Len() != 2000 {
		t.Errorf("tracked %d keys, want 2000", tbl.Len())
	}
}

func TestLookupOnlyReturnsCompleted(t *testing.T) {
	tbl := newTable(t, Config{})
	k := Key{Sender: "alice", ClientMsgID: "m1"}

	tbl.Claim(k)
	if _, ok := tbl.Lookup(k); ok {
		t.Error("Lookup returned an in-flight claim")
	}
	tbl.Complete(k, Result{MessageID: "srv-1"})
	if _, ok := tbl.Lookup(k); !ok {
		t.Error("Lookup missed a completed claim")
	}
}

func BenchmarkClaimComplete(b *testing.B) {
	tbl := New(Config{SweepInterval: time.Hour})
	defer tbl.Close()

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		k := Key{Sender: "alice", ClientMsgID: fmt.Sprint(i)}
		tbl.Claim(k)
		tbl.Complete(k, Result{MessageID: "x", Seq: uint64(i)})
	}
}

func BenchmarkClaimContended(b *testing.B) {
	tbl := New(Config{SweepInterval: time.Hour})
	defer tbl.Close()

	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		i := 0
		for pb.Next() {
			tbl.Claim(Key{Sender: "alice", ClientMsgID: fmt.Sprint(i)})
			i++
		}
	})
}
