package stream

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"hash/fnv"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

var (
	// ErrNoSuchTopic is returned when reading a topic that was never created.
	ErrNoSuchTopic = errors.New("stream: no such topic")
	// ErrNoSuchPartition is returned for an out-of-range partition ID.
	ErrNoSuchPartition = errors.New("stream: no such partition")
	// ErrInvalidTopic is returned for a topic name that cannot be a directory.
	ErrInvalidTopic = errors.New("stream: invalid topic name")
)

// TopicConfig describes a stream topic.
type TopicConfig struct {
	// Partitions fixes the partition count. It cannot change after creation
	// without remapping keys, so pick with headroom — partitions are cheap
	// (two file handles each), resharding is not.
	Partitions int32
	Partition  PartitionConfig
}

// Topic is a set of partitions sharing a name and configuration.
type Topic struct {
	Name       string
	partitions []*Partition
	cfg        TopicConfig
}

// PartitionCount returns how many partitions the topic has.
func (t *Topic) PartitionCount() int32 { return int32(len(t.partitions)) }

// Partition returns the partition with the given ID.
func (t *Topic) Partition(id int32) (*Partition, error) {
	if id < 0 || int(id) >= len(t.partitions) {
		return nil, fmt.Errorf("%w: %s/%d", ErrNoSuchPartition, t.Name, id)
	}
	return t.partitions[id], nil
}

// Partitions returns every partition, ordered by ID.
func (t *Topic) Partitions() []*Partition { return t.partitions }

// PartitionForKey maps a key to a partition.
//
// The function is FNV-1a/64 modulo the partition count, chosen because it is
// trivially reimplementable in every client language — clients need to compute
// the same partition to read a conversation without a round trip. Changing
// this function is a breaking wire change.
func (t *Topic) PartitionForKey(key []byte) int32 {
	if len(key) == 0 || len(t.partitions) == 1 {
		return 0
	}
	h := fnv.New64a()
	h.Write(key)
	return int32(h.Sum64() % uint64(len(t.partitions)))
}

// Log is the top-level handle over all stream topics on this node.
type Log struct {
	dir        string
	defaultCfg TopicConfig

	mu     sync.RWMutex
	topics map[string]*Topic
	closed bool

	// ackWaiter, when set, makes an append wait for replication before it is
	// reported as successful. Nil means single-node operation.
	ackWaiter AckWaiter

	// leaderEpoch is the term this node currently leads under, applied to every
	// partition. It lives on the Log rather than only on each Partition so a
	// topic created *after* promotion inherits the term instead of silently
	// writing epoch-less records that no follower could later reconcile.
	leaderEpoch uint32

	// enforceLeadership is remembered so partitions opened later — a topic
	// created at runtime, a replica placed here by a rebalance — start guarded
	// rather than briefly writable by a node that does not lead them.
	enforceLeadership bool

	// forwarder, when set, routes a write this node may not perform to the node
	// that leads the partition. Nil means there is nowhere to route to.
	forwarder WriteForwarder
}

// LogConfig configures the stream subsystem.
type LogConfig struct {
	// Dir is the root directory; each topic gets a subdirectory.
	Dir string
	// DefaultTopic is applied to topics created implicitly by a first append.
	DefaultTopic TopicConfig
}

// DefaultLogConfig returns settings appropriate for a chat workload.
func DefaultLogConfig(dir string) LogConfig {
	return LogConfig{
		Dir: dir,
		DefaultTopic: TopicConfig{
			Partitions: 16,
			Partition:  DefaultPartitionConfig(),
		},
	}
}

// OpenLog opens the stream subsystem, restoring every topic found on disk.
func OpenLog(cfg LogConfig) (*Log, error) {
	if cfg.Dir == "" {
		return nil, errors.New("stream: log dir is required")
	}
	if cfg.DefaultTopic.Partitions <= 0 {
		cfg.DefaultTopic.Partitions = 16
	}
	if err := os.MkdirAll(cfg.Dir, 0755); err != nil {
		return nil, fmt.Errorf("create stream dir: %w", err)
	}

	l := &Log{
		dir:        cfg.Dir,
		defaultCfg: cfg.DefaultTopic,
		topics:     make(map[string]*Topic),
	}

	entries, err := os.ReadDir(cfg.Dir)
	if err != nil {
		return nil, fmt.Errorf("read stream dir: %w", err)
	}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		name, err := decodeTopicDir(e.Name())
		if err != nil {
			continue // not a topic directory
		}
		t, err := l.openTopicDir(name, e.Name())
		if err != nil {
			l.Close()
			return nil, fmt.Errorf("open topic %q: %w", name, err)
		}
		l.topics[name] = t
	}
	return l, nil
}

