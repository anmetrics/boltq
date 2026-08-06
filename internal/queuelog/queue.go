package queuelog

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"github.com/boltq/boltq/internal/stream"
)

// Config tunes a queue.
type Config struct {
	// Group names the consumer group competing for this queue's records. It
	// defaults to the queue name, which is the RabbitMQ-shaped case: one queue,
	// one set of competing workers. Naming it explicitly is what allows two
	// independent groups over the same underlying topic.
	Group string
	// AckTimeout is how long a consumer may hold a record before its lease
	// expires and peers may take it.
	AckTimeout time.Duration
	// MaxDelivery caps redelivery attempts before dead-lettering.
	MaxDelivery int
	// MaxInFlight bounds in-flight records per partition.
	MaxInFlight int
	// Partitions is the partition count used if the topic must be created.
	//
	// Partitions are the concurrency unit: two consumers can work the same
	// partition simultaneously because delivery state is per record, but a
	// single partition still serialises log reads. Sizing it to the expected
	// worker count is the right default.
	Partitions int32
	// DeadLetter routes records that exhaust MaxDelivery. A nil value discards
	// them, which is almost always wrong — wire it to a dead-letter route.
	DeadLetter DeadLetterFunc
	// SweepInterval is how often expired leases are reclaimed in the background.
	// Defaults to AckTimeout/2, so a lease is never late by more than half its
	// own duration.
	SweepInterval time.Duration
}

// Queue is a work queue backed by one stream topic.
//
// It adds exactly one thing to the log: per-record delivery state for a
// consumer group, held in a bounded window per partition. Everything else —
// durability, replication, retention, recovery — is the log's, unchanged and
// unduplicated. That is the whole point of building the queue plane here rather
// than beside it.
type Queue struct {
	name    string
	log     *stream.Log
	topic   *stream.Topic
	cursors *stream.CursorStore
	cfg     Config

	parts []*sharePartition
	// rotate spreads consumers across partitions. Without it every consumer
	// would start its scan at partition 0 and contend on the same lock while the
	// rest of the topic sat idle.
	rotate atomic.Uint64

	// leadership and gate restrict leasing to partitions this node leads. Both
	// are nil on a single node, where there is nobody to double-deliver with.
	leadership LeadershipSource
	gate       *leadershipGate

	stopSweep func()
	closeOnce sync.Once
	closed    atomic.Bool
}

// Open creates or opens a queue over topic `name`.
func Open(log *stream.Log, cursors *stream.CursorStore, name string, cfg Config) (*Queue, error) {
	if log == nil {
		return nil, errors.New("queuelog: a stream log is required")
	}
	if name == "" {
		return nil, errors.New("queuelog: queue name is required")
	}
	if cfg.Group == "" {
		cfg.Group = name
	}
	if cfg.AckTimeout <= 0 {
		cfg.AckTimeout = 30 * time.Second
	}
	if cfg.SweepInterval <= 0 {
		cfg.SweepInterval = cfg.AckTimeout / 2
	}

	var (
		topic *stream.Topic
		err   error
	)
	if cfg.Partitions > 0 {
		// CreateTopic is idempotent when the partition count matches and an error
		// when it does not — which is the right outcome: silently opening a topic
		// with a different partition count would remap every key.
		topic, err = log.CreateTopic(name, stream.TopicConfig{Partitions: cfg.Partitions})
	} else {
		topic, err = log.GetOrCreateTopic(name)
	}
	if err != nil {
		return nil, fmt.Errorf("queuelog: open topic %s: %w", name, err)
	}

	q := &Queue{name: name, log: log, topic: topic, cursors: cursors, cfg: cfg}

	scfg := shareConfig{
		Group:       cfg.Group,
		AckTimeout:  cfg.AckTimeout,
		MaxDelivery: cfg.MaxDelivery,
		MaxInFlight: cfg.MaxInFlight,
		DeadLetter:  cfg.DeadLetter,
	}
	for _, p := range topic.Partitions() {
		q.parts = append(q.parts, openSharePartition(name, p, cursors, scfg))
	}
	if len(q.parts) == 0 {
		return nil, fmt.Errorf("queuelog: topic %s has no partitions", name)
	}

	// Ungated by default: a single-node queue has exactly one server, and a gate
	// there would refuse every legitimate lease. EnforceLeadership installs one.
	q.gate = newLeadershipGate(len(q.parts), false)

	q.stopSweep = q.startSweeper(cfg.SweepInterval)
	return q, nil
}

