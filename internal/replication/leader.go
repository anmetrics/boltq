package replication

import (
	"bufio"
	"bytes"
	"context"
	"crypto/subtle"
	"errors"
	"fmt"
	"log"
	"net"
	"sort"
	"sync"
	"time"

	"github.com/boltq/boltq/internal/stream"
)

// LeaderConfig configures the replication server.
type LeaderConfig struct {
	// Addr is the listen address for follower connections. This is an internal
	// plane; do not expose it to the internet.
	Addr string
	// NodeID identifies this leader in logs and follower handshakes.
	NodeID string
	// Secret authenticates followers. A replication connection can read every
	// message in the log, so an unauthenticated listener is a total data
	// disclosure. Empty means no authentication, which is only acceptable on a
	// trusted private network.
	Secret string

	// MinInSync is how many replicas — including the leader — must hold a
	// record before an append is acknowledged.
	//
	//	1  leader only; replication is asynchronous (default)
	//	2  the leader plus one follower; survives losing one node
	//	3  the leader plus two followers
	//
	// Values above the replica count make every write fail, which is the
	// correct behaviour: it is better to refuse writes than to accept them at a
	// durability the operator did not ask for.
	MinInSync int

	// AckTimeout bounds how long an append waits for replicas.
	AckTimeout time.Duration

	// MaxLagInSync is how far behind a follower may fall and still count
	// toward quorum. A follower past it is excluded so one slow node cannot
	// stall every write.
	MaxLagInSync uint64

	// BatchRecords caps records per MsgRecords frame.
	BatchRecords int

	// HeartbeatInterval is how often an idle stream sends a ping.
	HeartbeatInterval time.Duration
}

func (c *LeaderConfig) applyDefaults() {
	if c.MinInSync <= 0 {
		c.MinInSync = 1
	}
	if c.AckTimeout <= 0 {
		c.AckTimeout = 5 * time.Second
	}
	if c.MaxLagInSync == 0 {
		c.MaxLagInSync = 10000
	}
	if c.BatchRecords <= 0 {
		c.BatchRecords = 256
	}
	if c.HeartbeatInterval <= 0 {
		c.HeartbeatInterval = 10 * time.Second
	}
}

// Leader streams the local log to connected followers and tracks how far each
// has got, so an append can wait for the durability the operator configured.
type Leader struct {
	cfg LeaderConfig
	log *stream.Log

	mu sync.RWMutex
	// cursors is keyed by "topic/partition"; each holds per-follower positions.
	cursors map[string]*partitionAcks
	// followers is the set of connected node IDs.
	followers map[string]time.Time

	listener net.Listener
	// conns tracks accepted follower connections. Cancelling a context does not
	// interrupt a goroutine blocked in a socket read, so shutdown has to close
	// the sockets themselves or Close would wait forever on a connected peer.
	conns map[net.Conn]struct{}

	wg     sync.WaitGroup
	ctx    context.Context
	cancel context.CancelFunc
	once   sync.Once

	stats        Stats
	shuttingDown bool
}

// Stats counts leader activity.
type Stats struct {
	FollowersConnected int    `json:"followers_connected"`
	RecordsShipped     uint64 `json:"records_shipped"`
	BytesShipped       uint64 `json:"bytes_shipped"`
	AcksReceived       uint64 `json:"acks_received"`
	AuthFailures       uint64 `json:"auth_failures"`
	AckTimeouts        uint64 `json:"ack_timeouts"`
	MinInSync          int    `json:"min_in_sync"`
}

// partitionAcks tracks follower positions for one partition and wakes waiters
// when the quorum position advances.
type partitionAcks struct {
	mu     sync.Mutex
	acked  map[string]uint64
	seen   map[string]time.Time
	waiter chan struct{} // closed and replaced whenever any position advances
}

func newPartitionAcks() *partitionAcks {
	return &partitionAcks{
		acked:  make(map[string]uint64),
		seen:   make(map[string]time.Time),
		waiter: make(chan struct{}),
	}
}

func (p *partitionAcks) record(nodeID string, seq uint64) {
	p.mu.Lock()
	cur, ok := p.acked[nodeID]
	if !ok || seq > cur {
		p.acked[nodeID] = seq
	}
	p.seen[nodeID] = time.Now()
	// Broadcast on every ack: waiters re-evaluate their own threshold, which
	// is cheaper and far simpler than tracking per-sequence waiter lists.
	close(p.waiter)
	p.waiter = make(chan struct{})
	p.mu.Unlock()
}

