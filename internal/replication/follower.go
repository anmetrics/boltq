package replication

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"log"
	"net"
	"sort"
	"sync"
	"time"

	"github.com/boltq/boltq/internal/stream"
)

// FollowerConfig configures the replication client.
type FollowerConfig struct {
	// LeaderAddr is the leader's replication listener.
	LeaderAddr string
	// NodeID identifies this follower to the leader. It must be stable — the
	// leader keys quorum accounting by it, so a node that changes identity on
	// restart appears as a new replica while the old one lingers.
	NodeID string
	// Secret must match the leader's.
	Secret string

	// Topics and partitions to replicate. Empty Topics means "discover from
	// the leader as records arrive", which is not yet supported; list them.
	Assignments []Assignment

	// AckInterval is how often the follower reports its position. More often
	// means tighter quorum latency and more traffic.
	AckInterval time.Duration
	// ReconnectBackoff is the initial delay between reconnect attempts; it
	// doubles up to ReconnectMax.
	ReconnectBackoff time.Duration
	ReconnectMax     time.Duration
	// DialTimeout bounds one connection attempt.
	DialTimeout time.Duration
	// SyncOnApply fsyncs after applying a batch. Turning this on is what makes
	// a follower's acknowledgement mean "on disk" rather than "in page cache";
	// with it off, losing power to the whole cluster at once can still lose
	// recent records everywhere.
	SyncOnApply bool
}

// Assignment names a partition to replicate.
type Assignment struct {
	Topic          string
	Partition      int32
	PartitionCount int32
}

func (c *FollowerConfig) applyDefaults() {
	if c.AckInterval <= 0 {
		c.AckInterval = 200 * time.Millisecond
	}
	if c.ReconnectBackoff <= 0 {
		c.ReconnectBackoff = 500 * time.Millisecond
	}
	if c.ReconnectMax <= 0 {
		c.ReconnectMax = 30 * time.Second
	}
	if c.DialTimeout <= 0 {
		c.DialTimeout = 5 * time.Second
	}
}

// FollowerStats counts follower activity.
type FollowerStats struct {
	Connected      bool              `json:"connected"`
	LeaderNodeID   string            `json:"leader_node_id"`
	RecordsApplied uint64            `json:"records_applied"`
	BatchesApplied uint64            `json:"batches_applied"`
	Gaps           uint64            `json:"gaps"`
	Reconnects     uint64            `json:"reconnects"`
	Errors         uint64            `json:"errors"`
	LastAppliedAt  time.Time         `json:"last_applied_at"`
	PartitionHeads map[string]uint64 `json:"partition_heads"`

	// Truncations counts epoch reconciliations that discarded a local tail, and
	// RecordsTruncated the records they removed. A nonzero value is normal
	// after a failover and alarming at any other time — it means this node had
	// accepted records the cluster never committed.
	Truncations      uint64 `json:"truncations"`
	RecordsTruncated uint64 `json:"records_truncated"`
	// EpochConflicts counts times the leader could not place this follower's
	// epoch at all. Replication for that partition stops until an operator
	// intervenes; alert on it.
	EpochConflicts uint64 `json:"epoch_conflicts"`
}

// Follower replicates partitions from a leader into the local log.
type Follower struct {
	cfg FollowerConfig
	log *stream.Log

	mu    sync.RWMutex
	stats FollowerStats
	// heads is the local next-sequence per "topic/partition", used for acks.
	heads map[string]uint64

	// conn is the live leader connection, if any. Close needs it for the same
	// reason the leader does: a goroutine blocked reading a socket does not
	// notice a cancelled context.
	conn net.Conn

	ctx    context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup
	once   sync.Once
}

