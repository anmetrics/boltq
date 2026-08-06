package stream

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sync"
	"sync/atomic"
	"time"
)

var (
	// ErrSeqTruncated means the requested sequence has already been removed by
	// retention. The caller should restart from FirstSeq and, for a chat
	// client, show a "messages older than this were removed" marker.
	ErrSeqTruncated = errors.New("stream: sequence below retention horizon")
	// ErrClosed means the partition has been shut down.
	ErrClosed = errors.New("stream: partition closed")
	// ErrReplicationGap means a follower received a record that does not
	// follow its local head. The follower must re-fetch from NextSeq rather
	// than write a hole into the log.
	ErrReplicationGap = errors.New("stream: replication gap")
	// ErrNotPartitionLeader means this node holds a copy of the partition but
	// has not been granted the right to write to it. Callers should route the
	// write to the current leader rather than retrying here — retrying against
	// the same node will never succeed, and succeeding would be worse.
	ErrNotPartitionLeader = errors.New("stream: not the partition leader")
)

// PartitionConfig tunes a single partition's on-disk behaviour.
type PartitionConfig struct {
	// SegmentBytes is the size at which the active segment is rolled.
	SegmentBytes int64
	// IndexInterval is how many bytes of log go between sparse index entries.
	IndexInterval int64
	// RetentionBytes caps total partition size; oldest segments are dropped
	// first. Zero disables size-based retention.
	RetentionBytes int64
	// RetentionAge caps record age. Zero disables time-based retention.
	RetentionAge time.Duration
	// SyncOnAppend fsyncs every append. Correct but slow (~1ms/append on
	// spinning media, ~100µs on NVMe). Leave false and rely on replication for
	// durability; see docs/architecture/durability.md.
	SyncOnAppend bool
}

// DefaultPartitionConfig returns settings tuned for chat-shaped workloads:
// many small records, read-mostly-recent, long retention.
func DefaultPartitionConfig() PartitionConfig {
	return PartitionConfig{
		SegmentBytes:   256 << 20, // 256MB
		IndexInterval:  4 << 10,   // one index entry per 4KB
		RetentionBytes: 0,         // unlimited by default; chat history is the product
		RetentionAge:   0,
		SyncOnAppend:   false,
	}
}

func (c *PartitionConfig) applyDefaults() {
	d := DefaultPartitionConfig()
	if c.SegmentBytes <= 0 {
		c.SegmentBytes = d.SegmentBytes
	}
	if c.IndexInterval <= 0 {
		c.IndexInterval = d.IndexInterval
	}
}

// Partition is an ordered, append-only log with gap-free sequence numbers.
//
// Ordering guarantee: every record appended to a partition receives a sequence
// exactly one greater than the previous, assigned under the partition lock.
// Readers scanning by sequence therefore observe the exact order in which
// appends were serialised — which is what makes "messages in a conversation
// arrive in order" a structural property rather than a best effort.
type Partition struct {
	Topic string
	ID    int32
	dir   string
	cfg   PartitionConfig

	mu       sync.RWMutex
	segments []*segment // ascending by baseSeq; last element is active
	nextSeq  uint64     // sequence the next append will receive
	firstSeq uint64     // oldest sequence still readable
	closed   bool

	// epochs is the leadership history used to reconcile a follower's tail
	// against a new leader. curEpoch is the term this node writes under; zero
	// on a standalone node that has never been given leadership.
	epochs   *leaderEpochCache
	curEpoch uint32

	// enforceLeadership makes Append refuse writes on a partition this node
	// does not lead, and isLeader records whether it does.
	//
	// Enforcement is opt-in because "who leads this partition" is a question
	// only a control plane can answer. On a standalone node, or one using
	// static replication roles, there is exactly one writer by construction and
	// the check would reject every legitimate append. Once a controller is
	// assigning leadership per partition, the check becomes the thing that
	// stops two nodes from assigning the same sequence to different records —
	// a divergence epochs cannot repair afterwards, because both writes carry a
	// locally-valid epoch.
	enforceLeadership bool
	isLeader          bool

	// notify is closed (and replaced) on every append to wake tailing readers.
	// A closed-channel broadcast beats sync.Cond here because readers need to
	// select against ctx.Done().
	notifyMu sync.Mutex
	notify   chan struct{}

	appendCount atomic.Uint64
	byteCount   atomic.Int64
}