func (p *partitionAcks) drop(nodeID string) {
	p.mu.Lock()
	delete(p.acked, nodeID)
	delete(p.seen, nodeID)
	close(p.waiter)
	p.waiter = make(chan struct{})
	p.mu.Unlock()
}

// countAtLeast returns how many followers hold at least seq, plus a wake
// channel to wait on if that is not yet enough.
func (p *partitionAcks) countAtLeast(seq uint64) (int, <-chan struct{}) {
	p.mu.Lock()
	defer p.mu.Unlock()
	n := 0
	for _, v := range p.acked {
		if v >= seq {
			n++
		}
	}
	return n, p.waiter
}

func (p *partitionAcks) snapshot() (map[string]uint64, map[string]time.Time) {
	p.mu.Lock()
	defer p.mu.Unlock()
	a := make(map[string]uint64, len(p.acked))
	s := make(map[string]time.Time, len(p.seen))
	for k, v := range p.acked {
		a[k] = v
	}
	for k, v := range p.seen {
		s[k] = v
	}
	return a, s
}

// NewLeader creates a replication leader over the given log.
func NewLeader(l *stream.Log, cfg LeaderConfig) (*Leader, error) {
	if l == nil {
		return nil, errors.New("replication: a stream log is required")
	}
	cfg.applyDefaults()

	ctx, cancel := context.WithCancel(context.Background())
	return &Leader{
		cfg:       cfg,
		log:       l,
		cursors:   make(map[string]*partitionAcks),
		followers: make(map[string]time.Time),
		conns:     make(map[net.Conn]struct{}),
		ctx:       ctx,
		cancel:    cancel,
	}, nil
}

func cursorKey(topic string, partition int32) string {
	return fmt.Sprintf("%s/%d", topic, partition)
}

func (l *Leader) acksFor(topic string, partition int32) *partitionAcks {
	k := cursorKey(topic, partition)

	l.mu.RLock()
	pa, ok := l.cursors[k]
	l.mu.RUnlock()
	if ok {
		return pa
	}

	l.mu.Lock()
	defer l.mu.Unlock()
	if pa, ok := l.cursors[k]; ok {
		return pa
	}
	pa = newPartitionAcks()
	l.cursors[k] = pa
	return pa
}

// Start begins accepting follower connections.
func (l *Leader) Start() error {
	if l.cfg.Addr == "" {
		return errors.New("replication: leader address is required")
	}
	ln, err := net.Listen("tcp", l.cfg.Addr)
	if err != nil {
		return fmt.Errorf("replication listen: %w", err)
	}
	l.listener = ln
	log.Printf("[replication] leader listening on %s (min_in_sync=%d)", l.cfg.Addr, l.cfg.MinInSync)

	l.wg.Add(1)
	go func() {
		defer l.wg.Done()
		l.acceptLoop(ln)
	}()
	return nil
}

func (l *Leader) acceptLoop(ln net.Listener) {
	for {
		conn, err := ln.Accept()
		if err != nil {
			select {
			case <-l.ctx.Done():
				return
			default:
				log.Printf("[replication] accept: %v", err)
				continue
			}
		}
		l.wg.Add(1)
		go func() {
			defer l.wg.Done()
			l.serve(conn)
		}()
	}
}