// NewFollower creates a replication follower.
func NewFollower(l *stream.Log, cfg FollowerConfig) (*Follower, error) {
	if l == nil {
		return nil, errors.New("replication: a stream log is required")
	}
	if cfg.LeaderAddr == "" {
		return nil, errors.New("replication: leader address is required")
	}
	if cfg.NodeID == "" {
		return nil, errors.New("replication: follower node id is required")
	}
	cfg.applyDefaults()

	ctx, cancel := context.WithCancel(context.Background())
	return &Follower{
		cfg:    cfg,
		log:    l,
		heads:  make(map[string]uint64),
		ctx:    ctx,
		cancel: cancel,
	}, nil
}

// Start connects to the leader and keeps replicating until Close.
func (f *Follower) Start() {
	f.wg.Add(1)
	go func() {
		defer f.wg.Done()
		f.runLoop()
	}()
}

// runLoop reconnects with exponential backoff for the process's lifetime.
func (f *Follower) runLoop() {
	backoff := f.cfg.ReconnectBackoff

	for {
		if f.ctx.Err() != nil {
			return
		}

		err := f.session()
		if f.ctx.Err() != nil {
			return
		}
		if err != nil {
			f.bump(func(s *FollowerStats) { s.Errors++ })
			log.Printf("[replication] follower session ended: %v (retry in %s)", err, backoff)
		}

		f.mu.Lock()
		f.stats.Connected = false
		f.stats.Reconnects++
		f.mu.Unlock()

		select {
		case <-time.After(backoff):
		case <-f.ctx.Done():
			return
		}
		backoff *= 2
		if backoff > f.cfg.ReconnectMax {
			backoff = f.cfg.ReconnectMax
		}
	}
}

// session runs one connection to the leader from handshake to disconnect.
func (f *Follower) session() error {
	conn, err := net.DialTimeout("tcp", f.cfg.LeaderAddr, f.cfg.DialTimeout)
	if err != nil {
		return fmt.Errorf("dial leader: %w", err)
	}

	f.mu.Lock()
	if f.ctx.Err() != nil {
		f.mu.Unlock()
		conn.Close()
		return f.ctx.Err()
	}
	f.conn = conn
	f.mu.Unlock()

	defer func() {
		f.mu.Lock()
		if f.conn == conn {
			f.conn = nil
		}
		f.mu.Unlock()
		conn.Close()
	}()

	reader := bufio.NewReaderSize(conn, 256*1024)
	writer := bufio.NewWriterSize(conn, 64*1024)

	hello := append([]byte(f.cfg.NodeID), 0)
	hello = append(hello, f.cfg.Secret...)
	if err := writeFrame(writer, MsgHello, hello); err != nil {
		return err
	}
	if err := writer.Flush(); err != nil {
		return err
	}

	conn.SetReadDeadline(time.Now().Add(10 * time.Second))
	typ, payload, err := readFrame(reader)
	if err != nil {
		return fmt.Errorf("handshake: %w", err)
	}
	if typ == MsgError {
		return fmt.Errorf("leader rejected us: %s", payload)
	}
	if typ != MsgHello {
		return fmt.Errorf("%w: expected hello, got 0x%02x", ErrProtocol, typ)
	}
	conn.SetReadDeadline(time.Time{})

	f.mu.Lock()
	f.stats.Connected = true
	f.stats.LeaderNodeID = string(payload)
	f.mu.Unlock()
	log.Printf("[replication] follower %s connected to leader %s", f.cfg.NodeID, payload)

	var writeMu sync.Mutex

	// Reconcile against the leader's epoch history *before* fetching anything.
	//
	// Order matters: once records start arriving, an orphaned local tail would
	// make every one of them look like a gap, and the follower would re-fetch
	// forever without ever removing the records causing it.
	for _, a := range f.cfg.Assignments {
		if err := f.reconcileEpoch(a, reader, writer, &writeMu); err != nil {
			return err
		}
	}

	// Request each assignment from wherever the local log actually ends. A
	// follower that restarts mid-stream resumes precisely rather than
	// re-fetching from the beginning.
	for _, a := range f.cfg.Assignments {
		from := f.localNextSeq(a)
		req := fetchRequest{
			Topic: a.Topic, Partition: a.Partition,
			PartitionCount: a.PartitionCount, FromSeq: from,
		}
		writeMu.Lock()
		err := writeFrame(writer, MsgFetch, req.encode())
		if err == nil {
			err = writer.Flush()
		}
		writeMu.Unlock()
		if err != nil {
			return err
		}
	}

	sessionCtx, cancel := context.WithCancel(f.ctx)
	defer cancel()

	// Acks are batched on a timer rather than sent per record: a per-record ack
	// would double the message count on the replication link for no gain, since
	// the leader only cares about the high-water mark.
	go f.ackLoop(sessionCtx, writer, &writeMu)

	for {
		typ, payload, err := readFrame(reader)
		if err != nil {
			return err
		}

		switch typ {
		case MsgRecords:
			if err := f.applyBatch(payload, writer, &writeMu); err != nil {
				return err
			}

		case MsgError:
			// The leader reports retention gaps this way; the stream continues
			// from wherever it resumed, so this is informational, not fatal.
			log.Printf("[replication] leader reported: %s", payload)
			f.bump(func(s *FollowerStats) { s.Gaps++ })

		case MsgPing:
			writeMu.Lock()
			writeFrame(writer, MsgPong, nil)
			writer.Flush()
			writeMu.Unlock()

		case MsgPong:
			// liveness only

		default:
			return fmt.Errorf("%w: unexpected type 0x%02x", ErrProtocol, typ)
		}
	}
}

