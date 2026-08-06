// Package queuelog implements work-queue semantics on top of the stream log.
//
// The two planes in this system need different delivery semantics but not
// different storage. A stream consumer owns a cursor and can rewind; a queue
// consumer competes with its peers for messages, acknowledges them
// individually, and never sees an acknowledged message again. Only the second
// half of that sentence needs machinery the log does not already provide — so
// that is all this package adds. Durability, replication, leader election and
// crash recovery stay in exactly one place: internal/stream.
//
// The unit of that machinery is the share partition: a bounded window of
// per-record delivery state layered over one stream partition. Records below
// the window are done, records above it have never been handed out, and the
// window itself is the small mutable region where competing consumers, ack
// timeouts and redelivery live.
package queuelog

import (
	"container/heap"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/boltq/boltq/internal/stream"
)

var (
	// ErrUnknownDelivery means the referenced record is not currently held by
	// any consumer. Acking twice, or acking after the lease expired and the
	// record was handed to someone else, both land here.
	ErrUnknownDelivery = errors.New("queuelog: no such in-flight delivery")
	// ErrNotHolder means the caller acknowledged a record leased to a different
	// consumer. Honouring it would let one worker retire another's message.
	ErrNotHolder = errors.New("queuelog: delivery is held by another consumer")
)

// DeadLetterFunc is called when a record has been delivered maxDelivery times
// without being acknowledged.
//
// Returning nil retires the record: the window advances past it and it is never
// redelivered. Returning an error leaves the record available, so a dead-letter
// route that is temporarily broken causes redelivery rather than silent loss.
type DeadLetterFunc func(topic string, partition int32, rec *stream.Record, reason string) error

// deliveryState is the state of one record inside the window.
type deliveryState uint8

const (
	// stateAvailable means the record may be handed to a consumer. It is either
	// freshly read from the log or was released after a nack or a lease expiry.
	stateAvailable deliveryState = iota
	// stateAcquired means a consumer holds it under a lease.
	stateAcquired
)

// entry is one record's delivery state.
type entry struct {
	rec      *stream.Record
	state    deliveryState
	count    int       // how many times this record has been delivered
	deadline time.Time // lease expiry; meaningful only while acquired
	holder   string    // consumer holding the lease
}

// seqHeap is a min-heap of sequences available for delivery. Redelivery must
// prefer the oldest record: handing out newer ones first would let a poison
// message sit at the window base and stall advancement indefinitely.
type seqHeap []uint64

func (h seqHeap) Len() int            { return len(h) }
func (h seqHeap) Less(i, j int) bool  { return h[i] < h[j] }
func (h seqHeap) Swap(i, j int)       { h[i], h[j] = h[j], h[i] }
func (h *seqHeap) Push(x interface{}) { *h = append(*h, x.(uint64)) }
func (h *seqHeap) Pop() interface{} {
	old := *h
	n := len(old)
	v := old[n-1]
	*h = old[:n-1]
	return v
}

// shareConfig tunes one share partition.
type shareConfig struct {
	// Group is the consumer group competing for this partition. Two groups on
	// the same partition are fully independent: each has its own window and its
	// own cursor, which is what lets a queue and an audit consumer read the same
	// topic without interfering.
	Group string
	// AckTimeout is how long a consumer may hold a record before the lease
	// expires and the record becomes available to its peers.
	AckTimeout time.Duration
	// MaxDelivery caps redelivery attempts before dead-lettering. Zero means
	// unlimited, which is almost never what an operator wants — a poison message
	// then blocks the window base forever.
	MaxDelivery int
	// MaxInFlight bounds the window: the highest sequence handed out may not
	// exceed the window base by more than this. It is the back-pressure knob and
	// the memory bound, since every record in the window is held in memory for
	// redelivery without a second disk read.
	MaxInFlight int
	// DeadLetter routes records that exhausted MaxDelivery.
	DeadLetter DeadLetterFunc
}

func (c *shareConfig) applyDefaults() {
	if c.AckTimeout <= 0 {
		c.AckTimeout = 30 * time.Second
	}
	if c.MaxDelivery <= 0 {
		c.MaxDelivery = 5
	}
	if c.MaxInFlight <= 0 {
		c.MaxInFlight = 1000
	}
}

