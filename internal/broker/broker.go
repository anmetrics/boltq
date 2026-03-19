package broker

import (
	"bufio"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
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
	queues      map[string]queue.MessageQueue
	topics      map[string]*Topic
	deadLetters map[string]queue.MessageQueue
	exchanges   map[string]*Exchange // exchange name -> Exchange
	pending     map[string]*PendingMessage // message ID -> pending
	delayed     []*protocol.Message        // messages waiting for delivery
	durableSubs map[string]map[string]bool // topic -> subID -> isDurable
	spillDir    string
	store       storage.Storage
	mu          sync.RWMutex
	pendingMu   sync.RWMutex
	maxRetry    int
	ackTimeout  time.Duration
	queueCap            int
	compactionThreshold int64
	checkpointing       int32
	needsCompaction     int32
	walSize             int64 // atomic approximate size
}

// Config holds broker configuration.
type Config struct {
	MaxRetry            int
	AckTimeout          time.Duration
	QueueCap            int
	Storage             storage.Storage
	CompactionThreshold int64
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

	b := &Broker{
		queues:              make(map[string]queue.MessageQueue),
		topics:              make(map[string]*Topic),
		deadLetters:         make(map[string]queue.MessageQueue),
		exchanges:           make(map[string]*Exchange),
		pending:             make(map[string]*PendingMessage),
		delayed:             make([]*protocol.Message, 0),
		durableSubs:         make(map[string]map[string]bool),
		spillDir:            spillDir,
		store:               cfg.Storage,
		maxRetry:            cfg.MaxRetry,
		ackTimeout:          cfg.AckTimeout,
		queueCap:            cfg.QueueCap,
		compactionThreshold: cfg.CompactionThreshold,
	}
	// Create default exchange (direct type, routes by queue name)
	b.exchanges[""] = &Exchange{Name: "", Type: ExchangeDirect, Durable: true}

	if b.store != nil {
		b.walSize = b.store.Size()
	}
	return b
}

// getOrCreateQueue returns an existing queue or creates a new one.
func (b *Broker) getOrCreateQueue(name string) queue.MessageQueue {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.getOrCreateQueueLocked(name)
}

func (b *Broker) getOrCreateQueueLocked(name string) queue.MessageQueue {
	q, ok := b.queues[name]
	if !ok {
		q = queue.NewPriority(name, b.queueCap)
		b.queues[name] = q
	}
	return q
}

// getOrCreateTopic returns an existing topic or creates a new one.
func (b *Broker) getOrCreateTopic(name string) *Topic {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.getOrCreateTopicLocked(name)
}