// serve handles one follower connection for its whole lifetime.
func (l *Leader) serve(conn net.Conn) {
	l.mu.Lock()
	if l.shuttingDown {
		l.mu.Unlock()
		conn.Close()
		return
	}
	l.conns[conn] = struct{}{}
	l.mu.Unlock()

	defer func() {
		l.mu.Lock()
		delete(l.conns, conn)
		l.mu.Unlock()
		conn.Close()
	}()

	reader := bufio.NewReaderSize(conn, 64*1024)
	writer := bufio.NewWriterSize(conn, 256*1024)

	// Handshake first, so an unauthenticated peer never reaches the streaming
	// path and never learns a topic name.
	conn.SetReadDeadline(time.Now().Add(10 * time.Second))
	typ, payload, err := readFrame(reader)
	if err != nil || typ != MsgHello {
		return
	}

	nodeID, ok := l.authenticate(payload)
	if !ok {
		l.bump(func(s *Stats) { s.AuthFailures++ })
		writeFrame(writer, MsgError, []byte("unauthorized"))
		writer.Flush()
		return
	}
	conn.SetReadDeadline(time.Time{})

	l.mu.Lock()
	l.followers[nodeID] = time.Now()
	l.mu.Unlock()
	log.Printf("[replication] follower %s connected from %s", nodeID, conn.RemoteAddr())

	defer func() {
		l.mu.Lock()
		delete(l.followers, nodeID)
		cursors := make([]*partitionAcks, 0, len(l.cursors))
		for _, pa := range l.cursors {
			cursors = append(cursors, pa)
		}
		l.mu.Unlock()

		// A departed follower must stop counting toward quorum immediately,
		// otherwise writes would keep succeeding against a replica that is gone.
		for _, pa := range cursors {
			pa.drop(nodeID)
		}
		log.Printf("[replication] follower %s disconnected", nodeID)
	}()

	if err := writeFrame(writer, MsgHello, []byte(l.cfg.NodeID)); err != nil {
		return
	}
	if err := writer.Flush(); err != nil {
		return
	}

	// One writer goroutine per connection: several partition streams share the
	// socket and a bufio.Writer is not safe for concurrent use.
	var writeMu sync.Mutex
	ctx, cancel := context.WithCancel(l.ctx)
	defer cancel()

	streams := make(map[string]context.CancelFunc)
	var streamsMu sync.Mutex
	defer func() {
		streamsMu.Lock()
		for _, c := range streams {
			c()
		}
		streamsMu.Unlock()
	}()

	for {
		typ, payload, err := readFrame(reader)
		if err != nil {
			return
		}

		switch typ {
		case MsgFetch:
			req, err := decodeFetch(payload)
			if err != nil {
				l.sendError(writer, &writeMu, err.Error())
				continue
			}
			key := cursorKey(req.Topic, req.Partition)

			streamsMu.Lock()
			if prev, exists := streams[key]; exists {
				// A re-fetch means the follower hit a gap or restarted; the old
				// stream is now sending from the wrong position.
				prev()
			}
			sctx, scancel := context.WithCancel(ctx)
			streams[key] = scancel
			streamsMu.Unlock()

			l.wg.Add(1)
			go func() {
				defer l.wg.Done()
				l.streamPartition(sctx, writer, &writeMu, nodeID, req)
			}()

		case MsgEpochRequest:
			req, err := decodeEpochRequest(payload)
			if err != nil {
				l.sendError(writer, &writeMu, err.Error())
				continue
			}
			resp := l.answerEpoch(req)
			writeMu.Lock()
			err = writeFrame(writer, MsgEpochResponse, resp.encode())
			if err == nil {
				err = writer.Flush()
			}
			writeMu.Unlock()
			if err != nil {
				return
			}

		case MsgAck:
			ack, err := decodeAck(payload)
			if err != nil {
				continue
			}
			l.acksFor(ack.Topic, ack.Partition).record(nodeID, ack.Seq)
			l.bump(func(s *Stats) { s.AcksReceived++ })

		case MsgPing:
			writeMu.Lock()
			writeFrame(writer, MsgPong, nil)
			writer.Flush()
			writeMu.Unlock()

		case MsgPong:
			// liveness only

		default:
			l.sendError(writer, &writeMu, fmt.Sprintf("unexpected message type 0x%02x", typ))
		}
	}
}

// answerEpoch resolves where a follower's last known leader epoch ended on this
// node.
//
// A partition this leader does not have yet is answered with the undefined
// epoch rather than an error: the follower then starts from the beginning,
// which is correct for a topic created after it last connected.
func (l *Leader) answerEpoch(req epochRequest) epochResponse {
	resp := epochResponse{Topic: req.Topic, Partition: req.Partition}

	p, err := l.log.PartitionFor(req.Topic, req.Partition)
	if err != nil {
		return resp
	}
	epoch, endSeq := p.EndOffsetForEpoch(req.Epoch)
	resp.Epoch = epoch
	resp.EndSeq = endSeq
	return resp
}

// authenticate checks the hello payload, which is "nodeID\x00secret".
func (l *Leader) authenticate(payload []byte) (string, bool) {
	parts := bytes.SplitN(payload, []byte{0}, 2)
	nodeID := string(parts[0])
	if nodeID == "" {
		return "", false
	}
	if l.cfg.Secret == "" {
		return nodeID, true
	}
	if len(parts) != 2 {
		return "", false
	}
	// Constant-time: a timing-variable compare would leak the secret to a peer
	// able to reconnect repeatedly.
	if subtle.ConstantTimeCompare(parts[1], []byte(l.cfg.Secret)) != 1 {
		return "", false
	}
	return nodeID, true
}

func (l *Leader) sendError(w *bufio.Writer, mu *sync.Mutex, msg string) {
	mu.Lock()
	writeFrame(w, MsgError, []byte(msg))
	w.Flush()
	mu.Unlock()
}

