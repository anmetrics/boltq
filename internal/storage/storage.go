package storage

import (
	"github.com/boltq/boltq/internal/wal"
	"github.com/boltq/boltq/pkg/protocol"
)

// Storage defines the interface for message persistence.
type Storage interface {
	// Write persists a message to storage.
	Write(msg *protocol.Message) error

	// Ack persists an acknowledgment to storage.
	Ack(msgID string) error

	// ReadAllRecords reads all WAL records for recovery.
	ReadAllRecords() ([]*wal.WALRecord, error)

	// Close closes the storage.
	Close() error
}

