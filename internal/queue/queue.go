package queue

import (
	"sync"
	"sync/atomic"

	"github.com/boltq/boltq/pkg/protocol"
)

const defaultCapacity = 1 << 20 // 1M messages

// Queue is a high-performance, lock-free message queue backed by a ring buffer.
type Queue struct {
	name     string
	buf      []*protocol.Message
	mask     uint64
	head     uint64 // next write position
	tail     uint64 // next read position
	size     int64
	mu       sync.Mutex
	notEmpty *sync.Cond
	closed   int32
}

// New creates a new queue with the given name and capacity.
// Capacity is rounded up to the next power of two.
func New(name string, capacity int) *Queue {
	cap := nextPowerOfTwo(capacity)
	q := &Queue{
		name: name,
		buf:  make([]*protocol.Message, cap),
		mask: uint64(cap - 1),
	}
	q.notEmpty = sync.NewCond(&q.mu)
	return q
}

// NewDefault creates a queue with default capacity (1M messages).
func NewDefault(name string) *Queue {
	return New(name, defaultCapacity)
}

// Push adds a message to the queue. Returns false if the queue is full.
func (q *Queue) Push(msg *protocol.Message) bool {
	q.mu.Lock()
	if atomic.LoadInt64(&q.size) >= int64(len(q.buf)) {
		q.mu.Unlock()
		return false
	}
	pos := q.head & q.mask
	q.buf[pos] = msg
	q.head++
	atomic.AddInt64(&q.size, 1)
	q.notEmpty.Signal()
	q.mu.Unlock()
	return true
}

// Pop removes and returns the next message from the queue.
// Blocks until a message is available or the queue is closed.
func (q *Queue) Pop() *protocol.Message {
	q.mu.Lock()
	for atomic.LoadInt64(&q.size) == 0 {
		if atomic.LoadInt32(&q.closed) == 1 {
			q.mu.Unlock()
			return nil
		}
		q.notEmpty.Wait()
	}
	pos := q.tail & q.mask
	msg := q.buf[pos]
	q.buf[pos] = nil
	q.tail++
	atomic.AddInt64(&q.size, -1)
	q.mu.Unlock()
	return msg
}

// TryPop attempts to pop a message without blocking. Returns nil if empty.
func (q *Queue) TryPop() *protocol.Message {
	q.mu.Lock()
	if atomic.LoadInt64(&q.size) == 0 {
		q.mu.Unlock()
		return nil
	}
	pos := q.tail & q.mask
	msg := q.buf[pos]
	q.buf[pos] = nil
	q.tail++
	atomic.AddInt64(&q.size, -1)
	q.mu.Unlock()
	return msg
}

// Size returns the current number of messages in the queue.
func (q *Queue) Size() int64 {
	return atomic.LoadInt64(&q.size)
}

// Name returns the queue name.
func (q *Queue) Name() string {
	return q.name
}

// Close signals all waiting consumers to stop.
func (q *Queue) Close() {
	atomic.StoreInt32(&q.closed, 1)
	q.notEmpty.Broadcast()
}

// Purge removes all messages from the queue, returning the count of purged messages.
func (q *Queue) Purge() int64 {
	q.mu.Lock()
	defer q.mu.Unlock()
	count := atomic.LoadInt64(&q.size)
	for i := uint64(0); i < uint64(count); i++ {
		pos := (q.tail + i) & q.mask
		q.buf[pos] = nil
	}
	q.tail = q.head
	atomic.StoreInt64(&q.size, 0)
	return count
}

// Drain returns all messages in the queue without removing them (for snapshots).
func (q *Queue) Drain() []*protocol.Message {
	q.mu.Lock()
	defer q.mu.Unlock()
	count := atomic.LoadInt64(&q.size)
	msgs := make([]*protocol.Message, 0, count)
	for i := uint64(0); i < uint64(count); i++ {
		pos := (q.tail + i) & q.mask
		if q.buf[pos] != nil {
			msgs = append(msgs, q.buf[pos])
		}
	}
	return msgs
}

func nextPowerOfTwo(n int) int {
	if n <= 0 {
		return 1
	}
	n--
	n |= n >> 1
	n |= n >> 2
	n |= n >> 4
	n |= n >> 8
	n |= n >> 16
	n++
	return n
}
