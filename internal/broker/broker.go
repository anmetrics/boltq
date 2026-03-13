package broker

import (
	"fmt"
	"sync"
	"time"

	"github.com/boltq/boltq/internal/queue"
	"github.com/boltq/boltq/internal/storage"
	"github.com/boltq/boltq/pkg/protocol"
)

// QueueType represents the type of queue.
type QueueType int

const (
	WorkQueue QueueType = iota // 1 message -> 1 consumer
	PubSub                    // 1 message -> all subscribers
)

// ConsumerFunc is a callback for pub/sub subscribers.
type ConsumerFunc func(msg *protocol.Message)

// Subscriber represents a pub/sub subscriber.
type Subscriber struct {
	ID string
	Ch chan *protocol.Message
}

// Topic holds pub/sub subscribers.
type Topic struct {
	name        string
	subscribers map[string]*Subscriber
	mu          sync.RWMutex
}

// PendingMessage tracks messages waiting for ACK.
type PendingMessage struct {
	Message   *protocol.Message
	QueueName string
	DeliverAt time.Time
	AckDeadline time.Time
}

// Broker is the central message broker that manages work queues and pub/sub topics.
type Broker struct {
	queues      map[string]*queue.Queue
	topics      map[string]*Topic
	deadLetters map[string]*queue.Queue
	pending     map[string]*PendingMessage // message ID -> pending
	store       storage.Storage
	mu          sync.RWMutex
	pendingMu   sync.RWMutex
	maxRetry    int
	ackTimeout  time.Duration
	queueCap    int
}

// Config holds broker configuration.
type Config struct {
	MaxRetry   int
	AckTimeout time.Duration
	QueueCap   int
	Storage    storage.Storage
}

// New creates a new broker with the given config.
func New(cfg Config) *Broker {
	if cfg.MaxRetry <= 0 {
		cfg.MaxRetry = 5
	}
	if cfg.AckTimeout <= 0 {
		cfg.AckTimeout = 30 * time.Second
	}
	if cfg.QueueCap <= 0 {
		cfg.QueueCap = 1 << 20
	}
	return &Broker{
		queues:      make(map[string]*queue.Queue),
		topics:      make(map[string]*Topic),
		deadLetters: make(map[string]*queue.Queue),
		pending:     make(map[string]*PendingMessage),
		store:       cfg.Storage,
		maxRetry:    cfg.MaxRetry,
		ackTimeout:  cfg.AckTimeout,
		queueCap:    cfg.QueueCap,
	}
}

// getOrCreateQueue returns an existing queue or creates a new one.
func (b *Broker) getOrCreateQueue(name string) *queue.Queue {
	b.mu.Lock()
	defer b.mu.Unlock()
	q, ok := b.queues[name]
	if !ok {
		q = queue.New(name, b.queueCap)
		b.queues[name] = q
	}
	return q
}

// getOrCreateTopic returns an existing topic or creates a new one.
func (b *Broker) getOrCreateTopic(name string) *Topic {
	b.mu.Lock()
	defer b.mu.Unlock()
	t, ok := b.topics[name]
	if !ok {
		t = &Topic{
			name:        name,
			subscribers: make(map[string]*Subscriber),
		}
		b.topics[name] = t
	}
	return t
}

// getOrCreateDeadLetter returns the dead-letter queue for a given queue name.
func (b *Broker) getOrCreateDeadLetter(name string) *queue.Queue {
	dlName := name + "_dead_letter"
	b.mu.Lock()
	defer b.mu.Unlock()
	q, ok := b.deadLetters[dlName]
	if !ok {
		q = queue.New(dlName, b.queueCap)
		b.deadLetters[dlName] = q
	}
	return q
}

// Publish publishes a message to a work queue. Persists to storage if configured.
func (b *Broker) Publish(topic string, msg *protocol.Message) error {
	msg.Topic = topic
	msg.MaxRetry = b.maxRetry

	// Persist to WAL if storage is configured.
	if b.store != nil {
		if err := b.store.Write(msg); err != nil {
			return fmt.Errorf("storage write: %w", err)
		}
	}

	q := b.getOrCreateQueue(topic)
	if !q.Push(msg) {
		return fmt.Errorf("queue %s is full", topic)
	}
	return nil
}