func (b *Broker) getOrCreateTopicLocked(name string) *Topic {
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
func (b *Broker) getOrCreateDeadLetter(name string) queue.MessageQueue {
	dlName := name + "_dead_letter"
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.getOrCreateDeadLetterLocked(dlName)
}

func (b *Broker) getOrCreateDeadLetterLocked(dlName string) queue.MessageQueue {
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

	// Lock for the entire duration of WAL write + Memory update to avoid race with Checkpoint.
	b.mu.Lock()
	defer b.mu.Unlock()

	// Persist to WAL if storage is configured.
	if b.store != nil {
		n, err := b.store.Write(msg)
		if err != nil {
			return fmt.Errorf("storage write: %w", err)
		}
		atomic.AddInt64(&b.walSize, int64(n))
		// Trigger auto-compaction check (outside the lock preferably, but let's just use the flag)
		b.AutoCheckpoint()
	}

	if msg.DeliverAt > now {
		b.delayed = append(b.delayed, msg)
		return nil
	}

	q := b.getOrCreateQueueLocked(topic)
	if !q.Push(msg) {
		// Spill to disk if queue is full
		return b.spillToDiskLocked(topic, msg)
	}
	return nil
}

// PublishConfirm publishes a message and ensures it is fsynced to disk before returning.
// Used when publisher confirm mode is enabled.
func (b *Broker) PublishConfirm(topic string, msg *protocol.Message) error {
	if err := b.Publish(topic, msg); err != nil {
		return err
	}
	if b.store != nil {
		return b.store.Sync()
	}
	return nil
}

// IngestRecovered adds a message to the broker without writing to WAL.
// Used during startup recovery.
func (b *Broker) IngestRecovered(msg *protocol.Message) {
	now := time.Now().UnixNano()

	// Proactive TTL check during recovery: if message has already expired, discard it.
	if msg.ExpiresAt > 0 && msg.ExpiresAt <= now {
		return // Expired based on absolute time
	}
	if msg.TTL > 0 {
		expiresAt := msg.Timestamp + msg.TTL
		if expiresAt <= now {
			return // Expired based on relative TTL
		}
	}

	if msg.DeliverAt > now {
		b.mu.Lock()
		b.delayed = append(b.delayed, msg)
		b.mu.Unlock()
		return
	}

	q := b.getOrCreateQueue(msg.Topic)
	if !q.Push(msg) {
		b.spillToDisk(msg.Topic, msg)
	}
}

// Checkpoint triggers a WAL compaction by rewriting the entire storage with only active messages.
func (b *Broker) Checkpoint() error {
	if b.store == nil {
		return nil
	}

	// 1. Capture state under lock
	b.mu.Lock()
	b.pendingMu.Lock()

	var allMsgs []*protocol.Message
	for _, q := range b.queues {
		allMsgs = append(allMsgs, q.Drain()...)
	}
	for _, q := range b.deadLetters {
		allMsgs = append(allMsgs, q.Drain()...)
	}
	allMsgs = append(allMsgs, b.delayed...)
	for _, pm := range b.pending {
		allMsgs = append(allMsgs, pm.Message)
	}

	b.pendingMu.Unlock()
	b.mu.Unlock()

	// 2. Perform disk I/O outside of locks
	if err := b.store.Rewrite(allMsgs); err != nil {
		return err
	}

	// 3. Sync approximate size after rewrite
	atomic.StoreInt64(&b.walSize, b.store.Size())
	return nil
}

// AutoCheckpoint checks if the WAL size exceeds the threshold and triggers a checkpoint.
func (b *Broker) AutoCheckpoint() {
	if b.store == nil || b.compactionThreshold <= 0 {
		return
	}

	if atomic.LoadInt64(&b.walSize) <= b.compactionThreshold {
		return
	}

	// Mark that a compaction is needed.
	atomic.StoreInt32(&b.needsCompaction, 1)

	// Try to start the compaction if not already running.
	if !atomic.CompareAndSwapInt32(&b.checkpointing, 0, 1) {
		return
	}

	go func() {
		defer atomic.StoreInt32(&b.checkpointing, 0)
		
		// Continue running as long as compaction is needed.
		for atomic.CompareAndSwapInt32(&b.needsCompaction, 1, 0) {
			b.Checkpoint()
		}
	}()
}

func (b *Broker) spillToDisk(topic string, msg *protocol.Message) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.spillToDiskLocked(topic, msg)
}