// streamPartition ships a partition to one follower from req.FromSeq onwards,
// then tails it.
func (l *Leader) streamPartition(ctx context.Context, w *bufio.Writer, mu *sync.Mutex, nodeID string, req fetchRequest) {
	// The topic may not exist yet. Stream topics are created lazily on the
	// first message, so a follower assigned a partition before anyone has
	// spoken in that conversation is entirely normal — giving up here would
	// leave the partition unreplicated forever.
	p, err := l.awaitPartition(ctx, req.Topic, req.Partition)
	if err != nil {
		if ctx.Err() == nil {
			l.sendError(w, mu, err.Error())
		}
		return
	}

	from := req.FromSeq
	if from == 0 {
		from = 1
	}
	heartbeat := time.NewTicker(l.cfg.HeartbeatInterval)
	defer heartbeat.Stop()

	for {
		if ctx.Err() != nil {
			return
		}

		// Register for the wake-up before reading, or an append landing between
		// the read and the wait would leave the follower asleep until the next
		// message — which in a quiet conversation could be hours.
		wake := p.NotifyChan()

		recs, err := p.ReadFrom(from, l.cfg.BatchRecords)
		if err != nil {
			if errors.Is(err, stream.ErrSeqTruncated) {
				// Retention overtook this follower. Skip to the oldest record
				// still present and tell it, so it can surface a gap rather
				// than silently believing it has complete history.
				first := p.FirstSeq()
				l.sendError(w, mu, fmt.Sprintf("retention gap on %s/%d: resuming at %d",
					req.Topic, req.Partition, first))
				from = first
				continue
			}
			if errors.Is(err, stream.ErrClosed) {
				return
			}
			l.sendError(w, mu, err.Error())
			return
		}

		if len(recs) == 0 {
			select {
			case <-wake:
			case <-heartbeat.C:
				mu.Lock()
				err := writeFrame(w, MsgPing, nil)
				if err == nil {
					err = w.Flush()
				}
				mu.Unlock()
				if err != nil {
					return
				}
			case <-ctx.Done():
				return
			}
			continue
		}

		payload, err := encodeBatch(req.Topic, req.Partition, recs)
		if err != nil {
			l.sendError(w, mu, err.Error())
			return
		}

		mu.Lock()
		err = writeFrame(w, MsgRecords, payload)
		if err == nil {
			err = w.Flush()
		}
		mu.Unlock()
		if err != nil {
			return
		}

		from = recs[len(recs)-1].Seq + 1
		l.bump(func(s *Stats) {
			s.RecordsShipped += uint64(len(recs))
			s.BytesShipped += uint64(len(payload))
		})
	}
}

// partitionSource is what the leader needs from a partition in order to stream
// it: read from a position, know where the log starts and ends, and be woken
// when it grows.
type partitionSource interface {
	ReadFrom(fromSeq uint64, limit int) ([]*stream.Record, error)
	NextSeq() uint64
	FirstSeq() uint64
	NotifyChan() <-chan struct{}
}

