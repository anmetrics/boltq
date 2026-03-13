package storage

import "github.com/boltq/boltq/pkg/protocol"

// Storage defines the interface for message persistence.
type Storage interface {
	// Write persists a message to storage.
	Write(msg *protocol.Message) error

	// ReadAll reads all persisted messages (for recovery).
	ReadAll() ([]*protocol.Message, error)

	// Close closes the storage.
	Close() error
}