func (b *Broker) spillToDiskLocked(topic string, msg *protocol.Message) error {
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

	b.mu.Lock()
	defer b.mu.Unlock()

	if b.store != nil {
		n, err := b.store.Write(msg)
		if err != nil {
			return fmt.Errorf("storage write: %w", err)
		}
		atomic.AddInt64(&b.walSize, int64(n))
		b.AutoCheckpoint()
	}

	t := b.getOrCreateTopicLocked(topicName)
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
	subs := b.durableSubs[topicName]
	for subID := range subs {
		// Durable subscription name is "topic:subID"
		qName := fmt.Sprintf("%s:%s", topicName, subID)
		q := b.getOrCreateQueueLocked(qName)
		if !q.Push(msg) {
			b.spillToDiskLocked(qName, msg)
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

// GetReadyDelayedMessages returns messages whose delay has expired but hasn't been promoted.
func (b *Broker) GetReadyDelayedMessages() []*protocol.Message {
	b.mu.RLock()
	defer b.mu.RUnlock()
	now := time.Now().UnixNano()
	var ready []*protocol.Message
	for _, msg := range b.delayed {
		if now >= msg.DeliverAt {
			ready = append(ready, msg)
		}
	}
	return ready
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

// PromoteDelayed moves a delayed message to its active queue by its ID.
func (b *Broker) PromoteDelayed(messageID string) error {
	b.mu.Lock()
	defer b.mu.Unlock()

	var msg *protocol.Message
	var index int = -1
	for i, m := range b.delayed {
		if m.ID == messageID {
			msg = m
			index = i
			break
		}
	}

	if msg == nil {
		return fmt.Errorf("delayed message %s not found", messageID)
	}

	// Remove from delayed
	b.delayed = append(b.delayed[:index], b.delayed[index+1:]...)

	expectedID := messageID // simplify
	_ = expectedID

	q := b.getOrCreateQueueLocked(msg.Topic)
	if !q.Push(msg) {
		b.spillToDiskLocked(msg.Topic, msg)
	}
	return nil
}

// HasMessage checks if a message exists in any active queue or pending list.
func (b *Broker) HasMessage(messageID string) bool {
	// Check pending
	b.pendingMu.RLock()
	if _, ok := b.pending[messageID]; ok {
		b.pendingMu.RUnlock()
		return true
	}
	b.pendingMu.RUnlock()

	// Check active queues (expensive, but necessary for idempotency)
	b.mu.RLock()
	defer b.mu.RUnlock()
	for _, q := range b.queues {
		if q.HasMessage(messageID) {
			return true
		}
	}
	return b.hasDelayedMessageLocked(messageID)
}

// HasDelayedMessage checks if a message exists in the delayed list.
func (b *Broker) HasDelayedMessage(messageID string) bool {
	b.mu.RLock()
	defer b.mu.RUnlock()
	return b.hasDelayedMessageLocked(messageID)
}

func (b *Broker) hasDelayedMessageLocked(messageID string) bool {
	for _, m := range b.delayed {
		if m.ID == messageID {
			return true
		}
	}
	return false
}

// reloadFromDisk reloads messages from spill files if there is space in the queue.
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

func (b *Broker) processSpillFile(topic string, q queue.MessageQueue, path string) {
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

	// Persist ACK to storage if configured.
	if b.store != nil {
		n, err := b.store.Ack(messageID)
		if err != nil {
			// Log error but don't fail ACK? Or fail?
			// For consistency, we should probably log it.
			fmt.Printf("[broker] storage ack error: %v\n", err)
		}
		atomic.AddInt64(&b.walSize, int64(n))
		b.AutoCheckpoint()
	}

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

// RegisterDurableSub registers a durable subscriber for a topic.
func (b *Broker) RegisterDurableSub(topicName, subscriberID string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.durableSubs[topicName] == nil {
		b.durableSubs[topicName] = make(map[string]bool)
	}
	b.durableSubs[topicName][subscriberID] = true
}

// UnregisterDurableSub removes a durable subscriber for a topic.
func (b *Broker) UnregisterDurableSub(topicName, subscriberID string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if subs, ok := b.durableSubs[topicName]; ok {
		delete(subs, subscriberID)
	}
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
	if count > 0 && b.store != nil {
		b.Checkpoint()
	}
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
	if count > 0 && b.store != nil {
		b.Checkpoint()
	}
	return count, nil
}

// Stats returns broker statistics.
type Stats struct {
	Queues       map[string]int64
	Topics       map[string]int
	DeadLetters  map[string]int64
	PendingCount int
	DelayedCount int
}

func (b *Broker) Stats() Stats {
	b.mu.RLock()
	defer b.mu.RUnlock()

	s := Stats{
		Queues:      make(map[string]int64),
		Topics:      make(map[string]int),
		DeadLetters: make(map[string]int64),
		DelayedCount: len(b.delayed),
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

// FullState represents the entire serializable state of the broker.
type FullState struct {
	Queues      map[string][]*protocol.Message `json:"queues"`
	DeadLetters map[string][]*protocol.Message `json:"dead_letters,omitempty"`
	Delayed     []*protocol.Message            `json:"delayed,omitempty"`
	Pending     map[string]*PendingMessage     `json:"pending,omitempty"`
	DurableSubs map[string]map[string]bool     `json:"durable_subs,omitempty"`
	Exchanges   map[string]*ExchangeState      `json:"exchanges,omitempty"`
}

// SnapshotFullState returns a snapshot of the entire broker state (for Raft snapshots).
func (b *Broker) SnapshotFullState() FullState {
	b.mu.RLock()
	defer b.mu.RUnlock()

	state := FullState{
		Queues:      make(map[string][]*protocol.Message),
		DeadLetters: make(map[string][]*protocol.Message),
		Delayed:     make([]*protocol.Message, len(b.delayed)),
	}

	for name, q := range b.queues {
		msgs := q.Drain()
		if len(msgs) > 0 {
			state.Queues[name] = msgs
		}
	}

	for name, q := range b.deadLetters {
		msgs := q.Drain()
		if len(msgs) > 0 {
			state.DeadLetters[name] = msgs
		}
	}

	copy(state.Delayed, b.delayed)

	b.pendingMu.RLock()
	state.Pending = make(map[string]*PendingMessage, len(b.pending))
	for k, v := range b.pending {
		state.Pending[k] = v
	}
	b.pendingMu.RUnlock()

	state.DurableSubs = make(map[string]map[string]bool)
	for t, subs := range b.durableSubs {
		m := make(map[string]bool)
		for k, v := range subs {
			m[k] = v
		}
		state.DurableSubs[t] = m
	}

	state.Exchanges = make(map[string]*ExchangeState)
	for name, ex := range b.exchanges {
		s := ex.toState()
		state.Exchanges[name] = &s
	}

	return state
}

// RestoreFullState rebuilds the broker state from snapshot data.
func (b *Broker) RestoreFullState(state FullState) {
	b.mu.Lock()
	defer b.mu.Unlock()

	// Shutdown existing queues
	for _, q := range b.queues {
		q.Close()
	}
	for _, q := range b.deadLetters {
		q.Close()
	}

	b.queues = make(map[string]queue.MessageQueue)
	for name, msgs := range state.Queues {
		q := queue.NewPriority(name, b.queueCap)
		for _, msg := range msgs {
			q.Push(msg)
		}
		b.queues[name] = q
	}

	b.deadLetters = make(map[string]queue.MessageQueue)
	for name, msgs := range state.DeadLetters {
		q := queue.New(name, b.queueCap)
		for _, msg := range msgs {
			q.Push(msg)
		}
		b.deadLetters[name] = q
	}

	b.delayed = state.Delayed

	b.pendingMu.Lock()
	b.pending = state.Pending
	if b.pending == nil {
		b.pending = make(map[string]*PendingMessage)
	}
	b.pendingMu.Unlock()

	if state.DurableSubs != nil {
		b.durableSubs = state.DurableSubs
	} else {
		b.durableSubs = make(map[string]map[string]bool)
	}

	b.exchanges = make(map[string]*Exchange)
	b.exchanges[""] = &Exchange{Name: "", Type: ExchangeDirect, Durable: true}
	if state.Exchanges != nil {
		for name, es := range state.Exchanges {
			b.exchanges[name] = &Exchange{
				Name:     es.Name,
				Type:     es.Type,
				Durable:  es.Durable,
				Bindings: es.Bindings,
			}
		}
	}
}

// StorageSize returns the current size of the underlying storage in bytes.
func (b *Broker) StorageSize() int64 {
	if b.store == nil {
		return 0
	}
	return atomic.LoadInt64(&b.walSize)
}

// StorageMode returns the storage mode (memory or disk).
func (b *Broker) StorageMode() string {
	if b.store == nil {
		return "memory"
	}
	// This is a bit of a hack, but DiskStorage implements Size() but doesn't expose its type easily.
	// However, we can check if it's nil or not.
	return "disk" // If store exists in current implementation, it's disk.
}

// CompactionThreshold returns the current compaction threshold.
func (b *Broker) CompactionThreshold() int64 {
	return b.compactionThreshold
}

// ExchangeDeclare creates a new exchange or asserts an existing one matches.
func (b *Broker) ExchangeDeclare(name string, typ ExchangeType, durable bool) error {
	b.mu.Lock()
	defer b.mu.Unlock()

	if ex, ok := b.exchanges[name]; ok {
		if ex.Type != typ {
			return fmt.Errorf("exchange %q already exists with type %s", name, ex.Type)
		}
		return nil // idempotent
	}

	b.exchanges[name] = &Exchange{
		Name:    name,
		Type:    typ,
		Durable: durable,
	}

	if durable && b.store != nil {
		data, _ := json.Marshal(b.exchanges[name].toState())
		b.store.WriteMetadata(walRecordExchangeDeclare, data)
	}
	return nil
}

// ExchangeDelete removes an exchange and all its bindings.
func (b *Broker) ExchangeDelete(name string) error {
	if name == "" {
		return fmt.Errorf("cannot delete the default exchange")
	}
	b.mu.Lock()
	defer b.mu.Unlock()

	if _, ok := b.exchanges[name]; !ok {
		return fmt.Errorf("exchange %q not found", name)
	}
	delete(b.exchanges, name)

	if b.store != nil {
		data, _ := json.Marshal(map[string]string{"name": name})
		b.store.WriteMetadata(walRecordExchangeDelete, data)
	}
	return nil
}

// BindQueue binds a queue to an exchange with the given binding key.
func (b *Broker) BindQueue(exchange, queueName, bindingKey string, headers map[string]string, matchAll bool) error {
	b.mu.Lock()
	defer b.mu.Unlock()

	ex, ok := b.exchanges[exchange]
	if !ok {
		return fmt.Errorf("exchange %q not found", exchange)
	}

	binding := &Binding{
		Exchange:   exchange,
		Queue:      queueName,
		BindingKey: bindingKey,
		Headers:    headers,
		MatchAll:   matchAll,
	}

	ex.mu.Lock()
	ex.Bindings = append(ex.Bindings, binding)
	ex.mu.Unlock()

	// Ensure the queue exists
	b.getOrCreateQueueLocked(queueName)

	if b.store != nil {
		data, _ := json.Marshal(binding)
		b.store.WriteMetadata(walRecordBindQueue, data)
	}
	return nil
}

// UnbindQueue removes a binding from an exchange.
func (b *Broker) UnbindQueue(exchange, queueName, bindingKey string) error {
	b.mu.Lock()
	defer b.mu.Unlock()

	ex, ok := b.exchanges[exchange]
	if !ok {
		return fmt.Errorf("exchange %q not found", exchange)
	}

	ex.mu.Lock()
	for i, bind := range ex.Bindings {
		if bind.Queue == queueName && bind.BindingKey == bindingKey {
			ex.Bindings = append(ex.Bindings[:i], ex.Bindings[i+1:]...)
			break
		}
	}
	ex.mu.Unlock()

	if b.store != nil {
		data, _ := json.Marshal(map[string]string{
			"exchange":    exchange,
			"queue":       queueName,
			"binding_key": bindingKey,
		})
		b.store.WriteMetadata(walRecordUnbindQueue, data)
	}
	return nil
}

// PublishExchange routes a message through an exchange to all matching bound queues.
func (b *Broker) PublishExchange(exchangeName, routingKey string, msg *protocol.Message) error {
	b.mu.RLock()
	ex, ok := b.exchanges[exchangeName]
	b.mu.RUnlock()
	if !ok {
		return fmt.Errorf("exchange %q not found", exchangeName)
	}

	msg.Exchange = exchangeName
	msg.RoutingKey = routingKey

	// Default exchange: route directly to queue named by routing key
	if exchangeName == "" {
		return b.Publish(routingKey, msg)
	}

	queues := ex.route(routingKey, msg.Headers)
	if len(queues) == 0 {
		return nil // no matching bindings, message is dropped
	}

	// Publish a clone to each matched queue (each gets its own ID for independent ACK)
	for _, qName := range queues {
		clone := *msg
		clone.ID = protocol.GenerateID()
		clone.Topic = qName
		if err := b.Publish(qName, &clone); err != nil {
			return fmt.Errorf("publish to queue %s: %w", qName, err)
		}
	}
	return nil
}

// toState converts an Exchange to its serializable form.
func (e *Exchange) toState() ExchangeState {
	e.mu.RLock()
	defer e.mu.RUnlock()
	return ExchangeState{
		Name:     e.Name,
		Type:     e.Type,
		Durable:  e.Durable,
		Bindings: e.Bindings,
	}
}

// WAL record types for exchange metadata.
const (
	walRecordExchangeDeclare byte = 0x03
	walRecordExchangeDelete  byte = 0x04
	walRecordBindQueue       byte = 0x05
	walRecordUnbindQueue     byte = 0x06
)

// ReplayMetadata replays a WAL metadata record during recovery to restore exchange/binding state.
func (b *Broker) ReplayMetadata(recordType byte, data []byte) {
	switch recordType {
	case walRecordExchangeDeclare:
		var es ExchangeState
		if json.Unmarshal(data, &es) == nil {
			b.mu.Lock()
			b.exchanges[es.Name] = &Exchange{
				Name:     es.Name,
				Type:     es.Type,
				Durable:  es.Durable,
				Bindings: es.Bindings,
			}
			b.mu.Unlock()
		}
	case walRecordExchangeDelete:
		var info struct {
			Name string `json:"name"`
		}
		if json.Unmarshal(data, &info) == nil {
			b.mu.Lock()
			delete(b.exchanges, info.Name)
			b.mu.Unlock()
		}
	case walRecordBindQueue:
		var binding Binding
		if json.Unmarshal(data, &binding) == nil {
			b.mu.Lock()
			if ex, ok := b.exchanges[binding.Exchange]; ok {
				ex.mu.Lock()
				ex.Bindings = append(ex.Bindings, &binding)
				ex.mu.Unlock()
			}
			b.mu.Unlock()
		}
	case walRecordUnbindQueue:
		var info struct {
			Exchange   string `json:"exchange"`
			Queue      string `json:"queue"`
			BindingKey string `json:"binding_key"`
		}
		if json.Unmarshal(data, &info) == nil {
			b.mu.Lock()
			if ex, ok := b.exchanges[info.Exchange]; ok {
				ex.mu.Lock()
				for i, bind := range ex.Bindings {
					if bind.Queue == info.Queue && bind.BindingKey == info.BindingKey {
						ex.Bindings = append(ex.Bindings[:i], ex.Bindings[i+1:]...)
						break
					}
				}
				ex.mu.Unlock()
			}
			b.mu.Unlock()
		}
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
