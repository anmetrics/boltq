package queue

import (
	"fmt"
	"sync"
	"sync/atomic"

	"github.com/boltq/boltq/pkg/protocol"
)

// NumPriorityLevels is the number of priority levels (0-9).
const NumPriorityLevels = 10

// PriorityQueue wraps multiple ring buffer queues, one per priority level.
// Higher priority messages (9) are consumed before lower priority (0).
type PriorityQueue struct {
	qname    string
	levels   [NumPriorityLevels]*Queue
	size     int64
	mu       sync.Mutex
	notEmpty *sync.Cond
	closed   int32
}

// NewPriority creates a new priority queue with the given capacity per level.
func NewPriority(name string, capacityPerLevel int) *PriorityQueue {
	pq := &PriorityQueue{qname: name}
	for i := 0; i < NumPriorityLevels; i++ {
		pq.levels[i] = New(fmt.Sprintf("%s_p%d", name, i), capacityPerLevel)
	}
	pq.notEmpty = sync.NewCond(&pq.mu)
	return pq
}

func clampPriority(p int) int {
	if p < 0 {
		return 0
	}
	if p >= NumPriorityLevels {
		return NumPriorityLevels - 1
	}
	return p
}

// Push adds a message to the appropriate priority level.
func (pq *PriorityQueue) Push(msg *protocol.Message) bool {
	pri := clampPriority(msg.Priority)
	pq.mu.Lock()
	if !pq.levels[pri].pushInternal(msg) {
		pq.mu.Unlock()
		return false
	}
	atomic.AddInt64(&pq.size, 1)
	pq.notEmpty.Signal()
	pq.mu.Unlock()
	return true
}

// Pop removes and returns the highest priority message. Blocks until available.
func (pq *PriorityQueue) Pop() *protocol.Message {
	pq.mu.Lock()
	for atomic.LoadInt64(&pq.size) == 0 {
		if atomic.LoadInt32(&pq.closed) == 1 {
			pq.mu.Unlock()
			return nil
		}
		pq.notEmpty.Wait()
	}
	msg := pq.popHighest()
	pq.mu.Unlock()
	return msg
}

// TryPop returns the highest priority message without blocking. Returns nil if empty.
func (pq *PriorityQueue) TryPop() *protocol.Message {
	pq.mu.Lock()
	if atomic.LoadInt64(&pq.size) == 0 {
		pq.mu.Unlock()
		return nil
	}
	msg := pq.popHighest()
	pq.mu.Unlock()
	return msg
}

// popHighest scans from level 9 down to 0 and pops the first available message.
// Caller must hold pq.mu.
func (pq *PriorityQueue) popHighest() *protocol.Message {
	for pri := NumPriorityLevels - 1; pri >= 0; pri-- {
		if pq.levels[pri].sizeInternal() > 0 {
			msg := pq.levels[pri].tryPopInternal()
			if msg != nil {
				atomic.AddInt64(&pq.size, -1)
				return msg
			}
		}
	}
	return nil
}

func (pq *PriorityQueue) Size() int64 {
	return atomic.LoadInt64(&pq.size)
}

func (pq *PriorityQueue) Name() string {
	return pq.qname
}

func (pq *PriorityQueue) Capacity() int64 {
	var total int64
	for i := 0; i < NumPriorityLevels; i++ {
		total += pq.levels[i].Capacity()
	}
	return total
}

func (pq *PriorityQueue) IsFull() bool {
	pq.mu.Lock()
	defer pq.mu.Unlock()
	for i := 0; i < NumPriorityLevels; i++ {
		if pq.levels[i].sizeInternal() < int64(len(pq.levels[i].buf)) {
			return false
		}
	}
	return true
}

func (pq *PriorityQueue) Close() {
	atomic.StoreInt32(&pq.closed, 1)
	pq.notEmpty.Broadcast()
}

func (pq *PriorityQueue) Purge() int64 {
	pq.mu.Lock()
	defer pq.mu.Unlock()
	var total int64
	for i := 0; i < NumPriorityLevels; i++ {
		count := pq.levels[i].sizeInternal()
		if count > 0 {
			// Reset ring buffer positions
			for j := uint64(0); j < uint64(count); j++ {
				pos := (pq.levels[i].tail + j) & pq.levels[i].mask
				pq.levels[i].buf[pos] = nil
			}
			pq.levels[i].tail = pq.levels[i].head
			atomic.StoreInt64(&pq.levels[i].size, 0)
			total += count
		}
	}
	atomic.StoreInt64(&pq.size, 0)
	return total
}

func (pq *PriorityQueue) Drain() []*protocol.Message {
	pq.mu.Lock()
	defer pq.mu.Unlock()
	var msgs []*protocol.Message
	// Drain from highest to lowest priority
	for pri := NumPriorityLevels - 1; pri >= 0; pri-- {
		count := pq.levels[pri].sizeInternal()
		for j := uint64(0); j < uint64(count); j++ {
			pos := (pq.levels[pri].tail + j) & pq.levels[pri].mask
			if pq.levels[pri].buf[pos] != nil {
				msgs = append(msgs, pq.levels[pri].buf[pos])
			}
		}
	}
	return msgs
}

func (pq *PriorityQueue) HasMessage(id string) bool {
	pq.mu.Lock()
	defer pq.mu.Unlock()
	for i := 0; i < NumPriorityLevels; i++ {
		count := pq.levels[i].sizeInternal()
		for j := uint64(0); j < uint64(count); j++ {
			pos := (pq.levels[i].tail + j) & pq.levels[i].mask
			if pq.levels[i].buf[pos] != nil && pq.levels[i].buf[pos].ID == id {
				return true
			}
		}
	}
	return false
}