// Topic names are user-controlled ("chat.conv.7f3a"), so they are encoded
// rather than used directly as path components. Encoding keeps '/' and '..'
// out of the filesystem path — a topic name must never be able to escape the
// data directory.
func encodeTopicDir(name string) string {
	var b strings.Builder
	for i := 0; i < len(name); i++ {
		c := name[i]
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9',
			c == '.', c == '-', c == '_':
			b.WriteByte(c)
		default:
			fmt.Fprintf(&b, "%%%02X", c)
		}
	}
	return b.String()
}

func decodeTopicDir(dir string) (string, error) {
	var b strings.Builder
	for i := 0; i < len(dir); i++ {
		if dir[i] != '%' {
			b.WriteByte(dir[i])
			continue
		}
		if i+2 >= len(dir) {
			return "", ErrInvalidTopic
		}
		v, err := strconv.ParseUint(dir[i+1:i+3], 16, 8)
		if err != nil {
			return "", ErrInvalidTopic
		}
		b.WriteByte(byte(v))
		i += 2
	}
	return b.String(), nil
}

// ValidTopicName reports whether a name is acceptable. Empty names and names
// long enough to blow past filesystem limits after encoding are rejected.
func ValidTopicName(name string) error {
	if name == "" {
		return fmt.Errorf("%w: empty", ErrInvalidTopic)
	}
	if len(encodeTopicDir(name)) > 200 {
		return fmt.Errorf("%w: too long", ErrInvalidTopic)
	}
	return nil
}

func (l *Log) openTopicDir(name, dirName string) (*Topic, error) {
	topicDir := filepath.Join(l.dir, dirName)
	entries, err := os.ReadDir(topicDir)
	if err != nil {
		return nil, err
	}

	var ids []int32
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		id, err := strconv.ParseInt(e.Name(), 10, 32)
		if err != nil {
			continue
		}
		ids = append(ids, int32(id))
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })

	if len(ids) == 0 {
		return nil, fmt.Errorf("topic dir %q has no partitions", name)
	}
	// Partition IDs must be a dense 0..N-1 range; a gap means a partition
	// directory was lost, and silently renumbering would remap every key.
	for i, id := range ids {
		if int32(i) != id {
			return nil, fmt.Errorf("topic %q: partition gap at %d (found %d)", name, i, id)
		}
	}

	cfg := l.defaultCfg
	cfg.Partitions = int32(len(ids))

	t := &Topic{Name: name, cfg: cfg}
	for _, id := range ids {
		p, err := OpenPartition(name, id, filepath.Join(topicDir, strconv.Itoa(int(id))), cfg.Partition)
		if err != nil {
			for _, op := range t.partitions {
				op.Close()
			}
			return nil, err
		}
		// A partition opened after enforcement was switched on must start
		// guarded; otherwise a topic created at runtime is briefly writable by
		// every node that hosts it.
		p.EnforceLeadership(l.enforceLeadership)
		t.partitions = append(t.partitions, p)
	}
	return t, nil
}

