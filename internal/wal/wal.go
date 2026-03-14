package wal

import (
	"bufio"
	"encoding/binary"
	"fmt"
	"hash/crc32"
	"io"
	"os"
	"path/filepath"
	"sync"

	"github.com/boltq/boltq/pkg/protocol"
)

const (
	RecordPublish byte = 0x01
	RecordAck     byte = 0x02
)

type WALRecord struct {
	Type    byte
	Message *protocol.Message
	MsgID   string
}

const (
	walFileName = "queue.wal"
	headerSize  = 8 // 4 bytes length + 4 bytes CRC32
)

// WAL implements a Write-Ahead Log for message persistence.
// Format: [length:4][crc32:4][data:length]
type WAL struct {
	dir    string
	file   *os.File
	writer *bufio.Writer
	mu     sync.Mutex
}

// New creates a new WAL in the given directory.
func New(dir string) (*WAL, error) {
	if err := os.MkdirAll(dir, 0755); err != nil {
		return nil, fmt.Errorf("create wal dir: %w", err)
	}

	path := filepath.Join(dir, walFileName)
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR|os.O_APPEND, 0644)
	if err != nil {
		return nil, fmt.Errorf("open wal file: %w", err)
	}

	return &WAL{
		dir:    dir,
		file:   f,
		writer: bufio.NewWriterSize(f, 64*1024), // 64KB buffer
	}, nil
}

// Write appends a message to the WAL.
func (w *WAL) Write(msg *protocol.Message) error {
	data, err := msg.Encode()
	if err != nil {
		return fmt.Errorf("encode message: %w", err)
	}
	return w.writeRecord(RecordPublish, data)
}

// WriteAck appends an acknowledgment to the WAL.
func (w *WAL) WriteAck(msgID string) error {
	return w.writeRecord(RecordAck, []byte(msgID))
}

func (w *WAL) writeRecord(typ byte, data []byte) error {
	w.mu.Lock()
	defer w.mu.Unlock()

	// Write length (4 bytes, little-endian).
	// Length is type (1) + data length
	var header [headerSize]byte
	binary.LittleEndian.PutUint32(header[:4], uint32(1+len(data)))

	// Checksum over type + data
	h := crc32.NewIEEE()
	h.Write([]byte{typ})
	h.Write(data)
	binary.LittleEndian.PutUint32(header[4:8], h.Sum32())

	if _, err := w.writer.Write(header[:]); err != nil {
		return fmt.Errorf("write header: %w", err)
	}
	if _, err := w.writer.Write([]byte{typ}); err != nil {
		return fmt.Errorf("write type: %w", err)
	}
	if _, err := w.writer.Write(data); err != nil {
		return fmt.Errorf("write data: %w", err)
	}

	return w.writer.Flush()
}

// ReadAllRecords reads all records from the WAL file (for recovery).
func (w *WAL) ReadAllRecords() ([]*WALRecord, error) {
	w.mu.Lock()
	defer w.mu.Unlock()

	// Flush any buffered writes first.
	if err := w.writer.Flush(); err != nil {
		return nil, fmt.Errorf("flush: %w", err)
	}

	path := filepath.Join(w.dir, walFileName)
	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("open wal for read: %w", err)
	}
	defer f.Close()

	reader := bufio.NewReaderSize(f, 64*1024)
	var records []*WALRecord

	for {
		var header [headerSize]byte
		if _, err := io.ReadFull(reader, header[:]); err != nil {
			if err == io.EOF || err == io.ErrUnexpectedEOF {
				break
			}
			return nil, fmt.Errorf("read header: %w", err)
		}

		totalLen := binary.LittleEndian.Uint32(header[:4])
		checksum := binary.LittleEndian.Uint32(header[4:8])

		if totalLen < 1 {
			break // invalid
		}

		typ, err := reader.ReadByte()
		if err != nil {
			break
		}

		dataLen := totalLen - 1
		data := make([]byte, dataLen)
		if _, err := io.ReadFull(reader, data); err != nil {
			break
		}

		// Verify checksum.
		h := crc32.NewIEEE()
		h.Write([]byte{typ})
		h.Write(data)
		if h.Sum32() != checksum {
			break // corrupted
		}

		switch typ {
		case RecordPublish:
			msg, err := protocol.DecodeMessage(data)
			if err == nil {
				records = append(records, &WALRecord{Type: typ, Message: msg})
			}
		case RecordAck:
			records = append(records, &WALRecord{Type: typ, MsgID: string(data)})
		}
	}

	return records, nil
}

// ReadAll is deprecated, use ReadAllRecords.
func (w *WAL) ReadAll() ([]*protocol.Message, error) {
	records, err := w.ReadAllRecords()
	if err != nil {
		return nil, err
	}
	var msgs []*protocol.Message
	for _, r := range records {
		if r.Type == RecordPublish {
			msgs = append(msgs, r.Message)
		}
	}
	return msgs, nil
}

// Sync forces a flush to disk.
func (w *WAL) Sync() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if err := w.writer.Flush(); err != nil {
		return err
	}
	return w.file.Sync()
}

// Close flushes and closes the WAL file.
func (w *WAL) Close() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if err := w.writer.Flush(); err != nil {
		return err
	}
	return w.file.Close()
}

// Truncate resets the WAL file (e.g., after compaction).
func (w *WAL) Truncate() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if err := w.writer.Flush(); err != nil {
		return err
	}
	if err := w.file.Truncate(0); err != nil {
		return err
	}
	_, err := w.file.Seek(0, 0)
	w.writer.Reset(w.file)
	return err
}
