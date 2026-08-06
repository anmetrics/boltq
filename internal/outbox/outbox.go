// Package outbox delivers to users who are not connected.
//
// A message for an online user needs nothing extra: their device is tailing
// the log and wakes on the append. A message for an offline user needs a push
// notification, and that is a fundamentally different kind of delivery — it
// leaves the system, costs money per send, can fail for hours, and must not be
// retried forever.
//
// The dispatcher is therefore a normal log consumer with its own cursor,
// running behind the write path rather than inside it. Sending a message never
// waits on APNs. If the push provider is down, the cursor simply stops
// advancing and resumes when it recovers — no queue to overflow, no messages
// lost, and no coupling between "the message was stored" and "the notification
// was delivered".
package outbox

import (
	"context"
	"errors"
	"log"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/boltq/boltq/internal/fanout"
	"github.com/boltq/boltq/internal/presence"
	"github.com/boltq/boltq/internal/stream"
)

// DispatcherGroup is the cursor group the dispatcher commits under. It is
// distinct from any device group, so push progress and read progress advance
// independently.
const DispatcherGroup = "push-dispatcher"

// Notification is one pending push.
type Notification struct {
	UserID         string `json:"user_id"`
	MessageID      string `json:"message_id"`
	ConversationID string `json:"conversation_id"`
	Kind           string `json:"kind"`
	SenderID       string `json:"sender_id"`
	// ConvTopic, ConvPartition and ConvSeq let the receiving client jump
	// straight to the message once it opens the app.
	ConvTopic     string            `json:"conv_topic,omitempty"`
	ConvPartition int32             `json:"conv_partition"`
	ConvSeq       uint64            `json:"conv_seq"`
	Headers       map[string]string `json:"headers,omitempty"`
	At            int64             `json:"at"`
	// Attempt counts prior delivery attempts, so a notifier can suppress or
	// downgrade repeats.
	Attempt int `json:"attempt"`
}

// Notifier hands notifications to the outside world — APNs, FCM, an SMS
// gateway, or an application webhook that fans out to all three.
//
// It must be idempotent-tolerant: the dispatcher is at-least-once, so a
// notification can be presented twice after a crash between sending and
// committing the cursor.
type Notifier interface {
	Notify(ctx context.Context, batch []Notification) error
}

// NotifierFunc adapts a function to Notifier.
type NotifierFunc func(ctx context.Context, batch []Notification) error

// Notify implements Notifier.
func (f NotifierFunc) Notify(ctx context.Context, batch []Notification) error { return f(ctx, batch) }

// DiscardNotifier drops everything. It is the default, so an unconfigured
// deployment does not accumulate an unbounded backlog of undeliverable pushes.
type DiscardNotifier struct{}

// Notify implements Notifier.
func (DiscardNotifier) Notify(context.Context, []Notification) error { return nil }

// Config tunes the dispatcher.
type Config struct {
	// BatchSize is how many records are read per poll.
	BatchSize int
	// BatchWindow is how long to accumulate notifications before flushing.
	// Batching matters: a group message to 50 offline users should be one
	// call to the push provider, not 50.
	BatchWindow time.Duration
	// MaxAttempts bounds retries of a failing batch before it is dropped and
	// the cursor advances past it. Without a bound, one permanently bad batch
	// stalls every notification behind it.
	MaxAttempts int
	// RetryBackoff is the initial delay between attempts; it doubles up to
	// MaxBackoff.
	RetryBackoff time.Duration
	// MaxBackoff caps the retry delay.
	MaxBackoff time.Duration
	// GraceDelay is how long to wait after a message before pushing. A user
	// who picks up their phone within a couple of seconds should read the
	// message in the app rather than get a notification for something they are
	// already looking at.
	GraceDelay time.Duration
}

func (c *Config) applyDefaults() {
	if c.BatchSize <= 0 {
		c.BatchSize = 256
	}
	if c.BatchWindow <= 0 {
		c.BatchWindow = 500 * time.Millisecond
	}
	if c.MaxAttempts <= 0 {
		c.MaxAttempts = 5
	}
	if c.RetryBackoff <= 0 {
		c.RetryBackoff = time.Second
	}
	if c.MaxBackoff <= 0 {
		c.MaxBackoff = 2 * time.Minute
	}
	if c.GraceDelay < 0 {
		c.GraceDelay = 0
	}
}

// Stats counts dispatcher activity.
type Stats struct {
	Scanned    uint64 `json:"scanned"`
	Suppressed uint64 `json:"suppressed"` // recipient was online
	Sent       uint64 `json:"sent"`
	Failed     uint64 `json:"failed"`
	Dropped    uint64 `json:"dropped"` // exceeded MaxAttempts
	Watching   int    `json:"watching"`
}