// ErrUnplaceableEpoch means the leader could not say where this follower's last
// leader epoch ended. The follower holds records under a term the leader has no
// record of, and neither keeping them (divergence) nor discarding them
// (silent data loss on a guess) is safe without an operator deciding.
var ErrUnplaceableEpoch = errors.New("replication: leader cannot place follower's leader epoch")

// reconcileEpoch asks the leader where this follower's last epoch ended and
// truncates any local tail written past that point.
//
// The tail being discarded was accepted by a leader that died before
// replicating it, so no publisher was ever told it was durable. Keeping it
// would give two nodes different records at the same sequence — the one failure
// a replicated log must never have.
func (f *Follower) reconcileEpoch(a Assignment, r *bufio.Reader, w *bufio.Writer, mu *sync.Mutex) error {
	p, err := f.log.PartitionFor(a.Topic, a.Partition)
	if err != nil {
		return nil // nothing local yet; the first fetch creates it
	}
	localNext := p.NextSeq()
	if localNext <= 1 {
		return nil // empty log; nothing can be orphaned
	}
	localEpoch, _ := p.LeaderEpoch()

	mu.Lock()
	req := epochRequest{Topic: a.Topic, Partition: a.Partition, Epoch: localEpoch}
	err = writeFrame(w, MsgEpochRequest, req.encode())
	if err == nil {
		err = w.Flush()
	}
	mu.Unlock()
	if err != nil {
		return err
	}

	resp, err := f.awaitEpochResponse(r, a)
	if err != nil {
		return err
	}

	if resp.Epoch == stream.UndefinedEpoch {
		if localEpoch == stream.UndefinedEpoch {
			// Neither side has epoch history — a log written before epochs
			// existed, or a leader that has never been promoted. There is
			// nothing to reconcile, and refusing here would strand every
			// deployment upgrading into this version.
			return nil
		}
		f.bump(func(s *FollowerStats) { s.EpochConflicts++ })
		return fmt.Errorf("%w: %s/%d at epoch %d", ErrUnplaceableEpoch, a.Topic, a.Partition, localEpoch)
	}

	if resp.EndSeq >= localNext {
		return nil // our tail is a prefix of the leader's; keep everything
	}

	removed, err := p.TruncateTo(resp.EndSeq)
	if err != nil {
		return fmt.Errorf("truncate %s/%d to %d: %w", a.Topic, a.Partition, resp.EndSeq, err)
	}
	if removed > 0 {
		log.Printf("[replication] %s/%d: truncated %d record(s) past epoch %d end %d (local epoch %d)",
			a.Topic, a.Partition, removed, resp.Epoch, resp.EndSeq, localEpoch)
		f.bump(func(s *FollowerStats) {
			s.RecordsTruncated += removed
			s.Truncations++
		})
	}
	return nil
}