// OpenPartition opens (or creates) a partition rooted at dir.
func OpenPartition(topic string, id int32, dir string, cfg PartitionConfig) (*Partition, error) {
	cfg.applyDefaults()
	if err := os.MkdirAll(dir, 0755); err != nil {
		return nil, fmt.Errorf("create partition dir: %w", err)
	}

	epochs, err := openEpochCache(dir)
	if err != nil {
		return nil, err
	}

	p := &Partition{
		Topic:  topic,
		ID:     id,
		dir:    dir,
		cfg:    cfg,
		notify: make(chan struct{}),
		epochs: epochs,
	}
	p.curEpoch, _ = epochs.latest()

	bases, err := listSegmentBases(dir)
	if err != nil {
		return nil, fmt.Errorf("list segments: %w", err)
	}

	if len(bases) == 0 {
		seg, err := createSegment(dir, 1, cfg.IndexInterval)
		if err != nil {
			return nil, err
		}
		p.segments = []*segment{seg}
		p.nextSeq = 1
		p.firstSeq = 1
		return p, nil
	}

	// All but the last segment are sealed; only the last accepts writes.
	for i, base := range bases {
		readOnly := i < len(bases)-1
		seg, err := openSegment(dir, base, cfg.IndexInterval, readOnly)
		if err != nil {
			for _, s := range p.segments {
				s.close()
			}
			return nil, err
		}
		p.segments = append(p.segments, seg)
		p.byteCount.Add(seg.bytes())
	}

	active := p.segments[len(p.segments)-1]
	p.nextSeq = active.lastSequence() + 1
	p.firstSeq = p.segments[0].baseSeq
	return p, nil
}

// Append writes a record and returns the sequence assigned to it.
//
// The record's Seq and Timestamp fields are overwritten by the broker; a
// client-supplied value is never trusted, because sequence is the ordering
// authority and a malicious client could otherwise rewrite history.
func (p *Partition) Append(rec *Record) (uint64, error) {
	if rec.Flags&FlagEphemeral != 0 {
		return 0, errors.New("stream: cannot persist an ephemeral record")
	}

	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		return 0, ErrClosed
	}
	if p.enforceLeadership && !p.isLeader {
		p.mu.Unlock()
		return 0, fmt.Errorf("%w: %s/%d", ErrNotPartitionLeader, p.Topic, p.ID)
	}

	seq := p.nextSeq
	rec.Seq = seq
	rec.Epoch = p.curEpoch
	if rec.Timestamp == 0 {
		rec.Timestamp = time.Now().UnixNano()
	}

	encoded, err := rec.encode()
	if err != nil {
		p.mu.Unlock()
		return 0, err
	}

	active := p.segments[len(p.segments)-1]
	if active.bytes()+int64(len(encoded)) > p.cfg.SegmentBytes && !active.isEmpty() {
		if active, err = p.rollLocked(seq); err != nil {
			p.mu.Unlock()
			return 0, err
		}
	}

	if err := active.append(encoded, seq); err != nil {
		p.mu.Unlock()
		return 0, err
	}
	p.nextSeq = seq + 1
	p.mu.Unlock()

	p.appendCount.Add(1)
	p.byteCount.Add(int64(len(encoded)))

	if p.cfg.SyncOnAppend {
		if err := active.sync(); err != nil {
			return 0, fmt.Errorf("sync after append: %w", err)
		}
	}

	p.broadcast()
	return seq, nil
}

