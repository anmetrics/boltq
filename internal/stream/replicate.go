package stream

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"hash/crc32"
	"io"
	"sync"
	"time"
)

// This file is the seam between the log and whatever replicates it. The log
// knows nothing about transports, node identity or quorum; it exposes exactly
// two things — a way to move records over a wire, and a hook that lets an
// append block until someone else has the record too.

// EncodeRecord serialises a record for transport, using the same framing and
// checksum as the on-disk format.
//
// Reusing the disk encoding is deliberate: a record that survives the network
// is byte-identical to the one the leader wrote, so a follower cannot end up
// with a subtly different representation of the same message.
func EncodeRecord(r *Record) ([]byte, error) { return r.encode() }

// DecodeRecord parses a frame produced by EncodeRecord, verifying the checksum.
func DecodeRecord(frame []byte) (*Record, error) {
	if len(frame) < recordFrameHeaderSize {
		return nil, ErrCorruptRecord
	}
	checksum := binary.LittleEndian.Uint32(frame[0:4])
	bodyLen := binary.LittleEndian.Uint32(frame[4:8])

	if bodyLen < recordBodyHeaderSize || bodyLen > MaxRecordSize {
		return nil, fmt.Errorf("%w: body length %d", ErrCorruptRecord, bodyLen)
	}
	if len(frame) < recordFrameHeaderSize+int(bodyLen) {
		return nil, fmt.Errorf("%w: frame shorter than its declared body", ErrCorruptRecord)
	}

	body := frame[recordFrameHeaderSize : recordFrameHeaderSize+int(bodyLen)]
	if crc32.ChecksumIEEE(body) != checksum {
		return nil, ErrCorruptRecord
	}
	return decodeRecordBody(body)
}

// ReadRecordFrame reads one encoded record from r and returns the raw frame,
// so a transport can forward bytes without decoding them.
func ReadRecordFrame(r io.Reader) ([]byte, error) {
	var header [recordFrameHeaderSize]byte
	if _, err := io.ReadFull(r, header[:]); err != nil {
		return nil, err
	}
	bodyLen := binary.LittleEndian.Uint32(header[4:8])
	if bodyLen < recordBodyHeaderSize || bodyLen > MaxRecordSize {
		return nil, fmt.Errorf("%w: body length %d", ErrCorruptRecord, bodyLen)
	}

	frame := make([]byte, recordFrameHeaderSize+int(bodyLen))
	copy(frame, header[:])
	if _, err := io.ReadFull(r, frame[recordFrameHeaderSize:]); err != nil {
		return nil, err
	}
	return frame, nil
}

var (
	// ErrNotEnoughReplicas means an append could not reach the durability the
	// configured acknowledgement mode requires.
	ErrNotEnoughReplicas = errors.New("stream: not enough in-sync replicas")
	// ErrAckTimeout means replicas did not confirm within the deadline.
	ErrAckTimeout = errors.New("stream: timed out waiting for replica acknowledgement")
)

// AckWaiter blocks until a record is durable somewhere other than this node.
//
// It exists so the log can offer stronger durability than "it reached my page
// cache" without knowing how replication works. A nil waiter means single-node
// operation, which is the default and changes nothing.
type AckWaiter interface {
	// WaitFor returns once seq has been acknowledged by enough replicas of the
	// given partition, or fails if that cannot be achieved.
	WaitFor(ctx context.Context, topic string, partition int32, seq uint64) error
}

// SetAckWaiter installs (or clears, with nil) the durability hook.
//
// Installing a waiter changes what a successful Append means: before, it meant
// the record was in this node's page cache; after, it means the configured
// number of replicas hold it. That is the whole point, and it is also why this
// is explicit rather than automatic.
func (l *Log) SetAckWaiter(w AckWaiter) {
	l.mu.Lock()
	l.ackWaiter = w
	l.mu.Unlock()
}

// ackWaiterFor returns the installed waiter, if any.
func (l *Log) ackWaiterFor() AckWaiter {
	l.mu.RLock()
	defer l.mu.RUnlock()
	return l.ackWaiter
}

// AppendContext appends a record and, when an AckWaiter is installed, waits
// for it to be replicated before returning.
//
// The wait happens after the local append and outside the partition lock, so
// replication latency never blocks other writers to the same partition — it
// only delays the acknowledgement to this one publisher.
func (l *Log) AppendContext(ctx context.Context, topic string, rec *Record) (AppendResult, error) {
	res, pid, err := l.appendLocal(topic, rec)
	if errors.Is(err, ErrNotPartitionLeader) {
		if f := l.forwarderFor(); f != nil {
			// The leader owns this write end to end, replica acknowledgement
			// included. Waiting locally as well would block on a quorum this
			// node is not part of and cannot observe.
			return f.Forward(ctx, topic, pid, rec)
		}
	}
	if err != nil {
		return res, err
	}

	w := l.ackWaiterFor()
	if w == nil {
		return res, nil
	}
	if err := w.WaitFor(ctx, topic, res.Partition, res.Seq); err != nil {
		// The record is already durable locally and already visible to local
		// readers; it cannot be un-appended. Report the weaker durability
		// honestly and let the caller decide, rather than pretending the write
		// failed when the data exists.
		return res, fmt.Errorf("appended locally but not replicated: %w", err)
	}
	return res, nil
}

