package stream

import (
	"fmt"
	"testing"
)

// newMixedLog builds one topic whose single partition interleaves records from
// several keys — the layout a partition-first topic map produces, where many
// conversations share a partition.
func newMixedLog(t *testing.T, keys []string, perKey int) *Log {
	t.Helper()
	l, err := OpenLog(LogConfig{
		Dir:          t.TempDir(),
		DefaultTopic: TopicConfig{Partitions: 1},
	})
	if err != nil {
		t.Fatalf("open log: %v", err)
	}
	t.Cleanup(func() { l.Close() })

	if _, err := l.CreateTopic("conversations", TopicConfig{Partitions: 1}); err != nil {
		t.Fatalf("create topic: %v", err)
	}
	for i := 0; i < perKey; i++ {
		for _, k := range keys {
			_, err := l.Append("conversations", &Record{
				Key:     []byte(k),
				Payload: []byte(fmt.Sprintf("%s-%d", k, i)),
			})
			if err != nil {
				t.Fatalf("append: %v", err)
			}
		}
	}
	return l
}

// The bug this guards: ReadByKey resolves a partition by key but does not
// filter by it. Sharing a partition between conversations would hand one
// caller another conversation's messages.
func TestReadByKeyDoesNotFilter(t *testing.T) {
	l := newMixedLog(t, []string{"conv-a", "conv-b"}, 5)

	got, _, err := l.ReadByKey("conversations", []byte("conv-a"), 1, 100)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if len(got) != 10 {
		t.Fatalf("ReadByKey returned %d records, want all 10 in the partition — "+
			"if this now filters, callers relying on the unfiltered form changed behaviour", len(got))
	}
}

func TestReadKeyOnlyFiltersByKey(t *testing.T) {
	l := newMixedLog(t, []string{"conv-a", "conv-b", "conv-c"}, 5)

	got, _, next, err := l.ReadKeyOnly("conversations", []byte("conv-b"), 1, 100, 0)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if len(got) != 5 {
		t.Fatalf("got %d records for conv-b, want 5", len(got))
	}
	for i, r := range got {
		if string(r.Key) != "conv-b" {
			t.Fatalf("record %d has key %q — another conversation leaked into the result", i, r.Key)
		}
		if want := fmt.Sprintf("conv-b-%d", i); string(r.Payload) != want {
			t.Fatalf("record %d payload = %q, want %q", i, r.Payload, want)
		}
	}
	if next != 16 {
		t.Fatalf("nextSeq = %d, want 16 (one past the last record examined)", next)
	}
}

// Pagination must resume past every record examined, not past the last one
// returned, or each page re-scans the previous page's foreign records.
func TestReadKeyOnlyPaginates(t *testing.T) {
	l := newMixedLog(t, []string{"conv-a", "conv-b"}, 10)

	var all []*Record
	from := uint64(1)
	for i := 0; i < 10; i++ {
		page, _, next, err := l.ReadKeyOnly("conversations", []byte("conv-a"), from, 3, 0)
		if err != nil {
			t.Fatalf("page %d: %v", i, err)
		}
		all = append(all, page...)
		if next == from {
			break // no progress; done
		}
		from = next
	}

	if len(all) != 10 {
		t.Fatalf("paginated to %d records, want 10", len(all))
	}
	for i, r := range all {
		if want := fmt.Sprintf("conv-a-%d", i); string(r.Payload) != want {
			t.Fatalf("record %d = %q, want %q — pagination lost or duplicated records", i, r.Payload, want)
		}
	}
}

// A key buried among far busier ones must not make one call read the whole
// partition. The scan budget bounds the work; the caller paginates.
func TestReadKeyOnlyRespectsScanBudget(t *testing.T) {
	keys := make([]string, 0, 50)
	for i := 0; i < 50; i++ {
		keys = append(keys, fmt.Sprintf("conv-%02d", i))
	}
	l := newMixedLog(t, keys, 20) // 1000 records, 20 per key

	got, _, next, err := l.ReadKeyOnly("conversations", []byte("conv-49"), 1, 20, 100)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if len(got) >= 20 {
		t.Fatalf("got %d records despite a 100-record scan budget; the budget is not enforced", len(got))
	}
	if next > 101 {
		t.Fatalf("scanned to seq %d with a 100-record budget", next)
	}

	// Resuming must eventually reach every record for the key.
	total := len(got)
	from := next
	for i := 0; i < 50 && total < 20; i++ {
		page, _, n, err := l.ReadKeyOnly("conversations", []byte("conv-49"), from, 20, 100)
		if err != nil {
			t.Fatalf("resume: %v", err)
		}
		total += len(page)
		if n == from {
			break
		}
		from = n
	}
	if total != 20 {
		t.Fatalf("paginating with a scan budget reached %d records, want 20", total)
	}
}

func TestReadKeyOnlyEmptyResults(t *testing.T) {
	l := newMixedLog(t, []string{"conv-a"}, 3)

	got, _, _, err := l.ReadKeyOnly("conversations", []byte("conv-missing"), 1, 10, 0)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("got %d records for a key that was never written", len(got))
	}

	if _, _, _, err := l.ReadKeyOnly("nope", []byte("k"), 1, 10, 0); err == nil {
		t.Fatal("reading an unknown topic did not error")
	}
}