// AppendReplicated writes a record whose sequence was assigned by a leader.
//
// This is the follower's write path, and it differs from Append in exactly one
// way that matters: the sequence is an input, not an output. A follower that
// assigned its own sequences would diverge from the leader the moment the two
// disagreed about ordering, and the log would no longer be replicated — it
// would be two different logs with the same name.
//
// The three possible relationships to the local head are all normal:
//
//	seq == nextSeq   the expected case; append it
//	seq <  nextSeq   a replay after a reconnect; already durable, skip
//	seq >  nextSeq   a gap, which means records were missed; refuse, so the
//	                 follower re-fetches from its true position rather than
//	                 writing a hole into the log
func (p *Partition) AppendReplicated(rec *Record) error {
	if rec.Flags&FlagEphemeral != 0 {
		return errors.New("stream: cannot replicate an ephemeral record")
	}
	if rec.Seq == 0 {
		return errors.New("stream: replicated record has no sequence")
	}

	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		return ErrClosed
	}

	if rec.Seq < p.nextSeq {
		p.mu.Unlock()
		return nil // idempotent: already applied
	}
	if rec.Seq > p.nextSeq {
		expected := p.nextSeq
		p.mu.Unlock()
		return fmt.Errorf("%w: got %d, expected %d", ErrReplicationGap, rec.Seq, expected)
	}

	// A record carrying a newer epoch is the first evidence this follower has
	// that leadership changed. Recording where the term began — before the
	// record lands — keeps the checkpoint a superset of what the log holds, so
	// a crash between the two leaves an epoch with no records rather than
	// records with no epoch. The former is harmless; the latter would make a
	// later truncation query unanswerable.
	if rec.Epoch > p.curEpoch {
		if err := p.epochs.assign(rec.Epoch, rec.Seq); err != nil {
			p.mu.Unlock()
			return err
		}
		p.curEpoch = rec.Epoch
	}

	encoded, err := rec.encode()
	if err != nil {
		p.mu.Unlock()
		return err
	}

	active := p.segments[len(p.segments)-1]
	if active.bytes()+int64(len(encoded)) > p.cfg.SegmentBytes && !active.isEmpty() {
		if active, err = p.rollLocked(rec.Seq); err != nil {
			p.mu.Unlock()
			return err
		}
	}
	if err := active.append(encoded, rec.Seq); err != nil {
		p.mu.Unlock()
		return err
	}
	p.nextSeq = rec.Seq + 1
	p.mu.Unlock()

	p.appendCount.Add(1)
	p.byteCount.Add(int64(len(encoded)))

	if p.cfg.SyncOnAppend {
		if err := active.sync(); err != nil {
			return fmt.Errorf("sync after replicated append: %w", err)
		}
	}

	p.broadcast()
	return nil
}

// AppendReplicatedBatch applies a run of leader-assigned records under one lock
// acquisition. It stops at the first record that does not follow the local
// head and reports how many were applied, so the caller can resume precisely.
func (p *Partition) AppendReplicatedBatch(recs []*Record) (applied int, err error) {
	for _, rec := range recs {
		if err := p.AppendReplicated(rec); err != nil {
			return applied, err
		}
		applied++
	}
	return applied, nil
}

