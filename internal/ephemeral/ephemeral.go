// Package ephemeral carries signals that are worthless a second after they are
// sent: typing indicators, presence pings, "viewing this profile" markers.
//
// These outnumber real messages by an order of magnitude — a user types for ten
// seconds and emits a signal every second to send one message — but none of
// them are worth a disk write, a replication round trip, or a retry. Routing
// them through the durable log would mean the least valuable traffic in the
// system consuming most of its write capacity.
//
// So this is a separate path: memory only, best effort, rate limited, and
// dropped without ceremony when a subscriber cannot keep up. Nothing here is
// ever recovered after a restart, which is correct — a typing indicator from
// before the process died is not information, it is noise.
package ephemeral

import (
	"errors"
	"strings"
	"sync"
	"time"
)

var (
	// ErrRateLimited means the publisher exceeded its token budget.
	ErrRateLimited = errors.New("ephemeral: rate limit exceeded")
	// ErrPayloadTooLarge means the signal exceeded the size cap.
	ErrPayloadTooLarge = errors.New("ephemeral: payload too large")
	// ErrClosed means the hub has shut down.
	ErrClosed = errors.New("ephemeral: hub closed")
)

// Signal is one ephemeral message.
type Signal struct {
	Topic   string            `json:"topic"`
	Sender  string            `json:"sender"`
	Kind    string            `json:"kind,omitempty"`
	Payload []byte            `json:"payload,omitempty"`
	Headers map[string]string `json:"headers,omitempty"`
	At      int64             `json:"at"` // UnixNano
}

// Well-known signal kinds. Applications may define others.
const (
	KindTyping      = "typing"
	KindStopTyping  = "stop_typing"
	KindPresence    = "presence"
	KindReadReceipt = "read_receipt"
	KindViewing     = "viewing"
)

// Topic prefixes, matching identity.ChatPolicyRules.
const (
	TopicTypingPrefix   = "typing."
	TopicPresencePrefix = "presence."
)

// TypingTopic returns the ephemeral topic for a conversation's typing signals.
func TypingTopic(conversationID string) string { return TopicTypingPrefix + conversationID }

// Config tunes the hub.
type Config struct {
	// MaxPayload caps a signal's body. Ephemeral signals are metadata, not
	// content; a large payload here is either a bug or an attempt to use the
	// unmetered path for real data.
	MaxPayload int

	// RatePerSecond is the sustained per-publisher signal budget.
	RatePerSecond float64
	// Burst is the per-publisher bucket depth, allowing brief spikes.
	Burst float64

	// SubscriberBuffer bounds each subscriber's queue before drops begin.
	SubscriberBuffer int

	// LimiterIdleTTL is how long an inactive publisher's bucket is retained.
	LimiterIdleTTL time.Duration
}

func (c *Config) applyDefaults() {
	if c.MaxPayload <= 0 {
		c.MaxPayload = 4 << 10 // 4KB
	}
	if c.RatePerSecond <= 0 {
		// One signal every 200ms sustained. A typing indicator should be sent
		// at most every second or two; this leaves generous headroom while
		// still stopping a runaway client.
		c.RatePerSecond = 5
	}
	if c.Burst <= 0 {
		c.Burst = 20
	}
	if c.SubscriberBuffer <= 0 {
		c.SubscriberBuffer = 64
	}
	if c.LimiterIdleTTL <= 0 {
		c.LimiterIdleTTL = 5 * time.Minute
	}
}

// bucket is a token-bucket rate limiter.
//
// Tokens are computed lazily from elapsed time rather than refilled by a
// ticker: with hundreds of thousands of publishers, a goroutine or timer per
// bucket would cost more than the traffic it governs.
type bucket struct {
	tokens   float64
	last     time.Time
	rate     float64
	capacity float64
}

func (b *bucket) allow(now time.Time, cost float64) bool {
	elapsed := now.Sub(b.last).Seconds()
	if elapsed > 0 {
		b.tokens += elapsed * b.rate
		if b.tokens > b.capacity {
			b.tokens = b.capacity
		}
		b.last = now
	}
	if b.tokens < cost {
		return false
	}
	b.tokens -= cost
	return true
}