// Dispatcher tails inbox topics and pushes to offline users.
type Dispatcher struct {
	log      *stream.Log
	cursors  *stream.CursorStore
	presence presence.Lookup
	notifier Notifier
	cfg      Config

	mu       sync.Mutex
	watching map[string]context.CancelFunc // topic -> cancel
	stats    Stats

	wg       sync.WaitGroup
	stopOnce sync.Once
	ctx      context.Context
	cancel   context.CancelFunc
}

// Options assembles a Dispatcher.
type Options struct {
	Log     *stream.Log
	Cursors *stream.CursorStore
	// Presence decides whether a notification is needed. Routed to the shard
	// owner when a control plane is present.
	Presence presence.Lookup
	Notifier Notifier
	Config   Config
}

// New creates a Dispatcher. It does not start watching any topic until Watch
// or WatchInboxes is called.
func New(opts Options) (*Dispatcher, error) {
	if opts.Log == nil {
		return nil, errors.New("outbox: a stream log is required")
	}
	if opts.Cursors == nil {
		return nil, errors.New("outbox: a cursor store is required")
	}
	if opts.Notifier == nil {
		opts.Notifier = DiscardNotifier{}
	}
	opts.Config.applyDefaults()

	ctx, cancel := context.WithCancel(context.Background())
	return &Dispatcher{
		log:      opts.Log,
		cursors:  opts.Cursors,
		presence: opts.Presence,
		notifier: opts.Notifier,
		cfg:      opts.Config,
		watching: make(map[string]context.CancelFunc),
		ctx:      ctx,
		cancel:   cancel,
	}, nil
}

// Watch begins dispatching for one inbox topic. Calling it twice for the same
// topic is a no-op.
func (d *Dispatcher) Watch(topic string) error {
	d.mu.Lock()
	if _, exists := d.watching[topic]; exists {
		d.mu.Unlock()
		return nil
	}
	t, err := d.log.Topic(topic)
	if err != nil {
		d.mu.Unlock()
		return err
	}
	ctx, cancel := context.WithCancel(d.ctx)
	d.watching[topic] = cancel
	d.mu.Unlock()

	for _, p := range t.Partitions() {
		d.wg.Add(1)
		go func(p *stream.Partition) {
			defer d.wg.Done()
			d.runPartition(ctx, topic, p)
		}(p)
	}
	return nil
}

// Unwatch stops dispatching for a topic.
func (d *Dispatcher) Unwatch(topic string) {
	d.mu.Lock()
	if cancel, ok := d.watching[topic]; ok {
		cancel()
		delete(d.watching, topic)
	}
	d.mu.Unlock()
}

// WatchInboxes starts dispatching for every existing inbox topic and keeps
// checking for new ones on an interval.
//
// Polling for new topics is crude but correct: inbox topics are created lazily
// on a user's first message, and there is no cross-node topic-creation event to
// subscribe to. The interval only affects how quickly a brand-new user's first
// notification goes out.
func (d *Dispatcher) WatchInboxes(scanInterval time.Duration) {
	if scanInterval <= 0 {
		scanInterval = 30 * time.Second
	}
	d.scanInboxes()

	d.wg.Add(1)
	go func() {
		defer d.wg.Done()
		ticker := time.NewTicker(scanInterval)
		defer ticker.Stop()
		for {
			select {
			case <-d.ctx.Done():
				return
			case <-ticker.C:
				d.scanInboxes()
			}
		}
	}()
}

func (d *Dispatcher) scanInboxes() {
	for _, name := range d.log.TopicNames() {
		if strings.HasPrefix(name, fanout.TopicInboxPrefix) && len(name) > len(fanout.TopicInboxPrefix) {
			if err := d.Watch(name); err != nil {
				log.Printf("[outbox] watch %s: %v", name, err)
			}
		}
	}
}

// runPartition is the per-partition dispatch loop.
func (d *Dispatcher) runPartition(ctx context.Context, topic string, p *stream.Partition) {
	key := stream.CursorKey{Topic: topic, Partition: p.ID, Group: DispatcherGroup}

	// Start from the head, not from the beginning. A dispatcher starting fresh
	// on an existing deployment must not push every historical message.
	from := d.cursors.PositionOr(key, p.NextSeq())

	for {
		if ctx.Err() != nil {
			return
		}

		wake := p.NotifyChan()
		recs, err := p.Read(from, d.cfg.BatchSize, 4<<20)
		if err != nil {
			if errors.Is(err, stream.ErrSeqTruncated) {
				// Retention overtook the cursor: skip ahead rather than spin.
				from = p.FirstSeq()
				continue
			}
			if errors.Is(err, stream.ErrClosed) {
				return
			}
			log.Printf("[outbox] read %s/%d: %v", topic, p.ID, err)
			if !sleepCtx(ctx, d.cfg.RetryBackoff) {
				return
			}
			continue
		}

		if len(recs) == 0 {
			select {
			case <-wake:
			case <-ctx.Done():
				return
			}
			continue
		}

		last := recs[len(recs)-1].Seq
		batch := d.buildBatch(ctx, recs)

		if len(batch) > 0 {
			if !d.deliver(ctx, batch) {
				return // shutting down mid-retry
			}
		}

		from = last + 1
		if err := d.cursors.Commit(key, from); err != nil {
			log.Printf("[outbox] commit %s/%d: %v", topic, p.ID, err)
		}
	}
}