// AppendBatch writes several records under a single lock acquisition and a
// single broadcast. Group fan-out and cross-region replication both replay
// batches; doing them one at a time would multiply lock traffic by the fan-out
// factor.
func (p *Partition) AppendBatch(recs []*Record) ([]uint64, error) {
	if len(recs) == 0 {
		return nil, nil
	}

	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		return nil, ErrClosed
	}
	if p.enforceLeadership && !p.isLeader {
		p.mu.Unlock()
		return nil, fmt.Errorf("%w: %s/%d", ErrNotPartitionLeader, p.Topic, p.ID)
	}

	seqs := make([]uint64, 0, len(recs))
	now := time.Now().UnixNano()

	for _, rec := range recs {
		if rec.Flags&FlagEphemeral != 0 {
			p.mu.Unlock()
			return nil, errors.New("stream: cannot persist an ephemeral record")
		}
		seq := p.nextSeq
		rec.Seq = seq
		rec.Epoch = p.curEpoch
		if rec.Timestamp == 0 {
			rec.Timestamp = now
		}
		encoded, err := rec.encode()
		if err != nil {
			p.mu.Unlock()
			return nil, err
		}

		active := p.segments[len(p.segments)-1]
		if active.bytes()+int64(len(encoded)) > p.cfg.SegmentBytes && !active.isEmpty() {
			if active, err = p.rollLocked(seq); err != nil {
				p.mu.Unlock()
				return nil, err
			}
		}
		if err := active.append(encoded, seq); err != nil {
			p.mu.Unlock()
			return nil, err
		}
		p.nextSeq = seq + 1
		p.byteCount.Add(int64(len(encoded)))
		seqs = append(seqs, seq)
	}

	active := p.segments[len(p.segments)-1]
	p.mu.Unlock()

	p.appendCount.Add(uint64(len(recs)))
	if p.cfg.SyncOnAppend {
		if err := active.sync(); err != nil {
			return nil, fmt.Errorf("sync after batch: %w", err)
		}
	}
	p.broadcast()
	return seqs, nil
}

// BecomeLeader opens a new leadership term for this partition.
//
// Every record appended from now on is stamped with epoch, and the sequence at
// which the term began is checkpointed. A node that takes leadership without
// calling this would write records indistinguishable from the previous
// leader's — which is exactly the divergence epochs exist to prevent, so the
// call is mandatory rather than an optimisation.
//
// The epoch must be strictly greater than the current one. Callers get it from
// the metadata layer, which is the only thing that can decide leadership
// without two nodes reaching the same number.
func (p *Partition) BecomeLeader(epoch uint32) error {
	if epoch == UndefinedEpoch {
		return errors.New("stream: leader epoch must be non-zero")
	}

	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed {
		return ErrClosed
	}
	if epoch <= p.curEpoch {
		return fmt.Errorf("stream: leader epoch %d is not newer than current %d", epoch, p.curEpoch)
	}
	if err := p.epochs.assign(epoch, p.nextSeq); err != nil {
		return err
	}
	p.curEpoch = epoch
	p.isLeader = true
	return nil
}

// ReassertLeadership re-enables writes for a term this partition already holds.
//
// It exists because resignation and promotion are driven by two different
// signals that can disagree briefly: a node that resigns on a transient view of
// the metadata and then sees itself leading the *same* epoch again would
// otherwise be stuck. BecomeLeader rightly refuses to reopen a term — that
// would rewrite epoch history — so re-asserting is a separate, narrower
// operation that touches nothing but the write guard.
//
// It fails if the epoch is not the one this partition is actually on, which is
// what keeps it from becoming a way to skip the epoch check.
func (p *Partition) ReassertLeadership(epoch uint32) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed {
		return ErrClosed
	}
	if epoch != p.curEpoch {
		return fmt.Errorf("stream: cannot reassert epoch %d, partition is on %d", epoch, p.curEpoch)
	}
	p.isLeader = true
	return nil
}

// Resign gives up leadership of this partition.
//
// Called when the control plane moves leadership elsewhere. It does not touch
// the epoch history: the records written under the old term are still valid
// history, and the next leader opens a strictly newer epoch anyway. All this
// does is stop accepting local writes — which is the entire point, because the
// node that takes over is about to start assigning the same sequences.
func (p *Partition) Resign() {
	p.mu.Lock()
	p.isLeader = false
	p.mu.Unlock()
}

// IsLeader reports whether this node may write to the partition.
func (p *Partition) IsLeader() bool {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return !p.enforceLeadership || p.isLeader
}