// Subscription is a handle on an ephemeral topic.
type Subscription struct {
	ID string
	// Owner is the user whose signals should not be echoed back to this
	// subscription. It is separate from ID because a subscription ID must be
	// unique per connection (a user with two devices holds two), while echo
	// suppression is per user.
	Owner   string
	Topic   string
	C       <-chan Signal
	ch      chan Signal
	hub     *Hub
	dropped uint64
	once    sync.Once
}

// Close releases the subscription. It is safe to call more than once, and
// safe to race with Hub.Close — whichever runs first closes the channel.
func (s *Subscription) Close() {
	s.hub.unsubscribe(s.Topic, s.ID)
	s.closeCh()
}

// closeCh closes the delivery channel exactly once. Every path that retires a
// subscription funnels through here: explicit Close, replacement by a
// reconnect, and hub shutdown can all reach the same subscription.
func (s *Subscription) closeCh() {
	s.once.Do(func() { close(s.ch) })
}

// Dropped reports how many signals were discarded because this subscriber was
// too slow. A non-zero value on a chat client means typing indicators are
// stuttering, not that messages were lost.
func (s *Subscription) Dropped() uint64 {
	s.hub.mu.RLock()
	defer s.hub.mu.RUnlock()
	return s.dropped
}

type topicSubs struct {
	subs map[string]*Subscription
}

// Hub routes ephemeral signals.
type Hub struct {
	cfg Config

	mu     sync.RWMutex
	topics map[string]*topicSubs
	closed bool

	limMu    sync.Mutex
	limiters map[string]*bucket

	stats Stats

	stopOnce sync.Once
	stop     chan struct{}
}

// Stats counts hub activity. Fields are guarded by Hub.mu.
type Stats struct {
	Published   uint64 `json:"published"`
	Delivered   uint64 `json:"delivered"`
	Dropped     uint64 `json:"dropped"`
	RateLimited uint64 `json:"rate_limited"`
	Topics      int    `json:"topics"`
	Subscribers int    `json:"subscribers"`
}

// New creates a hub.
func New(cfg Config) *Hub {
	cfg.applyDefaults()
	h := &Hub{
		cfg:      cfg,
		topics:   make(map[string]*topicSubs),
		limiters: make(map[string]*bucket),
		stop:     make(chan struct{}),
	}
	go h.gcLoop()
	return h
}

// Subscribe registers interest in a topic. The owner is the user behind the
// subscription; their own signals are not echoed back to them.
func (h *Hub) Subscribe(topic, subscriberID string) (*Subscription, error) {
	return h.SubscribeAs(topic, subscriberID, subscriberID)
}

// SubscribeAs registers interest with an explicit owner.
func (h *Hub) SubscribeAs(topic, subscriberID, owner string) (*Subscription, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.closed {
		return nil, ErrClosed
	}

	ts, ok := h.topics[topic]
	if !ok {
		ts = &topicSubs{subs: make(map[string]*Subscription)}
		h.topics[topic] = ts
	}
	// A repeated subscribe from the same ID replaces the old one — the usual
	// cause is a reconnect whose previous close has not been observed yet.
	if old, exists := ts.subs[subscriberID]; exists {
		delete(ts.subs, subscriberID)
		old.closeCh()
	}

	ch := make(chan Signal, h.cfg.SubscriberBuffer)
	sub := &Subscription{ID: subscriberID, Owner: owner, Topic: topic, C: ch, ch: ch, hub: h}
	ts.subs[subscriberID] = sub
	return sub, nil
}

func (h *Hub) unsubscribe(topic, subscriberID string) {
	h.mu.Lock()
	defer h.mu.Unlock()

	ts, ok := h.topics[topic]
	if !ok {
		return
	}
	delete(ts.subs, subscriberID)
	if len(ts.subs) == 0 {
		delete(h.topics, topic)
	}
}