// sharePartition holds per-record delivery state for one consumer group over
// one stream partition.
//
// The invariant that makes this cheap: state exists only for sequences in
// [base, next). Everything below base is acknowledged or dead-lettered and is
// the cursor's business; everything at or above next has not been read yet and
// is the log's business. The window is bounded by MaxInFlight, so the memory
// cost of queue semantics is a constant per partition rather than a function of
// backlog depth.
type sharePartition struct {
	topic   string
	part    *stream.Partition
	cursors *stream.CursorStore
	cfg     shareConfig

	mu sync.Mutex
	// base is the lowest sequence not yet retired; it is what gets committed.
	base uint64
	// next is the sequence the next read from the log starts at.
	next uint64
	// entries holds state for [base, next) minus retired sequences.
	entries map[uint64]*entry
	// retired is the set of sequences in [base, next) that are done but cannot
	// advance base yet because an older sequence is still in flight.
	retired map[uint64]struct{}
	// available is a min-heap of sequences in entries with stateAvailable.
	available seqHeap
	closed    bool

	// Counters for the admin API. Kept here rather than derived on demand
	// because deriving them means walking the window under the lock.
	delivered uint64
	acked     uint64
	nacked    uint64
	expired   uint64
	dlqCount  uint64
}

// openSharePartition restores a share partition's window base from the cursor
// store.
//
// Only the base is persisted, not the per-record window. That is a deliberate
// at-least-once trade: after a crash, records that were acknowledged out of
// order above the base are delivered again, which a consumer deduplicates by
// message ID. Persisting the full window would cost a cursor write per ack
// instead of per window advance, and would buy exactly-once only for the
// crash case — which the rest of the system does not offer either.
func openSharePartition(topic string, part *stream.Partition, cursors *stream.CursorStore, cfg shareConfig) *sharePartition {
	cfg.applyDefaults()

	sp := &sharePartition{
		topic:   topic,
		part:    part,
		cursors: cursors,
		cfg:     cfg,
		entries: make(map[uint64]*entry),
		retired: make(map[uint64]struct{}),
	}

	base := part.FirstSeq()
	if cursors != nil {
		base = cursors.PositionOr(sp.cursorKey(), base)
	}
	// A committed base below the retention horizon means retention overtook this
	// group while it was down. Resuming at FirstSeq is the only option; the
	// records in between are gone, and pretending otherwise would make every
	// read fail with ErrSeqTruncated forever.
	if first := part.FirstSeq(); base < first {
		base = first
	}
	sp.base = base
	sp.next = base
	return sp
}

func (sp *sharePartition) cursorKey() stream.CursorKey {
	return stream.CursorKey{
		Topic:     sp.topic,
		Partition: sp.part.ID,
		Group:     sp.cfg.Group,
	}
}

// Delivery is one record leased to a consumer.
type Delivery struct {
	Topic     string         `json:"topic"`
	Partition int32          `json:"partition"`
	Seq       uint64         `json:"seq"`
	Record    *stream.Record `json:"-"`
	// Count is how many times this record has now been delivered, starting at 1.
	// A consumer uses it to distinguish a first attempt from a retry.
	Count    int       `json:"delivery_count"`
	Consumer string    `json:"consumer"`
	Deadline time.Time `json:"deadline"`
}

// acquire leases up to max records to consumer.
//
// Released records are handed out before new ones, oldest first. Draining the
// backlog of retries first is what keeps the window base moving; preferring
// fresh records would let a single repeatedly-nacked message pin the base while
// the window filled up behind it.
func (sp *sharePartition) acquire(consumer string, max int, now time.Time) ([]Delivery, error) {
	if max <= 0 {
		return nil, nil
	}

	sp.mu.Lock()
	defer sp.mu.Unlock()
	if sp.closed {
		return nil, stream.ErrClosed
	}

	sp.reapExpiredLocked(now)

	out := make([]Delivery, 0, max)

	// Phase 1: redeliveries.
	for len(out) < max && sp.available.Len() > 0 {
		seq := heap.Pop(&sp.available).(uint64)
		e, ok := sp.entries[seq]
		if !ok || e.state != stateAvailable {
			continue // stale heap entry: retired or re-acquired since being pushed
		}
		out = append(out, sp.leaseLocked(seq, e, consumer, now))
	}

	// Phase 2: fresh records, bounded by the in-flight window.
	for len(out) < max {
		room := sp.cfg.MaxInFlight - int(sp.next-sp.base)
		if room <= 0 {
			break
		}
		want := max - len(out)
		if want > room {
			want = room
		}

		recs, err := sp.part.ReadFrom(sp.next, want)
		if err != nil {
			if errors.Is(err, stream.ErrSeqTruncated) {
				// Retention passed the window base. Skip forward rather than
				// wedging: the records are gone and no amount of retrying brings
				// them back.
				sp.skipToLocked(sp.part.FirstSeq())
				continue
			}
			if len(out) > 0 {
				// Records already leased must reach the caller; reporting the
				// error alone would leak the leases until they timed out.
				return out, nil
			}
			return nil, err
		}
		if len(recs) == 0 {
			break // caught up with the log
		}

		for _, rec := range recs {
			if len(out) >= max {
				break
			}
			e := &entry{rec: rec, state: stateAvailable}
			sp.entries[rec.Seq] = e
			if rec.Seq >= sp.next {
				sp.next = rec.Seq + 1
			}
			out = append(out, sp.leaseLocked(rec.Seq, e, consumer, now))
		}
	}

	sp.delivered += uint64(len(out))
	return out, nil
}