// awaitPartition resolves a partition, waiting for the topic to be created if
// it does not exist yet.
//
// Polling is crude but correct: there is no topic-creation event to subscribe
// to, and the interval only affects how quickly replication starts for a brand
// new conversation — not steady-state latency, which is driven by the
// partition's notify channel once the stream is running.
func (l *Leader) awaitPartition(ctx context.Context, topic string, partition int32) (partitionSource, error) {
	const pollInterval = 100 * time.Millisecond

	for {
		p, err := l.log.PartitionFor(topic, partition)
		if err == nil {
			return p, nil
		}
		// A partition ID outside the topic's range is a configuration error and
		// will never resolve, so fail rather than poll forever.
		if errors.Is(err, stream.ErrNoSuchPartition) {
			return nil, err
		}
		if !errors.Is(err, stream.ErrNoSuchTopic) {
			return nil, err
		}

		select {
		case <-time.After(pollInterval):
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
}

func encodeBatch(topic string, partition int32, recs []*stream.Record) ([]byte, error) {
	var buf bytes.Buffer
	buf.Write(encodeRecordsHeader(recordsHeader{
		Topic: topic, Partition: partition, Count: uint32(len(recs)),
	}))
	for _, r := range recs {
		frame, err := stream.EncodeRecord(r)
		if err != nil {
			return nil, err
		}
		buf.Write(frame)
	}
	return buf.Bytes(), nil
}

// WaitFor implements stream.AckWaiter.
//
// It returns once enough replicas hold seq. The leader counts as one, so
// MinInSync of 1 returns immediately and 2 waits for a single follower.
func (l *Leader) WaitFor(ctx context.Context, topic string, partition int32, seq uint64) error {
	needFollowers := l.cfg.MinInSync - 1
	if needFollowers <= 0 {
		return nil
	}

	pa := l.acksFor(topic, partition)

	timer := time.NewTimer(l.cfg.AckTimeout)
	defer timer.Stop()

	for {
		have, wake := pa.countAtLeast(seq)
		if have >= needFollowers {
			return nil
		}

		// Fail fast when the required durability is unreachable, rather than
		// blocking every publisher until the timeout on a cluster that simply
		// does not have enough nodes connected.
		l.mu.RLock()
		connected := len(l.followers)
		l.mu.RUnlock()
		if connected < needFollowers {
			return fmt.Errorf("%w: need %d follower(s), %d connected",
				stream.ErrNotEnoughReplicas, needFollowers, connected)
		}

		select {
		case <-wake:
		case <-timer.C:
			l.bump(func(s *Stats) { s.AckTimeouts++ })
			return fmt.Errorf("%w: %s/%d seq %d reached %d of %d follower(s)",
				stream.ErrAckTimeout, topic, partition, seq, have, needFollowers)
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

// Replication returns the replication state for one partition.
func (l *Leader) Replication(topic string, partition int32) stream.PartitionReplication {
	out := stream.PartitionReplication{Topic: topic, Partition: partition}

	if p, err := l.log.PartitionFor(topic, partition); err == nil {
		out.LeaderSeq = p.NextSeq()
		out.LeaderEpoch, out.EpochStartSeq = p.LeaderEpoch()
	}

	acked, seen := l.acksFor(topic, partition).snapshot()
	for node, seq := range acked {
		lag := uint64(0)
		if out.LeaderSeq > seq {
			lag = out.LeaderSeq - seq
		}
		inSync := lag <= l.cfg.MaxLagInSync
		if inSync {
			out.InSync++
		}
		out.Replicas = append(out.Replicas, stream.ReplicaState{
			NodeID: node, AckedSeq: seq, Lag: lag,
			InSync: inSync, LastSeen: seen[node],
		})
	}
	sort.Slice(out.Replicas, func(i, j int) bool {
		return out.Replicas[i].NodeID < out.Replicas[j].NodeID
	})

	// The leader itself is always in sync with itself.
	out.InSync++
	out.HighWatermark = quorumPosition(out.LeaderSeq, acked, l.cfg.MinInSync)
	return out
}

// quorumPosition returns the highest sequence held by minInSync replicas,
// counting the leader.
func quorumPosition(leaderSeq uint64, acked map[string]uint64, minInSync int) uint64 {
	if minInSync <= 1 {
		return leaderSeq
	}
	positions := make([]uint64, 0, len(acked)+1)
	positions = append(positions, leaderSeq)
	for _, v := range acked {
		positions = append(positions, v)
	}
	// Descending: the k-th element is the highest position at least k replicas
	// hold.
	sort.Slice(positions, func(i, j int) bool { return positions[i] > positions[j] })
	if minInSync > len(positions) {
		return 0
	}
	return positions[minInSync-1]
}

// Followers lists connected follower node IDs.
func (l *Leader) Followers() []string {
	l.mu.RLock()
	defer l.mu.RUnlock()
	out := make([]string, 0, len(l.followers))
	for id := range l.followers {
		out = append(out, id)
	}
	sort.Strings(out)
	return out
}

func (l *Leader) bump(f func(*Stats)) {
	l.mu.Lock()
	f(&l.stats)
	l.mu.Unlock()
}

// Stats returns a snapshot of leader counters.
func (l *Leader) Stats() Stats {
	l.mu.RLock()
	defer l.mu.RUnlock()
	s := l.stats
	s.FollowersConnected = len(l.followers)
	s.MinInSync = l.cfg.MinInSync
	return s
}

// Close stops the listener and every follower stream.
func (l *Leader) Close() {
	l.once.Do(func() {
		l.cancel()
		if l.listener != nil {
			l.listener.Close()
		}

		// Close live connections so their read loops unblock. Without this,
		// wg.Wait below would hang for as long as a follower stays connected.
		l.mu.Lock()
		l.shuttingDown = true
		conns := make([]net.Conn, 0, len(l.conns))
		for c := range l.conns {
			conns = append(conns, c)
		}
		l.mu.Unlock()
		for _, c := range conns {
			c.Close()
		}

		l.wg.Wait()
	})
}