// PublishTopic publishes a message to all subscribers of a pub/sub topic.
func (b *Broker) PublishTopic(topicName string, msg *protocol.Message) error {
	msg.Topic = topicName

	if b.store != nil {
		if err := b.store.Write(msg); err != nil {
			return fmt.Errorf("storage write: %w", err)
		}
	}

	t := b.getOrCreateTopic(topicName)
	t.mu.RLock()
	defer t.mu.RUnlock()

	for _, sub := range t.subscribers {
		select {
		case sub.Ch <- msg:
		default:
			// subscriber is slow, drop the message for this subscriber
		}
	}
	return nil
}

// Consume retrieves a message from a work queue (blocking).
func (b *Broker) Consume(topic string) *protocol.Message {
	q := b.getOrCreateQueue(topic)
	msg := q.Pop()
	if msg == nil {
		return nil
	}

	// Track pending ACK.
	b.pendingMu.Lock()
	b.pending[msg.ID] = &PendingMessage{
		Message:     msg,
		QueueName:   topic,
		DeliverAt:   time.Now(),
		AckDeadline: time.Now().Add(b.ackTimeout),
	}
	b.pendingMu.Unlock()

	return msg
}

// TryConsume retrieves a message from a work queue (non-blocking).
func (b *Broker) TryConsume(topic string) *protocol.Message {
	q := b.getOrCreateQueue(topic)
	msg := q.TryPop()
	if msg == nil {
		return nil
	}

	b.pendingMu.Lock()
	b.pending[msg.ID] = &PendingMessage{
		Message:     msg,
		QueueName:   topic,
		DeliverAt:   time.Now(),
		AckDeadline: time.Now().Add(b.ackTimeout),
	}
	b.pendingMu.Unlock()

	return msg
}

// Subscribe creates a subscription to a pub/sub topic. Returns a channel for receiving messages.
func (b *Broker) Subscribe(topicName string, subscriberID string, bufSize int) <-chan *protocol.Message {
	if bufSize <= 0 {
		bufSize = 256
	}
	t := b.getOrCreateTopic(topicName)
	ch := make(chan *protocol.Message, bufSize)

	t.mu.Lock()
	t.subscribers[subscriberID] = &Subscriber{
		ID: subscriberID,
		Ch: ch,
	}
	t.mu.Unlock()

	return ch
}

// Unsubscribe removes a subscriber from a topic.
func (b *Broker) Unsubscribe(topicName string, subscriberID string) {
	b.mu.RLock()
	t, ok := b.topics[topicName]
	b.mu.RUnlock()
	if !ok {
		return
	}

	t.mu.Lock()
	if sub, exists := t.subscribers[subscriberID]; exists {
		close(sub.Ch)
		delete(t.subscribers, subscriberID)
	}
	t.mu.Unlock()
}

// Ack acknowledges a message, removing it from the pending list.
func (b *Broker) Ack(messageID string) error {
	b.pendingMu.Lock()
	defer b.pendingMu.Unlock()
	if _, ok := b.pending[messageID]; !ok {
		return fmt.Errorf("message %s not found in pending", messageID)
	}
	delete(b.pending, messageID)
	return nil
}

// Nack negatively acknowledges a message, triggering retry or dead-letter.
func (b *Broker) Nack(messageID string) error {
	b.pendingMu.Lock()
	pm, ok := b.pending[messageID]
	if !ok {
		b.pendingMu.Unlock()
		return fmt.Errorf("message %s not found in pending", messageID)
	}
	delete(b.pending, messageID)
	b.pendingMu.Unlock()

	return b.retryOrDeadLetter(pm.Message, pm.QueueName)
}

// retryOrDeadLetter either retries the message or sends it to the dead-letter queue.
func (b *Broker) retryOrDeadLetter(msg *protocol.Message, queueName string) error {
	msg.Retry++
	if msg.Retry > msg.MaxRetry {
		dl := b.getOrCreateDeadLetter(queueName)
		dl.Push(msg)
		return nil
	}

	q := b.getOrCreateQueue(queueName)
	if !q.Push(msg) {
		return fmt.Errorf("queue %s is full during retry", queueName)
	}
	return nil
}