// leaseLocked marks an entry acquired and builds its Delivery.
func (sp *sharePartition) leaseLocked(seq uint64, e *entry, consumer string, now time.Time) Delivery {
	e.state = stateAcquired
	e.count++
	e.holder = consumer
	e.deadline = now.Add(sp.cfg.AckTimeout)
	return Delivery{
		Topic:     sp.topic,
		Partition: sp.part.ID,
		Seq:       seq,
		Record:    e.rec,
		Count:     e.count,
		Consumer:  consumer,
		Deadline:  e.deadline,
	}
}

// ack retires a record. The consumer must be the lease holder.
func (sp *sharePartition) ack(consumer string, seq uint64) error {
	sp.mu.Lock()
	defer sp.mu.Unlock()
	if sp.closed {
		return stream.ErrClosed
	}

	e, ok := sp.entries[seq]
	if !ok {
		return fmt.Errorf("%w: %s/%d seq %d", ErrUnknownDelivery, sp.topic, sp.part.ID, seq)
	}
	if e.state != stateAcquired {
		return fmt.Errorf("%w: %s/%d seq %d is not leased", ErrUnknownDelivery, sp.topic, sp.part.ID, seq)
	}
	if e.holder != consumer {
		return fmt.Errorf("%w: %s/%d seq %d held by %q", ErrNotHolder, sp.topic, sp.part.ID, seq, e.holder)
	}

	sp.retireLocked(seq)
	sp.acked++
	return sp.advanceLocked()
}

// nack releases a record for redelivery, or dead-letters it once it has been
// delivered MaxDelivery times.
func (sp *sharePartition) nack(consumer string, seq uint64, requeue bool) error {
	sp.mu.Lock()
	defer sp.mu.Unlock()
	if sp.closed {
		return stream.ErrClosed
	}

	e, ok := sp.entries[seq]
	if !ok || e.state != stateAcquired {
		return fmt.Errorf("%w: %s/%d seq %d", ErrUnknownDelivery, sp.topic, sp.part.ID, seq)
	}
	if e.holder != consumer {
		return fmt.Errorf("%w: %s/%d seq %d held by %q", ErrNotHolder, sp.topic, sp.part.ID, seq, e.holder)
	}

	sp.nacked++

	// requeue=false is an explicit "this message is bad, do not retry" from the
	// consumer. It bypasses the delivery counter and goes straight to the
	// dead-letter route, which is the only way a consumer can reject a message
	// it knows it will never process without burning MaxDelivery attempts.
	if !requeue {
		sp.deadLetterLocked(seq, e, "rejected by consumer")
		return sp.advanceLocked()
	}
	if sp.cfg.MaxDelivery > 0 && e.count >= sp.cfg.MaxDelivery {
		sp.deadLetterLocked(seq, e, fmt.Sprintf("delivered %d times without ack", e.count))
		return sp.advanceLocked()
	}

	sp.releaseLocked(seq, e)
	return nil
}

// releaseLocked returns an entry to the available pool.
func (sp *sharePartition) releaseLocked(seq uint64, e *entry) {
	e.state = stateAvailable
	e.holder = ""
	e.deadline = time.Time{}
	heap.Push(&sp.available, seq)
}

// deadLetterLocked routes a record out of the queue and retires it.
//
// A failing dead-letter route must not drop the record: the entry goes back to
// available instead, so the message is retried rather than lost when the
// dead-letter exchange is misconfigured or the disk is full.
func (sp *sharePartition) deadLetterLocked(seq uint64, e *entry, reason string) {
	if sp.cfg.DeadLetter != nil {
		if err := sp.cfg.DeadLetter(sp.topic, sp.part.ID, e.rec, reason); err != nil {
			sp.releaseLocked(seq, e)
			return
		}
	}
	sp.dlqCount++
	sp.retireLocked(seq)
}

// retireLocked removes an entry from the window and marks its sequence done.
func (sp *sharePartition) retireLocked(seq uint64) {
	delete(sp.entries, seq)
	sp.retired[seq] = struct{}{}
}

