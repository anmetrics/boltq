package storage

import (
	"github.com/boltq/boltq/internal/wal"
	"github.com/boltq/boltq/pkg/protocol"
)

// Storage defines the interface for message persistence.
type Storage interface {
	// Write persists a message to storage. Returns number of bytes written.
	Write(msg *protocol.Message) (int, error)

	// Ack persists an acknowledgment to storage. Returns number of bytes written.
	Ack(msgID string) (int, error)

	// ReadAllRecords reads all WAL records for recovery.
	ReadAllRecords() ([]*wal.WALRecord, error)

	// Close closes the storage.
	Close() error

	// Rewrite replaces the entire storage content with the given messages.
	// Used for log compaction.
	Rewrite(msgs []*protocol.Message) error

	// Size returns the current size of the storage in bytes.
	Size() int64
}