// awaitEpochResponse reads frames until the answer for a arrives, tolerating
// the keepalive and error traffic the leader may interleave.
func (f *Follower) awaitEpochResponse(r *bufio.Reader, a Assignment) (epochResponse, error) {
	deadline := time.Now().Add(f.cfg.DialTimeout + 10*time.Second)
	for {
		if time.Now().After(deadline) {
			return epochResponse{}, fmt.Errorf("%w: no epoch response for %s/%d",
				ErrProtocol, a.Topic, a.Partition)
		}
		typ, payload, err := readFrame(r)
		if err != nil {
			return epochResponse{}, err
		}
		switch typ {
		case MsgEpochResponse:
			resp, err := decodeEpochResponse(payload)
			if err != nil {
				return epochResponse{}, err
			}
			if resp.Topic != a.Topic || resp.Partition != a.Partition {
				return epochResponse{}, fmt.Errorf("%w: epoch response for %s/%d, expected %s/%d",
					ErrProtocol, resp.Topic, resp.Partition, a.Topic, a.Partition)
			}
			return resp, nil
		case MsgError:
			return epochResponse{}, fmt.Errorf("leader refused epoch query: %s", payload)
		case MsgPing, MsgPong:
			// keepalive; keep waiting
		default:
			return epochResponse{}, fmt.Errorf("%w: unexpected type 0x%02x during epoch query", ErrProtocol, typ)
		}
	}
}

// applyBatch writes a received batch into the local log.
func (f *Follower) applyBatch(payload []byte, w *bufio.Writer, mu *sync.Mutex) error {
	header, offset, err := decodeRecordsHeader(payload)
	if err != nil {
		return err
	}

	var assignment Assignment
	for _, a := range f.cfg.Assignments {
		if a.Topic == header.Topic && a.Partition == header.Partition {
			assignment = a
			break
		}
	}

	body := payload[offset:]
	applied := 0

	for i := uint32(0); i < header.Count; i++ {
		frame, n, err := nextRecordFrame(body)
		if err != nil {
			return err
		}
		body = body[n:]

		rec, err := stream.DecodeRecord(frame)
		if err != nil {
			return fmt.Errorf("decode replicated record: %w", err)
		}

		err = f.log.ApplyReplicated(header.Topic, assignment.PartitionCount, header.Partition, rec)
		if errors.Is(err, stream.ErrReplicationGap) {
			// The local log does not continue from this record. Re-fetch from
			// the true local head rather than writing a hole; the leader
			// replaces the stream on a new fetch for the same partition.
			f.bump(func(s *FollowerStats) { s.Gaps++ })
			log.Printf("[replication] gap on %s/%d: %v — re-fetching",
				header.Topic, header.Partition, err)
			return f.refetch(assignment, header, w, mu)
		}
		if err != nil {
			return fmt.Errorf("apply replicated record: %w", err)
		}
		applied++
	}

	if f.cfg.SyncOnApply && applied > 0 {
		if p, err := f.log.PartitionFor(header.Topic, header.Partition); err == nil {
			if sp, ok := p.(interface{ Sync() error }); ok {
				if err := sp.Sync(); err != nil {
					// A follower that cannot fsync must not claim durability.
					return fmt.Errorf("sync replicated batch: %w", err)
				}
			}
		}
	}

	head := f.recordHead(header.Topic, header.Partition)
	f.mu.Lock()
	f.stats.RecordsApplied += uint64(applied)
	f.stats.BatchesApplied++
	f.stats.LastAppliedAt = time.Now()
	f.mu.Unlock()
	_ = head

	return nil
}