// CreateTopic creates a topic with an explicit configuration. It is idempotent
// when the existing partition count matches, and an error when it does not —
// silently accepting a different count would scatter one conversation's
// messages across partitions and destroy ordering.
func (l *Log) CreateTopic(name string, cfg TopicConfig) (*Topic, error) {
	if err := ValidTopicName(name); err != nil {
		return nil, err
	}
	if cfg.Partitions <= 0 {
		cfg.Partitions = l.defaultCfg.Partitions
	}
	cfg.Partition.applyDefaults()

	l.mu.Lock()
	defer l.mu.Unlock()
	if l.closed {
		return nil, ErrClosed
	}

	if t, ok := l.topics[name]; ok {
		if t.PartitionCount() != cfg.Partitions {
			return nil, fmt.Errorf("topic %q already exists with %d partitions (requested %d)",
				name, t.PartitionCount(), cfg.Partitions)
		}
		return t, nil
	}

	topicDir := filepath.Join(l.dir, encodeTopicDir(name))
	t := &Topic{Name: name, cfg: cfg}
	for id := int32(0); id < cfg.Partitions; id++ {
		p, err := OpenPartition(name, id, filepath.Join(topicDir, strconv.Itoa(int(id))), cfg.Partition)
		if err != nil {
			for _, op := range t.partitions {
				op.Close()
			}
			return nil, err
		}
		// Guard first, promote second. Under a control plane the promotion below
		// does not apply — leadership is granted per partition by the
		// controller — so the partition must start closed to local writes.
		p.EnforceLeadership(l.enforceLeadership)
		if l.leaderEpoch > UndefinedEpoch && !l.enforceLeadership {
			if err := p.BecomeLeader(l.leaderEpoch); err != nil {
				// A brand-new partition cannot already be at a higher epoch, so
				// this is a real failure (checkpoint unwritable), not a race.
				p.Close()
				for _, op := range t.partitions {
					op.Close()
				}
				return nil, fmt.Errorf("stamp leader epoch on new partition: %w", err)
			}
		}
		t.partitions = append(t.partitions, p)
	}
	l.topics[name] = t
	return t, nil
}

// BecomeLeader opens a new leadership term across every partition in the log.
//
// Call it once per promotion, before accepting any write. Every record appended
// afterwards carries this epoch, which is what lets a follower reconnecting to
// this node discover — and discard — records the previous leader accepted but
// never replicated.
//
// The epoch must come from whatever decides leadership. Passing a
// locally-invented number is only safe while promotion is a manual,
// one-at-a-time procedure: two nodes that independently pick the same epoch
// produce exactly the divergence this mechanism exists to detect.
func (l *Log) BecomeLeader(epoch uint32) error {
	if epoch == UndefinedEpoch {
		return errors.New("stream: leader epoch must be non-zero")
	}

	l.mu.Lock()
	defer l.mu.Unlock()
	if l.closed {
		return ErrClosed
	}
	if epoch <= l.leaderEpoch {
		return fmt.Errorf("stream: leader epoch %d is not newer than current %d", epoch, l.leaderEpoch)
	}

	for _, t := range l.topics {
		for _, p := range t.partitions {
			if err := p.BecomeLeader(epoch); err != nil {
				// Leave l.leaderEpoch unchanged: a partial promotion that
				// reported success would let some partitions write under the
				// new term and others under the old one.
				return fmt.Errorf("promote %s/%d: %w", t.Name, p.ID, err)
			}
		}
	}
	l.leaderEpoch = epoch
	return nil
}

// EnforceLeadership makes every partition in the log refuse local writes unless
// it has been granted leadership.
//
// Call it exactly once, at startup, when a control plane is present. It is not
// a runtime toggle: turning enforcement on while writes are in flight would
// reject them, and turning it off would reopen the split-brain window it exists
// to close.
//
// Partitions created after this call inherit the setting.
func (l *Log) EnforceLeadership(on bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.enforceLeadership = on
	for _, t := range l.topics {
		for _, p := range t.partitions {
			p.EnforceLeadership(on)
		}
	}
}

// ResignFor gives up leadership of one partition.
//
// The control plane calls this the moment it sees the partition assigned
// elsewhere. Doing it promptly matters more than it looks: between the new
// leader opening its term and this node noticing, both believe they may write,
// and each would assign its own records to the same sequences.
func (l *Log) ResignFor(topic string, partition int32) error {
	l.mu.RLock()
	t, ok := l.topics[topic]
	l.mu.RUnlock()
	if !ok {
		return fmt.Errorf("%w: %s", ErrNoSuchTopic, topic)
	}
	p, err := t.Partition(partition)
	if err != nil {
		return err
	}
	p.Resign()
	return nil
}

