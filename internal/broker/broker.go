package broker

import (
	"bufio"
	"encoding/binary"
	"fmt"
	"io"
	"os"
	"path/filepath"
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
	delayed     []*protocol.Message        // messages waiting for delivery
	durableSubs map[string]map[string]bool // topic -> subID -> isDurable
	spillDir    string
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
	spillDir := "./data/spill"
	if cfg.Storage != nil {
		// Try to get data dir from storage if possible, but for now use default
	}

	return &Broker{
		queues:      make(map[string]*queue.Queue),
		topics:      make(map[string]*Topic),
		deadLetters: make(map[string]*queue.Queue),
		pending:     make(map[string]*PendingMessage),
		delayed:     make([]*protocol.Message, 0),
		durableSubs: make(map[string]map[string]bool),
		spillDir:    spillDir,
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

	now := time.Now().UnixNano()
	if msg.Delay > 0 {
		msg.DeliverAt = now + msg.Delay
	}
	if msg.TTL > 0 {
		msg.ExpiresAt = now + msg.TTL
	}

	// Persist to WAL if storage is configured.
	if b.store != nil {
		if err := b.store.Write(msg); err != nil {
			return fmt.Errorf("storage write: %w", err)
		}
	}

	if msg.DeliverAt > now {
		b.mu.Lock()
		b.delayed = append(b.delayed, msg)
		b.mu.Unlock()
		return nil
	}

	q := b.getOrCreateQueue(topic)
	if !q.Push(msg) {
		// Spill to disk if queue is full
		return b.spillToDisk(topic, msg)
	}
	return nil
}

// IngestRecovered adds a message to the broker without writing to WAL.
// Used during startup recovery.
func (b *Broker) IngestRecovered(msg *protocol.Message) {
	q := b.getOrCreateQueue(msg.Topic)
	if !q.Push(msg) {
		b.spillToDisk(msg.Topic, msg)
	}
}

func (b *Broker) spillToDisk(topic string, msg *protocol.Message) error {
	b.mu.Lock()
	defer b.mu.Unlock()

	if err := os.MkdirAll(b.spillDir, 0755); err != nil {
		return fmt.Errorf("create spill dir: %w", err)
	}

	path := filepath.Join(b.spillDir, topic+".spill")
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0644)
	if err != nil {
		return fmt.Errorf("open spill file: %w", err)
	}
	defer f.Close()

	data, err := msg.Encode()
	if err != nil {
		return err
	}

	// Simple format: [length:4][data]
	var lenBuf [4]byte
	binary.LittleEndian.PutUint32(lenBuf[:], uint32(len(data)))
	if _, err := f.Write(lenBuf[:]); err != nil {
		return err
	}
	if _, err := f.Write(data); err != nil {
		return err
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

	// Push to active subscribers
	for _, sub := range t.subscribers {
		select {
		case sub.Ch <- msg:
		default:
			// subscriber is slow, drop the message for this subscriber
		}
	}

	// Push to durable queues
	b.mu.RLock()
	subs := b.durableSubs[topicName]
	b.mu.RUnlock()

	for subID := range subs {
		// Durable subscription name is "topic:subID"
		qName := fmt.Sprintf("%s:%s", topicName, subID)
		q := b.getOrCreateQueue(qName)
		if !q.Push(msg) {
			b.spillToDisk(qName, msg)
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
	for {
		msg := q.TryPop()
		if msg == nil {
			return nil
		}

		// Check TTL
		if msg.ExpiresAt > 0 && time.Now().UnixNano() > msg.ExpiresAt {
			continue // skip expired message
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
}

// ProcessAdvancedFeatures moves delayed messages to active queues and purges expired pending messages.
func (b *Broker) ProcessAdvancedFeatures() {
	now := time.Now().UnixNano()

	// 1. Move delayed messages to active queues
	b.mu.Lock()
	var remaining []*protocol.Message
	var ready []*protocol.Message
	for _, msg := range b.delayed {
		if now >= msg.DeliverAt {
			ready = append(ready, msg)
		} else {
			remaining = append(remaining, msg)
		}
	}
	b.delayed = remaining
	b.mu.Unlock()

	for _, msg := range ready {
		q := b.getOrCreateQueue(msg.Topic)
		q.Push(msg)
	}

	// 2. Clear expired pending messages
	b.pendingMu.Lock()
	for id, pm := range b.pending {
		if pm.Message.ExpiresAt > 0 && now > pm.Message.ExpiresAt {
			delete(b.pending, id)
		}
	}
	b.pendingMu.Unlock()

	// 3. Reload from spill files if there is space
	b.reloadFromDisk()
}

func (b *Broker) reloadFromDisk() {
	b.mu.RLock()
	topics := make([]string, 0, len(b.queues))
	for name := range b.queues {
		topics = append(topics, name)
	}
	b.mu.RUnlock()

	for _, topic := range topics {
		q := b.getOrCreateQueue(topic)
		if q.IsFull() {
			continue
		}

		path := filepath.Join(b.spillDir, topic+".spill")
		if _, err := os.Stat(path); os.IsNotExist(err) {
			continue
		}

		b.mu.Lock()
		b.processSpillFile(topic, q, path)
		b.mu.Unlock()
	}
}

func (b *Broker) processSpillFile(topic string, q *queue.Queue, path string) {
	f, err := os.Open(path)
	if err != nil {
		return
	}
	defer f.Close()

	tempPath := path + ".tmp"
	tf, err := os.Create(tempPath)
	if err != nil {
		return
	}
	defer tf.Close()

	reader := bufio.NewReader(f)
	reloadedCount := 0

	for !q.IsFull() {
		var lenBuf [4]byte
		if _, err := io.ReadFull(reader, lenBuf[:]); err != nil {
			break
		}
		length := binary.LittleEndian.Uint32(lenBuf[:])
		data := make([]byte, length)
		if _, err := io.ReadFull(reader, data); err != nil {
			break
		}

		msg, err := protocol.DecodeMessage(data)
		if err != nil {
			continue
		}

		if q.Push(msg) {
			reloadedCount++
		} else {
			// Write back to temp file if queue became full unexpectedly
			binary.LittleEndian.PutUint32(lenBuf[:], uint32(len(data)))
			tf.Write(lenBuf[:])
			tf.Write(data)
			break
		}
	}

	// Copy remaining messages to temp file
	io.Copy(tf, reader)
	tf.Close()
	f.Close()

	if reloadedCount > 0 {
		info, _ := os.Stat(tempPath)
		if info.Size() == 0 {
			os.Remove(tempPath)
			os.Remove(path)
		} else {
			os.Rename(tempPath, path)
		}
	} else {
		os.Remove(tempPath)
	}
}

// Subscribe creates a subscription to a pub/sub topic. Returns a channel for receiving messages.
func (b *Broker) Subscribe(topicName string, subscriberID string, bufSize int, durable bool) <-chan *protocol.Message {
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

	if durable {
		b.mu.Lock()
		if _, ok := b.durableSubs[topicName]; !ok {
			b.durableSubs[topicName] = make(map[string]bool)
		}
		b.durableSubs[topicName][subscriberID] = true
		b.mu.Unlock()

		// If there are missed messages in the durable queue, push them to the channel
		go b.catchUpDurable(topicName, subscriberID, ch)
	}
	return ch
}

func (b *Broker) catchUpDurable(topicName string, subscriberID string, ch chan *protocol.Message) {
	qName := fmt.Sprintf("%s:%s", topicName, subscriberID)
	q := b.getOrCreateQueue(qName)

	for {
		// Check if the subscriber still exists and is using THIS channel
		b.mu.RLock()
		t, ok := b.topics[topicName]
		b.mu.RUnlock()
		if !ok {
			return
		}

		t.mu.RLock()
		sub, exists := t.subscribers[subscriberID]
		// Important: Check if the channel is the same. Subscriber might have
		// reconnected and a newer catchUpDurable might be running for a NEW channel.
		if !exists || sub.Ch != ch {
			t.mu.RUnlock()
			return
		}
		t.mu.RUnlock()

		msg := q.TryPop()
		if msg == nil {
			break
		}

		select {
		case ch <- msg:
			// Success, continue looping
		case <-time.After(5 * time.Second):
			// Timeout or subscriber slow, put back and return to avoid goroutine leak
			q.Push(msg)
			return
		}
	}
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