// EnforceLeadership turns the write guard on or off for this partition.
func (p *Partition) EnforceLeadership(on bool) {
	p.mu.Lock()
	p.enforceLeadership = on
	p.mu.Unlock()
}

// LeaderEpoch returns the term this partition is writing under, and the
// sequence at which it began.
func (p *Partition) LeaderEpoch() (uint32, uint64) {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.epochs.latest()
}

// EndOffsetForEpoch reports where the given epoch stopped being authoritative
// on this node — the leader side of a follower's truncation query.
//
// The returned epoch is the one that actually covers the answer, which may be
// older than the one asked about. A returned epoch of UndefinedEpoch means this
// node cannot place the query in its history and the follower must re-fetch
// from FirstSeq rather than truncate on a guess.
func (p *Partition) EndOffsetForEpoch(epoch uint32) (uint32, uint64) {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.epochs.endOffsetFor(epoch, p.nextSeq)
}

// EpochHistory returns this partition's leadership history, for the admin API
// and for tests.
func (p *Partition) EpochHistory() []epochEntry {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.epochs.snapshot()
}

// TruncateTo discards every record with Seq >= seq, so the log ends at seq-1.
//
// This is the destructive half of epoch reconciliation. It exists for exactly
// one caller: a follower that has learned, from a new leader's epoch history,
// that its tail was written under a term the cluster never committed. Those
// records were accepted by a leader that died before replicating them, so no
// publisher was ever told they were durable — discarding them loses nothing a
// client believes it has, and keeping them would fork the log.
//
// Truncating at or below FirstSeq is refused: that is not reconciliation, it is
// deleting the partition, and no epoch disagreement can legitimately ask for it.
func (p *Partition) TruncateTo(seq uint64) (removed uint64, err error) {
	p.mu.Lock()
	defer p.mu.Unlock()

	if p.closed {
		return 0, ErrClosed
	}
	if seq >= p.nextSeq {
		return 0, nil // nothing past this point
	}
	if seq <= p.firstSeq {
		return 0, fmt.Errorf("stream: refusing to truncate partition %s/%d to %d, at or below first sequence %d",
			p.Topic, p.ID, seq, p.firstSeq)
	}

	removed = p.nextSeq - seq

	// Drop whole segments that begin at or after seq, newest first.
	for len(p.segments) > 1 {
		last := p.segments[len(p.segments)-1]
		if last.baseSeq < seq {
			break
		}
		p.byteCount.Add(-last.bytes())
		if err := last.remove(); err != nil {
			return 0, fmt.Errorf("remove truncated segment: %w", err)
		}
		p.segments = p.segments[:len(p.segments)-1]
	}

	// The segment that now ends the log may be sealed; truncating into it means
	// it becomes the active one again.
	active := p.segments[len(p.segments)-1]
	if err := active.reopenWritable(); err != nil {
		return 0, err
	}
	before := active.bytes()
	if _, err := active.truncateTo(seq); err != nil {
		return 0, err
	}
	p.byteCount.Add(active.bytes() - before)

	p.nextSeq = seq
	if err := p.epochs.truncateFromSeq(seq); err != nil {
		return 0, err
	}
	p.curEpoch, _ = p.epochs.latest()

	// Readers blocked past the new end must be woken, or a tailing consumer
	// waits forever for records that no longer exist.
	// broadcast takes only notifyMu, so calling it under p.mu is safe.
	p.broadcast()
	return removed, nil
}

