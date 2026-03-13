package storage

import (
	"sync"

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

func (m *MemoryStorage) ReadAll() ([]*protocol.Message, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	result := make([]*protocol.Message, len(m.messages))
	copy(result, m.messages)
	return result, nil
}

func (m *MemoryStorage) Close() error {
	return nil
}
