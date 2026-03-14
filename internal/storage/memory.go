package storage

import (
	"sync"

	"github.com/boltq/boltq/internal/wal"
	"github.com/boltq/boltq/pkg/protocol"
)

// MemoryStorage implements Storage with no persistence (memory only).
type MemoryStorage struct {
	messages []*protocol.Message
	mu       sync.Mutex
}

// NewMemoryStorage creates a new memory-only storage.
func NewMemoryStorage() *MemoryStorage {
	return &MemoryStorage{}
}

func (m *MemoryStorage) Write(msg *protocol.Message) error {
	m.mu.Lock()
	m.messages = append(m.messages, msg)
	m.mu.Unlock()
	return nil
}

func (m *MemoryStorage) ReadAllRecords() ([]*wal.WALRecord, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	result := make([]*wal.WALRecord, len(m.messages))
	for i, msg := range m.messages {
		result[i] = &wal.WALRecord{
			Type:    wal.RecordPublish,
			Message: msg,
		}
	}
	return result, nil
}

func (m *MemoryStorage) Ack(msgID string) error {
	// For memory storage, we don't strictly need to track ACKs for recovery if we don't persist,
	// but we implement it for interface compatibility.
	return nil
}

func (m *MemoryStorage) Close() error {
	return nil
}