// buildBatch converts inbox pointer records into notifications, dropping those
// whose recipient is already connected.
func (d *Dispatcher) buildBatch(ctx context.Context, recs []*stream.Record) []Notification {
	batch := make([]Notification, 0, len(recs))
	now := time.Now()

	for _, r := range recs {
		d.bump(func(s *Stats) { s.Scanned++ })

		userID := string(r.Key)
		if userID == "" {
			continue
		}

		// The grace delay gives a user who is actively opening the app a
		// moment to read the message before a notification fires.
		if d.cfg.GraceDelay > 0 {
			age := now.Sub(time.Unix(0, r.Timestamp))
			if age < d.cfg.GraceDelay {
				if !sleepCtx(ctx, d.cfg.GraceDelay-age) {
					return batch
				}
			}
		}

		if d.presence != nil && d.presence.Online(ctx, userID) {
			d.bump(func(s *Stats) { s.Suppressed++ })
			continue
		}

		n := Notification{
			UserID:         userID,
			MessageID:      r.Headers[fanout.HeaderMessageID],
			ConversationID: r.Headers[fanout.HeaderConvID],
			Kind:           r.Headers[fanout.HeaderKind],
			SenderID:       r.Headers[fanout.HeaderSender],
			Headers:        r.Headers,
			At:             r.Timestamp,
		}
		if v := r.Headers[fanout.HeaderConvSeq]; v != "" {
			n.ConvSeq, _ = strconv.ParseUint(v, 10, 64)
		}
		if v := r.Headers[fanout.HeaderConvPart]; v != "" {
			if pid, err := strconv.ParseInt(v, 10, 32); err == nil {
				n.ConvPartition = int32(pid)
			}
		}
		if n.ConversationID != "" {
			kind := fanout.KindDirect
			if n.Kind == string(fanout.KindGroup) {
				kind = fanout.KindGroup
			}
			n.ConvTopic = fanout.ConversationTopic(kind, n.ConversationID)
		}
		batch = append(batch, n)
	}
	return batch
}

// deliver sends a batch with bounded retries. It returns false only when the
// dispatcher is shutting down.
func (d *Dispatcher) deliver(ctx context.Context, batch []Notification) bool {
	backoff := d.cfg.RetryBackoff

	for attempt := 1; attempt <= d.cfg.MaxAttempts; attempt++ {
		for i := range batch {
			batch[i].Attempt = attempt
		}

		err := d.notifier.Notify(ctx, batch)
		if err == nil {
			d.bump(func(s *Stats) { s.Sent += uint64(len(batch)) })
			return true
		}
		d.bump(func(s *Stats) { s.Failed++ })

		if attempt == d.cfg.MaxAttempts {
			break
		}
		log.Printf("[outbox] notify attempt %d/%d failed: %v", attempt, d.cfg.MaxAttempts, err)
		if !sleepCtx(ctx, backoff) {
			return false
		}
		backoff *= 2
		if backoff > d.cfg.MaxBackoff {
			backoff = d.cfg.MaxBackoff
		}
	}

	// Give up on this batch and advance. Blocking here would stall every
	// notification behind a single poisoned batch.
	log.Printf("[outbox] dropping %d notifications after %d attempts", len(batch), d.cfg.MaxAttempts)
	d.bump(func(s *Stats) { s.Dropped += uint64(len(batch)) })
	return true
}

func (d *Dispatcher) bump(f func(*Stats)) {
	d.mu.Lock()
	f(&d.stats)
	d.mu.Unlock()
}

// Stats returns a snapshot of dispatcher counters.
func (d *Dispatcher) Stats() Stats {
	d.mu.Lock()
	defer d.mu.Unlock()
	s := d.stats
	s.Watching = len(d.watching)
	return s
}

// Close stops every dispatch loop and waits for them to finish.
func (d *Dispatcher) Close() {
	d.stopOnce.Do(func() {
		d.cancel()
		d.wg.Wait()
	})
}

func sleepCtx(ctx context.Context, d time.Duration) bool {
	if d <= 0 {
		return ctx.Err() == nil
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return true
	case <-ctx.Done():
		return false
	}
}