// reapExpiredLocked releases leases whose deadline has passed.
//
// This is what makes a crashed consumer harmless: its messages return to the
// pool after AckTimeout instead of being stuck until an operator notices.
func (sp *sharePartition) reapExpiredLocked(now time.Time) {
	for seq, e := range sp.entries {
		if e.state != stateAcquired || now.Before(e.deadline) {
			continue
		}
		sp.expired++
		if sp.cfg.MaxDelivery > 0 && e.count >= sp.cfg.MaxDelivery {
			sp.deadLetterLocked(seq, e, fmt.Sprintf("lease expired after %d deliveries", e.count))
			continue
		}
		sp.releaseLocked(seq, e)
	}
	sp.advanceLocked()
}

// advanceLocked slides the window base past the contiguous run of retired
// sequences and commits the new base.
//
// Only a contiguous prefix can be committed. A cursor is a single number, so
// committing past a sequence that is still in flight would mean a crash loses
// that record entirely — the window would restart above it and nothing would
// ever read it again.
func (sp *sharePartition) advanceLocked() error {
	moved := false
	for {
		if _, ok := sp.retired[sp.base]; !ok {
			break
		}
		delete(sp.retired, sp.base)
		sp.base++
		moved = true
	}
	if !moved || sp.cursors == nil {
		return nil
	}
	return sp.cursors.Commit(sp.cursorKey(), sp.base)
}

// skipToLocked abandons everything below seq. Used when retention has removed
// records the window still referred to.
func (sp *sharePartition) skipToLocked(seq uint64) {
	if seq <= sp.base {
		return
	}
	for s := range sp.entries {
		if s < seq {
			delete(sp.entries, s)
		}
	}
	for s := range sp.retired {
		if s < seq {
			delete(sp.retired, s)
		}
	}
	sp.base = seq
	if sp.next < seq {
		sp.next = seq
	}
	if sp.cursors != nil {
		sp.cursors.Commit(sp.cursorKey(), sp.base)
	}
}

// releaseConsumer returns every lease held by a consumer, used when it
// disconnects. Waiting for AckTimeout would be correct but needlessly slow: a
// closed socket is proof the lease will never be acknowledged.
// releaseAll returns every in-flight lease to the available pool, regardless of
// who holds it.
//
// Called when this node stops leading the partition. The leases are worthless
// from that moment — the new leader is handing out the same records from its own
// view — so holding them only means this node's window disagrees with the one
// that now matters.
func (sp *sharePartition) releaseAll() int {
	sp.mu.Lock()
	defer sp.mu.Unlock()

	n := 0
	for seq, e := range sp.entries {
		if e.state == stateAcquired {
			sp.releaseLocked(seq, e)
			n++
		}
	}
	return n
}

func (sp *sharePartition) releaseConsumer(consumer string) int {
	sp.mu.Lock()
	defer sp.mu.Unlock()

	n := 0
	for seq, e := range sp.entries {
		if e.state == stateAcquired && e.holder == consumer {
			sp.releaseLocked(seq, e)
			n++
		}
	}
	return n
}

// shareStats summarises one share partition.
type shareStats struct {
	Partition    int32  `json:"partition"`
	Base         uint64 `json:"base"`
	Next         uint64 `json:"next"`
	LogEnd       uint64 `json:"log_end"`
	Backlog      uint64 `json:"backlog"`
	InFlight     int    `json:"in_flight"`
	Available    int    `json:"available"`
	Delivered    uint64 `json:"delivered"`
	Acked        uint64 `json:"acked"`
	Nacked       uint64 `json:"nacked"`
	Expired      uint64 `json:"expired"`
	DeadLettered uint64 `json:"dead_lettered"`
}

func (sp *sharePartition) stats() shareStats {
	sp.mu.Lock()
	defer sp.mu.Unlock()

	inflight, avail := 0, 0
	for _, e := range sp.entries {
		if e.state == stateAcquired {
			inflight++
		} else {
			avail++
		}
	}
	end := sp.part.NextSeq()
	backlog := uint64(0)
	if end > sp.base {
		backlog = end - sp.base
	}
	return shareStats{
		Partition: sp.part.ID, Base: sp.base, Next: sp.next, LogEnd: end,
		Backlog: backlog, InFlight: inflight, Available: avail,
		Delivered: sp.delivered, Acked: sp.acked, Nacked: sp.nacked,
		Expired: sp.expired, DeadLettered: sp.dlqCount,
	}
}

func (sp *sharePartition) close() {
	sp.mu.Lock()
	defer sp.mu.Unlock()
	sp.closed = true
}