// Name returns the queue name.
func (q *Queue) Name() string { return q.name }

// Topic exposes the underlying stream topic, for callers that want to read the
// same records as a stream — an audit tail, a replay tool — without going
// through delivery state.
func (q *Queue) Topic() *stream.Topic { return q.topic }

// Publish appends a record to the queue.
//
// Ordering: records sharing a key land in the same partition and are delivered
// in append order, but only when a single consumer works that partition.
// Competing consumers trade ordering for throughput, which is the same bargain
// RabbitMQ makes and the reason ordered work must use a key plus one consumer.
func (q *Queue) Publish(ctx context.Context, rec *stream.Record) (stream.AppendResult, error) {
	if q.closed.Load() {
		return stream.AppendResult{}, stream.ErrClosed
	}
	return q.log.AppendContext(ctx, q.name, rec)
}

// TryConsume leases up to prefetch records without blocking. An empty result
// means the queue is drained.
func (q *Queue) TryConsume(consumer string, prefetch int) ([]Delivery, error) {
	if q.closed.Load() {
		return nil, stream.ErrClosed
	}
	if prefetch <= 0 {
		prefetch = 1
	}

	now := time.Now()
	out := make([]Delivery, 0, prefetch)
	start := int(q.rotate.Add(1) % uint64(len(q.parts)))

	// Sweep every partition once, starting at a rotating offset. Stopping at the
	// first partition that yields anything would starve later partitions
	// whenever the first has a steady trickle of work.
	for i := 0; i < len(q.parts) && len(out) < prefetch; i++ {
		idx := (start + i) % len(q.parts)
		// Partitions led elsewhere are skipped, not refused. A consumer polling
		// this node should get whatever this node may serve; the rest of the
		// queue is another node's to hand out.
		if !q.gate.mayLease(int32(idx)) {
			continue
		}
		sp := q.parts[idx]
		got, err := sp.acquire(consumer, prefetch-len(out), now)
		if err != nil {
			if errors.Is(err, stream.ErrClosed) {
				continue
			}
			if len(out) > 0 {
				return out, nil
			}
			return nil, err
		}
		out = append(out, got...)
	}
	return out, nil
}

// Consume leases up to prefetch records, blocking until at least one is
// available or ctx is cancelled.
//
// A cancelled context returns ctx.Err() with no leases, so a consumer that
// times out never leaves records held by a request it has abandoned.
func (q *Queue) Consume(ctx context.Context, consumer string, prefetch int) ([]Delivery, error) {
	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}

		// Register for wake-ups before acquiring. A record appended between the
		// acquire and the wait would otherwise leave this consumer asleep until
		// the retry timer fired, turning a sub-millisecond handoff into seconds.
		wake, stop := q.waitChan()

		out, err := q.TryConsume(consumer, prefetch)
		if err != nil {
			stop()
			return nil, err
		}
		if len(out) > 0 {
			stop()
			return out, nil
		}

		// The timer covers what the notify channel cannot: a lease expiring makes
		// a record available without anything being appended, so there is no log
		// event to wake on.
		timer := time.NewTimer(q.retryDelay())
		select {
		case <-wake:
		case <-timer.C:
		case <-ctx.Done():
			timer.Stop()
			stop()
			return nil, ctx.Err()
		}
		timer.Stop()
		stop()
	}
}

// retryDelay bounds how long a blocked consumer sleeps before re-checking for
// expired leases.
func (q *Queue) retryDelay() time.Duration {
	d := q.cfg.AckTimeout / 4
	if d > time.Second {
		d = time.Second
	}
	if d < 10*time.Millisecond {
		d = 10 * time.Millisecond
	}
	return d
}