// ReplicaState describes one follower's position in a partition.
type ReplicaState struct {
	NodeID string `json:"node_id"`
	// AckedSeq is the highest sequence the follower has confirmed durable.
	AckedSeq uint64 `json:"acked_seq"`
	// Lag is the leader's next sequence minus AckedSeq.
	Lag uint64 `json:"lag"`
	// InSync reports whether the follower is close enough to count toward
	// quorum. A follower that falls too far behind is excluded so a permanently
	// slow node cannot hold up every write.
	InSync bool `json:"in_sync"`
	// LastSeen is when the follower last acknowledged anything.
	LastSeen time.Time `json:"last_seen"`
}

// PartitionReplication summarises replication for one partition.
type PartitionReplication struct {
	Topic     string         `json:"topic"`
	Partition int32          `json:"partition"`
	LeaderSeq uint64         `json:"leader_seq"`
	Replicas  []ReplicaState `json:"replicas"`
	InSync    int            `json:"in_sync"`
	// HighWatermark is the highest sequence held by a quorum. Everything at or
	// below it survives the loss of this node.
	HighWatermark uint64 `json:"high_watermark"`
	// LeaderEpoch is the term this node is writing the partition under, and
	// EpochStartSeq where that term began. Zero means the node has never been
	// promoted — worth alerting on for a node configured as a leader, since it
	// means its records carry nothing a follower can reconcile against.
	LeaderEpoch   uint32 `json:"leader_epoch"`
	EpochStartSeq uint64 `json:"epoch_start_seq"`
}

// partitionReader is the subset of Partition the replication layer needs on the
// leader side. Declaring it here keeps internal/replication from depending on
// concrete partition internals.
type partitionReader interface {
	ReadFrom(fromSeq uint64, limit int) ([]*Record, error)
	NextSeq() uint64
	FirstSeq() uint64
	NotifyChan() <-chan struct{}

	// The epoch trio is what makes failover safe. A leader answers
	// EndOffsetForEpoch; a follower reads LeaderEpoch to know what to ask
	// about, and calls TruncateTo when the answer says its tail is orphaned.
	LeaderEpoch() (uint32, uint64)
	EndOffsetForEpoch(epoch uint32) (uint32, uint64)
	TruncateTo(seq uint64) (uint64, error)
}

// PartitionFor resolves a topic/partition pair to something a replicator can
// stream from.
func (l *Log) PartitionFor(topic string, partition int32) (partitionReader, error) {
	t, err := l.Topic(topic)
	if err != nil {
		return nil, err
	}
	return t.Partition(partition)
}

// ApplyReplicated writes a leader-assigned record into the local log, creating
// the topic if this node has not seen it before.
//
// Topic auto-creation matters on a follower: it may join after the leader has
// already created topics, and refusing unknown topics would leave it
// permanently unable to catch up.
func (l *Log) ApplyReplicated(topic string, partitionCount int32, partition int32, rec *Record) error {
	t, err := l.Topic(topic)
	if err != nil {
		if partitionCount <= 0 {
			partitionCount = l.defaultCfg.Partitions
		}
		if t, err = l.CreateTopic(topic, TopicConfig{Partitions: partitionCount}); err != nil {
			return fmt.Errorf("create replicated topic %s: %w", topic, err)
		}
	}
	p, err := t.Partition(partition)
	if err != nil {
		return err
	}
	return p.AppendReplicated(rec)
}

// followerCursor tracks where each follower is, per partition.
type followerCursor struct {
	mu    sync.RWMutex
	acked map[string]uint64 // nodeID -> acked seq
	seen  map[string]time.Time
}

func newFollowerCursor() *followerCursor {
	return &followerCursor{
		acked: make(map[string]uint64),
		seen:  make(map[string]time.Time),
	}
}

func (f *followerCursor) ack(nodeID string, seq uint64) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if cur, ok := f.acked[nodeID]; !ok || seq > cur {
		f.acked[nodeID] = seq
	}
	f.seen[nodeID] = time.Now()
}

func (f *followerCursor) drop(nodeID string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.acked, nodeID)
	delete(f.seen, nodeID)
}

func (f *followerCursor) snapshot() (map[string]uint64, map[string]time.Time) {
	f.mu.RLock()
	defer f.mu.RUnlock()
	a := make(map[string]uint64, len(f.acked))
	s := make(map[string]time.Time, len(f.seen))
	for k, v := range f.acked {
		a[k] = v
	}
	for k, v := range f.seen {
		s[k] = v
	}
	return a, s
}

// countAtLeast returns how many followers have acknowledged at least seq.
func (f *followerCursor) countAtLeast(seq uint64) int {
	f.mu.RLock()
	defer f.mu.RUnlock()
	n := 0
	for _, v := range f.acked {
		if v >= seq {
			n++
		}
	}
	return n
}