// BecomeLeaderFor opens a new leadership term for a single partition.
//
// This is the granularity a controller-driven cluster actually needs: a node
// leads some partitions and follows others, and failing over one partition must
// not disturb the terms of the rest. BecomeLeader remains the right call for
// single-node promotion, where the whole log moves at once.
//
// The epoch comes from the control plane, which assigned it under consensus, so
// the "locally-invented number" hazard BecomeLeader warns about does not apply
// here — that is the entire reason the assignment carries one.
func (l *Log) BecomeLeaderFor(topic string, partition int32, epoch uint32) error {
	if epoch == UndefinedEpoch {
		return errors.New("stream: leader epoch must be non-zero")
	}

	l.mu.RLock()
	if l.closed {
		l.mu.RUnlock()
		return ErrClosed
	}
	t, ok := l.topics[topic]
	l.mu.RUnlock()
	if !ok {
		return fmt.Errorf("%w: %s", ErrNoSuchTopic, topic)
	}

	p, err := t.Partition(partition)
	if err != nil {
		return err
	}
	cur, _ := p.LeaderEpoch()
	if epoch == cur {
		// Already this node's term. Re-assert rather than fail: after a
		// resignation the write guard is closed while the epoch is unchanged,
		// and treating that as an error would leave the partition permanently
		// unwritable by the node the cluster says leads it.
		return p.ReassertLeadership(epoch)
	}
	if epoch < cur {
		return fmt.Errorf("stream: leader epoch %d for %s/%d is older than current %d",
			epoch, topic, partition, cur)
	}
	if err := p.BecomeLeader(epoch); err != nil {
		return fmt.Errorf("promote %s/%d: %w", topic, partition, err)
	}

	// Track the high-water mark so NextLeaderEpoch and a later whole-log
	// promotion cannot hand back an epoch this partition has already used.
	l.mu.Lock()
	if epoch > l.leaderEpoch {
		l.leaderEpoch = epoch
	}
	l.mu.Unlock()
	return nil
}

// NextLeaderEpoch returns the epoch a promotion should use: one past the
// highest this node has ever seen, across every partition.
//
// Scanning all partitions rather than trusting l.leaderEpoch matters on
// restart — a node that was a follower learned epochs from its leader without
// ever setting its own.
func (l *Log) NextLeaderEpoch() uint32 {
	l.mu.RLock()
	defer l.mu.RUnlock()

	highest := l.leaderEpoch
	for _, t := range l.topics {
		for _, p := range t.partitions {
			if e, _ := p.LeaderEpoch(); e > highest {
				highest = e
			}
		}
	}
	return highest + 1
}

// LeaderEpoch returns the term this node is writing under, or UndefinedEpoch if
// it has never been promoted.
func (l *Log) LeaderEpoch() uint32 {
	l.mu.RLock()
	defer l.mu.RUnlock()
	return l.leaderEpoch
}

// Topic returns an existing topic.
func (l *Log) Topic(name string) (*Topic, error) {
	l.mu.RLock()
	defer l.mu.RUnlock()
	t, ok := l.topics[name]
	if !ok {
		return nil, fmt.Errorf("%w: %s", ErrNoSuchTopic, name)
	}
	return t, nil
}

// GetOrCreateTopic returns the topic, creating it with defaults if absent.
func (l *Log) GetOrCreateTopic(name string) (*Topic, error) {
	l.mu.RLock()
	t, ok := l.topics[name]
	l.mu.RUnlock()
	if ok {
		return t, nil
	}
	return l.CreateTopic(name, l.defaultCfg)
}