// refetch asks the leader to restart a partition stream at the local head.
func (f *Follower) refetch(a Assignment, h recordsHeader, w *bufio.Writer, mu *sync.Mutex) error {
	from := f.localNextSeq(Assignment{
		Topic: h.Topic, Partition: h.Partition, PartitionCount: a.PartitionCount,
	})
	req := fetchRequest{
		Topic: h.Topic, Partition: h.Partition,
		PartitionCount: a.PartitionCount, FromSeq: from,
	}
	mu.Lock()
	defer mu.Unlock()
	if err := writeFrame(w, MsgFetch, req.encode()); err != nil {
		return err
	}
	return w.Flush()
}

// nextRecordFrame slices one encoded record off the front of b.
func nextRecordFrame(b []byte) ([]byte, int, error) {
	const frameHeader = 8
	if len(b) < frameHeader {
		return nil, 0, fmt.Errorf("%w: truncated record frame", ErrProtocol)
	}
	bodyLen := int(uint32(b[4]) | uint32(b[5])<<8 | uint32(b[6])<<16 | uint32(b[7])<<24)
	total := frameHeader + bodyLen
	if bodyLen < 0 || total > len(b) {
		return nil, 0, fmt.Errorf("%w: record frame overruns batch", ErrProtocol)
	}
	return b[:total], total, nil
}

// localNextSeq returns where this follower's copy of a partition ends.
func (f *Follower) localNextSeq(a Assignment) uint64 {
	p, err := f.log.PartitionFor(a.Topic, a.Partition)
	if err != nil {
		return 1 // topic not created locally yet; start from the beginning
	}
	return p.NextSeq()
}

func (f *Follower) recordHead(topic string, partition int32) uint64 {
	next := f.localNextSeq(Assignment{Topic: topic, Partition: partition})
	f.mu.Lock()
	f.heads[cursorKey(topic, partition)] = next
	f.mu.Unlock()
	return next
}

// ackLoop reports the follower's position for every assignment on an interval.
func (f *Follower) ackLoop(ctx context.Context, w *bufio.Writer, mu *sync.Mutex) {
	ticker := time.NewTicker(f.cfg.AckInterval)
	defer ticker.Stop()

	sent := make(map[string]uint64)

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}

		for _, a := range f.cfg.Assignments {
			next := f.localNextSeq(a)
			if next == 0 {
				continue
			}
			// Ack the last applied sequence, not the next one.
			acked := next - 1
			key := cursorKey(a.Topic, a.Partition)
			if prev, ok := sent[key]; ok && prev >= acked {
				continue // nothing new to report
			}

			msg := ackMessage{Topic: a.Topic, Partition: a.Partition, Seq: acked}
			mu.Lock()
			err := writeFrame(w, MsgAck, msg.encode())
			if err == nil {
				err = w.Flush()
			}
			mu.Unlock()
			if err != nil {
				return
			}
			sent[key] = acked

			f.mu.Lock()
			f.heads[key] = next
			f.mu.Unlock()
		}
	}
}

func (f *Follower) bump(fn func(*FollowerStats)) {
	f.mu.Lock()
	fn(&f.stats)
	f.mu.Unlock()
}

// Stats returns a snapshot of follower counters.
func (f *Follower) Stats() FollowerStats {
	f.mu.RLock()
	defer f.mu.RUnlock()
	s := f.stats
	s.PartitionHeads = make(map[string]uint64, len(f.heads))
	for k, v := range f.heads {
		s.PartitionHeads[k] = v
	}
	return s
}

// Assignments returns the configured partitions, sorted.
func (f *Follower) Assignments() []Assignment {
	out := append([]Assignment(nil), f.cfg.Assignments...)
	sort.Slice(out, func(i, j int) bool {
		if out[i].Topic != out[j].Topic {
			return out[i].Topic < out[j].Topic
		}
		return out[i].Partition < out[j].Partition
	})
	return out
}

// Close stops replicating.
func (f *Follower) Close() {
	f.once.Do(func() {
		f.cancel()

		f.mu.Lock()
		conn := f.conn
		f.mu.Unlock()
		if conn != nil {
			conn.Close()
		}

		f.wg.Wait()
	})
}
