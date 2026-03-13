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

	w.mu.Lock()
	defer w.mu.Unlock()

	// Write length (4 bytes, little-endian).
	var header [headerSize]byte
	binary.LittleEndian.PutUint32(header[:4], uint32(len(data)))
	// Write CRC32 checksum.
	binary.LittleEndian.PutUint32(header[4:8], crc32.ChecksumIEEE(data))

	if _, err := w.writer.Write(header[:]); err != nil {
		return fmt.Errorf("write header: %w", err)
	}
	if _, err := w.writer.Write(data); err != nil {
		return fmt.Errorf("write data: %w", err)
	}

	return w.writer.Flush()
}

// ReadAll reads all messages from the WAL file (for recovery).
func (w *WAL) ReadAll() ([]*protocol.Message, error) {
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
	var messages []*protocol.Message

	for {
		var header [headerSize]byte
		if _, err := io.ReadFull(reader, header[:]); err != nil {
			if err == io.EOF || err == io.ErrUnexpectedEOF {
				break
			}
			return nil, fmt.Errorf("read header: %w", err)
		}

		length := binary.LittleEndian.Uint32(header[:4])
		checksum := binary.LittleEndian.Uint32(header[4:8])

		data := make([]byte, length)
		if _, err := io.ReadFull(reader, data); err != nil {
			if err == io.EOF || err == io.ErrUnexpectedEOF {
				break // truncated entry, stop
			}
			return nil, fmt.Errorf("read data: %w", err)
		}

		// Verify checksum.
		if crc32.ChecksumIEEE(data) != checksum {
			break // corrupted entry, stop recovery here
		}

		msg, err := protocol.DecodeMessage(data)
		if err != nil {
			continue // skip malformed entries
		}
		messages = append(messages, msg)
	}

	return messages, nil
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