// rollLocked seals the active segment and starts a new one at baseSeq.
// Caller must hold p.mu.
func (p *Partition) rollLocked(baseSeq uint64) (*segment, error) {
	old := p.segments[len(p.segments)-1]
	if err := old.flush(); err != nil {
		return nil, fmt.Errorf("flush before roll: %w", err)
	}
	// Sealing is durable-by-fsync: once a segment stops accepting writes we
	// want it on disk before the new one starts taking traffic, so a crash
	// cannot leave a hole between two segments.
	if err := old.sync(); err != nil {
		return nil, fmt.Errorf("sync before roll: %w", err)
	}

	seg, err := createSegment(p.dir, baseSeq, p.cfg.IndexInterval)
	if err != nil {
		return nil, err
	}
	p.segments = append(p.segments, seg)
	return seg, nil
}

// Read returns up to maxRecords records with Seq >= fromSeq, walking across
// segment boundaries as needed. It never blocks: a caller at the head of the
// log receives an empty slice.
func (p *Partition) Read(fromSeq uint64, maxRecords int, maxBytes int64) ([]*Record, error) {
	if maxRecords <= 0 {
		maxRecords = 100
	}

	p.mu.RLock()
	if p.closed {
		p.mu.RUnlock()
		return nil, ErrClosed
	}
	// Sequence 0 never names a record, so a caller passing it means "from the
	// beginning" rather than "from a record that was removed". Clamping it is
	// correct: reporting truncation would claim history was lost when none
	// ever existed at that position.
	if fromSeq == 0 {
		fromSeq = p.firstSeq
	}
	if fromSeq < p.firstSeq {
		first := p.firstSeq
		p.mu.RUnlock()
		return nil, fmt.Errorf("%w: requested %d, oldest available %d", ErrSeqTruncated, fromSeq, first)
	}
	segs := make([]*segment, len(p.segments))
	copy(segs, p.segments)
	p.mu.RUnlock()

	var out []*Record
	var bytesRead int64

	for i, seg := range segs {
		// Skip segments that end before fromSeq. The last segment is always a
		// candidate because its lastSeq moves as writes land.
		if i < len(segs)-1 && seg.lastSequence() < fromSeq {
			continue
		}
		remaining := maxRecords - len(out)
		if remaining <= 0 {
			break
		}
		var budget int64
		if maxBytes > 0 {
			budget = maxBytes - bytesRead
			if budget <= 0 {
				break
			}
		}

		recs, err := seg.read(fromSeq, remaining, budget)
		if err != nil {
			// A corrupt tail in a sealed segment must not hide the healthy
			// prefix — return what we have alongside the error so the caller
			// can serve partial history and alert.
			return out, err
		}
		for _, r := range recs {
			out = append(out, r)
			bytesRead += int64(r.encodedSize())
		}
		if len(recs) > 0 {
			fromSeq = recs[len(recs)-1].Seq + 1
		}
	}
	return out, nil
}

// ReadFrom is Read with a byte budget derived from a sane default.
func (p *Partition) ReadFrom(fromSeq uint64, maxRecords int) ([]*Record, error) {
	return p.Read(fromSeq, maxRecords, 4<<20)
}