// GetPendingMessages returns all currently pending messages (for scheduler inspection).
func (b *Broker) GetPendingMessages() map[string]*PendingMessage {
	b.pendingMu.RLock()
	defer b.pendingMu.RUnlock()
	result := make(map[string]*PendingMessage, len(b.pending))
	for k, v := range b.pending {
		result[k] = v
	}
	return result
}

// RequeueTimedOut moves a timed-out pending message back to its queue for retry.
func (b *Broker) RequeueTimedOut(messageID string) error {
	b.pendingMu.Lock()
	pm, ok := b.pending[messageID]
	if !ok {
		b.pendingMu.Unlock()
		return fmt.Errorf("message %s not found", messageID)
	}
	delete(b.pending, messageID)
	b.pendingMu.Unlock()

	return b.retryOrDeadLetter(pm.Message, pm.QueueName)
}

// PurgeQueue removes all messages from the specified queue.
func (b *Broker) PurgeQueue(topic string) (int64, error) {
	b.mu.RLock()
	q, ok := b.queues[topic]
	b.mu.RUnlock()
	if !ok {
		return 0, fmt.Errorf("queue %s not found", topic)
	}
	count := q.Purge()
	return count, nil
}

// PurgeDeadLetters removes all messages from a dead-letter queue.
func (b *Broker) PurgeDeadLetters(topic string) (int64, error) {
	dlName := topic + "_dead_letter"
	b.mu.RLock()
	q, ok := b.deadLetters[dlName]
	b.mu.RUnlock()
	if !ok {
		return 0, fmt.Errorf("dead letter queue %s not found", dlName)
	}
	count := q.Purge()
	return count, nil
}

// Stats returns broker statistics.
type Stats struct {
	Queues       map[string]int64
	Topics       map[string]int
	DeadLetters  map[string]int64
	PendingCount int
}

func (b *Broker) Stats() Stats {
	b.mu.RLock()
	defer b.mu.RUnlock()

	s := Stats{
		Queues:      make(map[string]int64),
		Topics:      make(map[string]int),
		DeadLetters: make(map[string]int64),
	}

	for name, q := range b.queues {
		s.Queues[name] = q.Size()
	}
	for name, t := range b.topics {
		t.mu.RLock()
		s.Topics[name] = len(t.subscribers)
		t.mu.RUnlock()
	}
	for name, q := range b.deadLetters {
		s.DeadLetters[name] = q.Size()
	}
	b.pendingMu.RLock()
	s.PendingCount = len(b.pending)
	b.pendingMu.RUnlock()

	return s
}

// SnapshotQueues returns a snapshot of all queue contents (for Raft snapshots).
func (b *Broker) SnapshotQueues() map[string][]*protocol.Message {
	b.mu.RLock()
	defer b.mu.RUnlock()
	result := make(map[string][]*protocol.Message)
	for name, q := range b.queues {
		msgs := q.Drain()
		if len(msgs) > 0 {
			result[name] = msgs
		}
	}
	return result
}

// RestoreQueues rebuilds all queues from snapshot data.
func (b *Broker) RestoreQueues(data map[string][]*protocol.Message) {
	b.mu.Lock()
	defer b.mu.Unlock()
	for _, q := range b.queues {
		q.Close()
	}
	b.queues = make(map[string]*queue.Queue)
	for name, msgs := range data {
		q := queue.New(name, b.queueCap)
		for _, msg := range msgs {
			q.Push(msg)
		}
		b.queues[name] = q
	}
}

// Close closes all queues.
func (b *Broker) Close() {
	b.mu.Lock()
	defer b.mu.Unlock()

	for _, q := range b.queues {
		q.Close()
	}
	for _, q := range b.deadLetters {
		q.Close()
	}
	for _, t := range b.topics {
		t.mu.Lock()
		for _, sub := range t.subscribers {
			close(sub.Ch)
		}
		t.mu.Unlock()
	}
	if b.store != nil {
		b.store.Close()
	}
}