// waitChan returns a channel closed when any partition accepts a record, plus a
// stop function that must be called to release the watcher goroutines.
func (q *Queue) waitChan() (<-chan struct{}, func()) {
	wake := make(chan struct{})
	done := make(chan struct{})
	var once sync.Once
	fire := func() { once.Do(func() { close(wake) }) }

	for _, sp := range q.parts {
		ch := sp.part.NotifyChan()
		go func() {
			select {
			case <-ch:
				fire()
			case <-done:
			}
		}()
	}
	return wake, func() { close(done) }
}

// Ack retires a leased record.
func (q *Queue) Ack(consumer string, partition int32, seq uint64) error {
	sp, err := q.partition(partition)
	if err != nil {
		return err
	}
	// An ack for a partition this node no longer leads refers to a lease that
	// no longer exists here. Reporting it plainly beats silently succeeding and
	// letting the consumer believe work was retired that will be redelivered.
	if err := q.checkLease(partition); err != nil {
		return err
	}
	return sp.ack(consumer, seq)
}

// AckAll retires a batch of leases, applying every one it can and returning the
// first failure. A partial batch must not abort: the records it did retire are
// genuinely processed, and rolling them back would redeliver work already done.
func (q *Queue) AckAll(consumer string, ds []Delivery) error {
	var firstErr error
	for _, d := range ds {
		if err := q.Ack(consumer, d.Partition, d.Seq); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}

// Nack releases a lease. With requeue the record is redelivered until
// MaxDelivery is reached; without it, the record is dead-lettered immediately.
func (q *Queue) Nack(consumer string, partition int32, seq uint64, requeue bool) error {
	sp, err := q.partition(partition)
	if err != nil {
		return err
	}
	if err := q.checkLease(partition); err != nil {
		return err
	}
	return sp.nack(consumer, seq, requeue)
}

// ReleaseConsumer returns every lease a consumer holds, for use when it
// disconnects.
func (q *Queue) ReleaseConsumer(consumer string) int {
	n := 0
	for _, sp := range q.parts {
		n += sp.releaseConsumer(consumer)
	}
	return n
}

func (q *Queue) partition(id int32) (*sharePartition, error) {
	if id < 0 || int(id) >= len(q.parts) {
		return nil, fmt.Errorf("queuelog: %s has no partition %d", q.name, id)
	}
	return q.parts[id], nil
}

// startSweeper reclaims expired leases even while no consumer is polling.
// Without it, a queue whose only consumer crashed would hold its records until
// someone happened to call Consume again.
func (q *Queue) startSweeper(interval time.Duration) func() {
	if interval <= 0 {
		interval = 5 * time.Second
	}
	done := make(chan struct{})
	go func() {
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-done:
				return
			case now := <-ticker.C:
				for _, sp := range q.parts {
					sp.mu.Lock()
					if !sp.closed {
						sp.reapExpiredLocked(now)
					}
					sp.mu.Unlock()
				}
			}
		}
	}()
	return func() { close(done) }
}

// Stats summarises the queue for the admin API.
type Stats struct {
	Name       string       `json:"name"`
	Group      string       `json:"group"`
	Partitions []shareStats `json:"partitions"`
	Backlog    uint64       `json:"backlog"`
	InFlight   int          `json:"in_flight"`
}

// Stats returns a snapshot of queue depth and delivery state.
func (q *Queue) Stats() Stats {
	s := Stats{Name: q.name, Group: q.cfg.Group}
	for _, sp := range q.parts {
		ps := sp.stats()
		s.Partitions = append(s.Partitions, ps)
		s.Backlog += ps.Backlog
		s.InFlight += ps.InFlight
	}
	return s
}

// Close stops the sweeper and rejects further operations. It does not close the
// underlying log, which the queue does not own.
func (q *Queue) Close() {
	q.closeOnce.Do(func() {
		q.closed.Store(true)
		if q.stopSweep != nil {
			q.stopSweep()
		}
		for _, sp := range q.parts {
			sp.close()
		}
		// Cursor progress is worth an fsync at shutdown: losing it costs every
		// consumer a redelivery of work it already finished.
		if q.cursors != nil {
			q.cursors.Flush()
		}
	})
}