// Tail streams records starting at fromSeq, blocking until new records arrive.
// It returns when ctx is cancelled or the partition closes.
//
// This is the primitive behind an open chat window: the client's cursor sits at
// the head and each append wakes it immediately, with no polling interval to
// trade off against latency.
func (p *Partition) Tail(ctx context.Context, fromSeq uint64, batch int, fn func([]*Record) error) error {
	if batch <= 0 {
		batch = 64
	}
	for {
		if err := ctx.Err(); err != nil {
			return err
		}

		// Register for the wake-up *before* reading. Reading first would race:
		// an append landing between the read and the registration would be
		// missed, and the reader would sleep despite data being available.
		wake := p.waitChan()

		recs, err := p.Read(fromSeq, batch, 4<<20)
		if err != nil {
			return err
		}
		if len(recs) > 0 {
			if err := fn(recs); err != nil {
				return err
			}
			fromSeq = recs[len(recs)-1].Seq + 1
			continue // drain fully before sleeping
		}

		select {
		case <-wake:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

// NotifyChan returns a channel that is closed when the partition next accepts
// a record.
//
// Consumers that run their own read loop — rather than using Tail — must
// acquire this channel *before* reading, so that an append landing between the
// read and the wait is not missed. The channel is single-use: acquire a fresh
// one on each iteration.
func (p *Partition) NotifyChan() <-chan struct{} { return p.waitChan() }

func (p *Partition) waitChan() <-chan struct{} {
	p.notifyMu.Lock()
	defer p.notifyMu.Unlock()
	return p.notify
}

func (p *Partition) broadcast() {
	p.notifyMu.Lock()
	close(p.notify)
	p.notify = make(chan struct{})
	p.notifyMu.Unlock()
}

// NextSeq returns the sequence the next append will receive. NextSeq-1 is the
// newest committed record.
func (p *Partition) NextSeq() uint64 {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.nextSeq
}

// FirstSeq returns the oldest sequence still readable.
func (p *Partition) FirstSeq() uint64 {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.firstSeq
}

// Bytes returns the partition's total on-disk size.
func (p *Partition) Bytes() int64 { return p.byteCount.Load() }

// Appends returns the lifetime append count since process start.
func (p *Partition) Appends() uint64 { return p.appendCount.Load() }

// Sync flushes and fsyncs the active segment.
func (p *Partition) Sync() error {
	p.mu.RLock()
	if p.closed || len(p.segments) == 0 {
		p.mu.RUnlock()
		return nil
	}
	active := p.segments[len(p.segments)-1]
	p.mu.RUnlock()
	return active.sync()
}

// EnforceRetention drops whole sealed segments that fall outside the retention
// policy. It never touches the active segment, and it never deletes a partial
// segment — retention is coarse on purpose so it costs an unlink rather than a
// rewrite.
func (p *Partition) EnforceRetention() (removed int, err error) {
	if p.cfg.RetentionBytes <= 0 && p.cfg.RetentionAge <= 0 {
		return 0, nil
	}

	cutoff := int64(0)
	if p.cfg.RetentionAge > 0 {
		cutoff = time.Now().Add(-p.cfg.RetentionAge).UnixNano()
	}

	for {
		p.mu.Lock()
		if p.closed || len(p.segments) <= 1 {
			p.mu.Unlock()
			return removed, nil
		}

		oldest := p.segments[0]
		drop := false

		if p.cfg.RetentionBytes > 0 && p.byteCount.Load() > p.cfg.RetentionBytes {
			drop = true
		}
		if !drop && cutoff > 0 {
			// The whole segment must be older than the cutoff. Checking the
			// *next* segment's base is not enough; use this segment's newest
			// record, approximated by the following segment's first timestamp.
			next := p.segments[1]
			if ts := next.firstTimestamp(); ts > 0 && ts < cutoff {
				drop = true
			}
		}
		if !drop {
			p.mu.Unlock()
			return removed, nil
		}

		freed := oldest.bytes()
		p.segments = p.segments[1:]
		p.firstSeq = p.segments[0].baseSeq
		// Epoch history for records that no longer exist is dead weight, but the
		// entry covering the new firstSeq must survive — a follower still needs
		// to be able to place the oldest record we retain.
		if err := p.epochs.truncateBeforeSeq(p.firstSeq); err != nil {
			p.mu.Unlock()
			return removed, err
		}
		p.mu.Unlock()

		if err := oldest.remove(); err != nil {
			return removed, fmt.Errorf("remove segment %d: %w", oldest.baseSeq, err)
		}
		p.byteCount.Add(-freed)
		removed++
	}
}

// Close flushes and releases all segment file handles.
func (p *Partition) Close() error {
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		return nil
	}
	p.closed = true
	segs := p.segments
	p.segments = nil
	p.mu.Unlock()

	// Wake tailers so they observe ErrClosed instead of hanging.
	p.broadcast()

	var firstErr error
	for _, s := range segs {
		if err := s.close(); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}
