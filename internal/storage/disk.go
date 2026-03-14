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

// MustNewDiskStorage creates a new disk-backed storage or panics.
func MustNewDiskStorage(dataDir string) *DiskStorage {
	s, err := NewDiskStorage(dataDir)
	if err != nil {
		panic(err)
	}
	return s
}

func (d *DiskStorage) Write(msg *protocol.Message) error {
	return d.wal.Write(msg)
}

func (d *DiskStorage) ReadAllRecords() ([]*wal.WALRecord, error) {
	records, err := d.wal.ReadAllRecords()
	if err != nil {
		return nil, err
	}
	var res []*wal.WALRecord
	for _, r := range records {
		res = append(res, &wal.WALRecord{
			Type:    r.Type,
			Message: r.Message,
			MsgID:   r.MsgID,
		})
	}
	return res, nil
}

func (d *DiskStorage) Ack(msgID string) error {
	return d.wal.WriteAck(msgID)
}

func (d *DiskStorage) Close() error {
	return d.wal.Close()
}