// TopicNames lists every topic, sorted.
func (l *Log) TopicNames() []string {
	l.mu.RLock()
	defer l.mu.RUnlock()
	names := make([]string, 0, len(l.topics))
	for n := range l.topics {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}

// DeleteTopic closes and removes a topic and all its data. Irreversible.
func (l *Log) DeleteTopic(name string) error {
	l.mu.Lock()
	t, ok := l.topics[name]
	if !ok {
		l.mu.Unlock()
		return fmt.Errorf("%w: %s", ErrNoSuchTopic, name)
	}
	delete(l.topics, name)
	l.mu.Unlock()

	for _, p := range t.partitions {
		p.Close()
	}
	return os.RemoveAll(filepath.Join(l.dir, encodeTopicDir(name)))
}

// AppendResult describes where a record landed.
type AppendResult struct {
	Topic     string `json:"topic"`
	Partition int32  `json:"partition"`
	Seq       uint64 `json:"seq"`
	Timestamp int64  `json:"timestamp"`
}

// Append routes a record to the partition owning its key and appends it.
// The topic is created with default configuration if it does not exist.
func (l *Log) Append(topic string, rec *Record) (AppendResult, error) {
	t, err := l.GetOrCreateTopic(topic)
	if err != nil {
		return AppendResult{}, err
	}
	pid := t.PartitionForKey(rec.Key)
	p, err := t.Partition(pid)
	if err != nil {
		return AppendResult{}, err
	}
	seq, err := p.Append(rec)
	if errors.Is(err, ErrNotPartitionLeader) {
		// This node holds the partition but does not lead it. Rather than fail
		// the user's message, hand the write to whoever does.
		//
		// Reads deliberately have no equivalent path: a replica has the data
		// locally, which is the entire reason for holding one. Only writes have
		// a single correct destination.
		if f := l.forwarderFor(); f != nil {
			return f.Forward(context.Background(), topic, pid, rec)
		}
	}
	if err != nil {
		return AppendResult{}, err
	}
	return AppendResult{Topic: topic, Partition: pid, Seq: seq, Timestamp: rec.Timestamp}, nil
}

// appendLocal appends without ever forwarding, reporting the partition it chose
// so a caller that wants to forward can address it.
func (l *Log) appendLocal(topic string, rec *Record) (AppendResult, int32, error) {
	t, err := l.GetOrCreateTopic(topic)
	if err != nil {
		return AppendResult{}, 0, err
	}
	pid := t.PartitionForKey(rec.Key)
	p, err := t.Partition(pid)
	if err != nil {
		return AppendResult{}, pid, err
	}
	seq, err := p.Append(rec)
	if err != nil {
		return AppendResult{}, pid, err
	}
	return AppendResult{Topic: topic, Partition: pid, Seq: seq, Timestamp: rec.Timestamp}, pid, nil
}

// WriteForwarder sends a write to the node that leads its partition.
//
// It exists as a hook for the same reason AckWaiter does: the log knows a write
// does not belong here, but has no business knowing how nodes talk to each
// other. A nil forwarder means single-node or static operation, where being
// asked to write a partition you do not lead is a genuine error rather than a
// routing decision.
type WriteForwarder interface {
	Forward(ctx context.Context, topic string, partition int32, rec *Record) (AppendResult, error)
}

// SetWriteForwarder installs (or clears, with nil) the routing hook.
func (l *Log) SetWriteForwarder(f WriteForwarder) {
	l.mu.Lock()
	l.forwarder = f
	l.mu.Unlock()
}

func (l *Log) forwarderFor() WriteForwarder {
	l.mu.RLock()
	defer l.mu.RUnlock()
	return l.forwarder
}

// Read fetches records from one partition.
func (l *Log) Read(topic string, partition int32, fromSeq uint64, limit int) ([]*Record, error) {
	t, err := l.Topic(topic)
	if err != nil {
		return nil, err
	}
	p, err := t.Partition(partition)
	if err != nil {
		return nil, err
	}
	return p.ReadFrom(fromSeq, limit)
}

// ReadByKey fetches records from the partition that owns key.
//
// It resolves the partition by key but does NOT filter by it: the result
// contains every record in that partition from fromSeq onwards, whatever its
// key. That is only safe when the topic holds a single key — one topic per
// conversation, or a single-partition inbox.
//
// If several keys share a topic, this returns other keys' records to the
// caller. Use ReadKeyOnly instead, which is the same call with the filter
// applied. The two are kept separate rather than one being made "smart"
// because the unfiltered form is what a partition tailer wants and silently
// changing its result set would break those callers.
func (l *Log) ReadByKey(topic string, key []byte, fromSeq uint64, limit int) ([]*Record, int32, error) {
	t, err := l.Topic(topic)
	if err != nil {
		return nil, 0, err
	}
	pid := t.PartitionForKey(key)
	p, err := t.Partition(pid)
	if err != nil {
		return nil, 0, err
	}
	recs, err := p.ReadFrom(fromSeq, limit)
	return recs, pid, err
}

// ReadKeyOnly returns up to limit records carrying exactly this key, from the
// partition that owns it.
//
// nextSeq is where a follow-up call should resume: one past the last record
// examined, not the last record returned. Resuming from the last *returned*
// record would re-scan the same foreign records on every page, turning a
// paginated history read into quadratic work.
//
// Scanning is the cost of sharing a partition between keys. It is bounded by
// scanLimit records examined per call, so a conversation buried among busier
// ones cannot make one request read the whole partition. A short result with
// nextSeq below the partition head means the budget ran out, not that the key
// has no more records — callers paginate until nextSeq stops advancing.
func (l *Log) ReadKeyOnly(topic string, key []byte, fromSeq uint64, limit, scanLimit int) (recs []*Record, pid int32, nextSeq uint64, err error) {
	if limit <= 0 {
		return nil, 0, fromSeq, nil
	}
	if scanLimit <= 0 {
		scanLimit = 4096
	}

	t, err := l.Topic(topic)
	if err != nil {
		return nil, 0, fromSeq, err
	}
	pid = t.PartitionForKey(key)
	p, err := t.Partition(pid)
	if err != nil {
		return nil, 0, fromSeq, err
	}

	next := fromSeq
	scanned := 0
	for len(recs) < limit && scanned < scanLimit {
		batch, err := p.ReadFrom(next, min(scanLimit-scanned, 512))
		if err != nil {
			return recs, pid, next, err
		}
		if len(batch) == 0 {
			break // caught up with the partition head
		}
		for _, r := range batch {
			scanned++
			next = r.Seq + 1
			if bytes.Equal(r.Key, key) {
				recs = append(recs, r)
				if len(recs) == limit {
					break
				}
			}
		}
	}
	return recs, pid, next, nil
}

// Tail streams a partition from fromSeq until ctx is cancelled.
func (l *Log) Tail(ctx context.Context, topic string, partition int32, fromSeq uint64, fn func([]*Record) error) error {
	t, err := l.Topic(topic)
	if err != nil {
		return err
	}
	p, err := t.Partition(partition)
	if err != nil {
		return err
	}
	return p.Tail(ctx, fromSeq, 64, fn)
}

// TopicStats summarises a topic for the admin API and dashboard.
type TopicStats struct {
	Name       string           `json:"name"`
	Partitions []PartitionStats `json:"partitions"`
	TotalBytes int64            `json:"total_bytes"`
}

// PartitionStats summarises one partition.
type PartitionStats struct {
	ID       int32  `json:"id"`
	FirstSeq uint64 `json:"first_seq"`
	NextSeq  uint64 `json:"next_seq"`
	Records  uint64 `json:"records"`
	Bytes    int64  `json:"bytes"`
}

// Stats returns per-topic statistics.
func (l *Log) Stats() []TopicStats {
	l.mu.RLock()
	topics := make([]*Topic, 0, len(l.topics))
	for _, t := range l.topics {
		topics = append(topics, t)
	}
	l.mu.RUnlock()

	sort.Slice(topics, func(i, j int) bool { return topics[i].Name < topics[j].Name })

	out := make([]TopicStats, 0, len(topics))
	for _, t := range topics {
		ts := TopicStats{Name: t.Name}
		for _, p := range t.partitions {
			first, next, b := p.FirstSeq(), p.NextSeq(), p.Bytes()
			ts.Partitions = append(ts.Partitions, PartitionStats{
				ID: p.ID, FirstSeq: first, NextSeq: next, Records: next - first, Bytes: b,
			})
			ts.TotalBytes += b
		}
		out = append(out, ts)
	}
	return out
}

// EnforceRetention runs retention over every partition. Call it periodically.
func (l *Log) EnforceRetention() (int, error) {
	l.mu.RLock()
	var parts []*Partition
	for _, t := range l.topics {
		parts = append(parts, t.partitions...)
	}
	l.mu.RUnlock()

	total := 0
	var firstErr error
	for _, p := range parts {
		n, err := p.EnforceRetention()
		total += n
		if err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return total, firstErr
}

// Sync fsyncs every active segment.
func (l *Log) Sync() error {
	l.mu.RLock()
	var parts []*Partition
	for _, t := range l.topics {
		parts = append(parts, t.partitions...)
	}
	l.mu.RUnlock()

	var firstErr error
	for _, p := range parts {
		if err := p.Sync(); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}

// StartMaintenance runs retention and periodic fsync on a background loop.
// Returns a stop function.
func (l *Log) StartMaintenance(interval time.Duration) func() {
	if interval <= 0 {
		interval = 30 * time.Second
	}
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				l.Sync()
				l.EnforceRetention()
			}
		}
	}()
	return cancel
}

// Close flushes and closes every partition.
func (l *Log) Close() error {
	l.mu.Lock()
	if l.closed {
		l.mu.Unlock()
		return nil
	}
	l.closed = true
	topics := l.topics
	l.topics = nil
	l.mu.Unlock()

	var firstErr error
	for _, t := range topics {
		for _, p := range t.partitions {
			if err := p.Close(); err != nil && firstErr == nil {
				firstErr = err
			}
		}
	}
	return firstErr
}