// Publish broadcasts a signal to a topic's subscribers.
//
// Delivery is best effort by design. A subscriber whose buffer is full has the
// signal dropped rather than the publisher blocked: one stalled phone must
// never be able to slow down everyone else in a conversation.
func (h *Hub) Publish(sig Signal) error {
	if len(sig.Payload) > h.cfg.MaxPayload {
		return ErrPayloadTooLarge
	}
	if sig.At == 0 {
		sig.At = time.Now().UnixNano()
	}

	if sig.Sender != "" && !h.allow(sig.Sender, time.Now()) {
		h.mu.Lock()
		h.stats.RateLimited++
		h.mu.Unlock()
		return ErrRateLimited
	}

	h.mu.Lock()
	defer h.mu.Unlock()
	if h.closed {
		return ErrClosed
	}
	h.stats.Published++

	ts, ok := h.topics[sig.Topic]
	if !ok {
		return nil // nobody listening; not an error
	}

	for _, sub := range ts.subs {
		// The sender does not need their own typing indicator echoed back.
		if sig.Sender != "" && sub.Owner == sig.Sender {
			continue
		}
		select {
		case sub.ch <- sig:
			h.stats.Delivered++
		default:
			sub.dropped++
			h.stats.Dropped++
		}
	}
	return nil
}

// PublishTyping is the common case, spelled out.
func (h *Hub) PublishTyping(conversationID, senderID string, typing bool) error {
	kind := KindTyping
	if !typing {
		kind = KindStopTyping
	}
	return h.Publish(Signal{
		Topic:  TypingTopic(conversationID),
		Sender: senderID,
		Kind:   kind,
	})
}

func (h *Hub) allow(publisher string, now time.Time) bool {
	h.limMu.Lock()
	defer h.limMu.Unlock()

	b, ok := h.limiters[publisher]
	if !ok {
		b = &bucket{tokens: h.cfg.Burst, last: now, rate: h.cfg.RatePerSecond, capacity: h.cfg.Burst}
		h.limiters[publisher] = b
	}
	return b.allow(now, 1)
}

// SubscriberCount returns how many subscribers a topic has.
func (h *Hub) SubscriberCount(topic string) int {
	h.mu.RLock()
	defer h.mu.RUnlock()
	if ts, ok := h.topics[topic]; ok {
		return len(ts.subs)
	}
	return 0
}

// Stats returns a snapshot of hub counters.
func (h *Hub) Stats() Stats {
	h.mu.RLock()
	defer h.mu.RUnlock()
	s := h.stats
	s.Topics = len(h.topics)
	for _, ts := range h.topics {
		s.Subscribers += len(ts.subs)
	}
	return s
}

// TopicsWithPrefix lists live topics under a prefix, for the admin view.
func (h *Hub) TopicsWithPrefix(prefix string) []string {
	h.mu.RLock()
	defer h.mu.RUnlock()
	var out []string
	for t := range h.topics {
		if strings.HasPrefix(t, prefix) {
			out = append(out, t)
		}
	}
	return out
}

func (h *Hub) gcLoop() {
	ticker := time.NewTicker(time.Minute)
	defer ticker.Stop()
	for {
		select {
		case <-h.stop:
			return
		case <-ticker.C:
			h.gcLimiters()
		}
	}
}

// gcLimiters drops buckets for publishers that have gone quiet, so the limiter
// map tracks active users rather than every user who ever connected.
func (h *Hub) gcLimiters() int {
	cutoff := time.Now().Add(-h.cfg.LimiterIdleTTL)
	h.limMu.Lock()
	defer h.limMu.Unlock()

	removed := 0
	for k, b := range h.limiters {
		if b.last.Before(cutoff) {
			delete(h.limiters, k)
			removed++
		}
	}
	return removed
}

// Close shuts the hub down and releases every subscriber.
func (h *Hub) Close() {
	h.stopOnce.Do(func() {
		close(h.stop)
		h.mu.Lock()
		h.closed = true
		for _, ts := range h.topics {
			for _, sub := range ts.subs {
				sub.closeCh()
			}
		}
		h.topics = make(map[string]*topicSubs)
		h.mu.Unlock()
	})
}
