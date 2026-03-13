package storage

import (
	"github.com/boltq/boltq/internal/wal"
	"github.com/boltq/boltq/pkg/protocol"
)

// DiskStorage implements Storage using WAL for persistence.
type DiskStorage struct {
	wal *wal.WAL
}

// NewDiskStorage creates a new disk-backed storage.
func NewDiskStorage(dataDir string) (*DiskStorage, error) {
	w, err := wal.New(dataDir)
	if err != nil {
		return nil, err
	}
	return &DiskStorage{wal: w}, nil
}

func (d *DiskStorage) Write(msg *protocol.Message) error {
	return d.wal.Write(msg)
}

func (d *DiskStorage) ReadAll() ([]*protocol.Message, error) {
	return d.wal.ReadAll()
}

func (d *DiskStorage) Close() error {
	return d.wal.Close()
}
