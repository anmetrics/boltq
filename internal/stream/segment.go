package stream

import (
	"bufio"
	"encoding/binary"
	"fmt"
	"hash/crc32"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
)

const (
	logSuffix   = ".log"
	indexSuffix = ".index"
	// indexEntrySize is [seq:8][filePos:8].
	indexEntrySize = 16
	// segmentNameWidth zero-pads base sequence numbers so lexical filename
	// order matches numeric order — makes `ls` and glob-sorting correct.
	segmentNameWidth = 20
)

// indexEntry maps a sequence number to its byte offset in the log file.
type indexEntry struct {
	seq uint64
	pos int64
}

// segment is one contiguous chunk of a partition's log, backed by a pair of
// files: an append-only .log and a sparse .index.
//
// The index is sparse (one entry per indexInterval bytes) rather than dense
// because a chat partition holds millions of records and a dense index would
// cost as much RAM as the data. A read seeks to the nearest indexed record at
// or before the target sequence, then scans forward — bounded by indexInterval
// bytes, so a few hundred microseconds at worst.
type segment struct {
	dir      string
	baseSeq  uint64 // sequence of the first record in this segment
	readOnly bool

	mu       sync.RWMutex
	file     *os.File
	writer   *bufio.Writer
	idxFile  *os.File
	index    []indexEntry
	size     int64  // bytes written to .log
	lastSeq  uint64 // sequence of the last appended record; baseSeq-1 when empty
	sinceIdx int64  // bytes appended since the last index entry

	indexInterval int64
}

func segmentPaths(dir string, baseSeq uint64) (logPath, idxPath string) {
	name := fmt.Sprintf("%0*d", segmentNameWidth, baseSeq)
	return filepath.Join(dir, name+logSuffix), filepath.Join(dir, name+indexSuffix)
}

// parseSegmentBase extracts the base sequence from a segment log filename.
func parseSegmentBase(name string) (uint64, bool) {
	if !strings.HasSuffix(name, logSuffix) {
		return 0, false
	}
	v, err := strconv.ParseUint(strings.TrimSuffix(name, logSuffix), 10, 64)
	if err != nil {
		return 0, false
	}
	return v, true
}

// createSegment opens a new writable segment starting at baseSeq.
func createSegment(dir string, baseSeq uint64, indexInterval int64) (*segment, error) {
	logPath, idxPath := segmentPaths(dir, baseSeq)

	f, err := os.OpenFile(logPath, os.O_CREATE|os.O_RDWR|os.O_APPEND, 0644)
	if err != nil {
		return nil, fmt.Errorf("create segment log: %w", err)
	}
	idx, err := os.OpenFile(idxPath, os.O_CREATE|os.O_RDWR|os.O_APPEND, 0644)
	if err != nil {
		f.Close()
		return nil, fmt.Errorf("create segment index: %w", err)
	}

	s := &segment{
		dir:           dir,
		baseSeq:       baseSeq,
		file:          f,
		writer:        bufio.NewWriterSize(f, 256*1024),
		idxFile:       idx,
		lastSeq:       baseSeq - 1,
		indexInterval: indexInterval,
	}
	return s, nil
}

// openSegment opens an existing segment and recovers its state by scanning the
// log. Scanning is bounded to the tail when a usable index exists.
//
// Recovery truncates the log at the first corrupt or partial record. A torn
// final write is expected after an unclean shutdown — the record was never
// acknowledged to a publisher, so discarding it is correct and loses nothing a
// client believes was committed.
func openSegment(dir string, baseSeq uint64, indexInterval int64, readOnly bool) (*segment, error) {
	logPath, idxPath := segmentPaths(dir, baseSeq)

	flags := os.O_RDWR | os.O_APPEND
	if readOnly {
		flags = os.O_RDONLY
	}
	f, err := os.OpenFile(logPath, flags, 0644)
	if err != nil {
		return nil, fmt.Errorf("open segment log: %w", err)
	}

	s := &segment{
		dir:           dir,
		baseSeq:       baseSeq,
		file:          f,
		readOnly:      readOnly,
		lastSeq:       baseSeq - 1,
		indexInterval: indexInterval,
	}

	if err := s.loadIndex(idxPath); err != nil {
		f.Close()
		return nil, err
	}
	if err := s.recover(); err != nil {
		f.Close()
		return nil, err
	}

	if !readOnly {
		s.writer = bufio.NewWriterSize(f, 256*1024)
		idx, err := os.OpenFile(idxPath, os.O_CREATE|os.O_RDWR|os.O_APPEND, 0644)
		if err != nil {
			f.Close()
			return nil, fmt.Errorf("open segment index: %w", err)
		}
		s.idxFile = idx
	}
	return s, nil
}

func (s *segment) loadIndex(idxPath string) error {
	data, err := os.ReadFile(idxPath)
	if err != nil {
		if os.IsNotExist(err) {
			return nil // index will be rebuilt by recover()
		}
		return fmt.Errorf("read segment index: %w", err)
	}
	n := len(data) / indexEntrySize
	s.index = make([]indexEntry, 0, n)
	for i := 0; i < n; i++ {
		b := data[i*indexEntrySize:]
		s.index = append(s.index, indexEntry{
			seq: binary.LittleEndian.Uint64(b[0:8]),
			pos: int64(binary.LittleEndian.Uint64(b[8:16])),
		})
	}
	return nil
}

// recover scans from the last trustworthy index entry to the end of the log,
// establishing lastSeq and the true (post-truncation) size.
func (s *segment) recover() error {
	info, err := s.file.Stat()
	if err != nil {
		return fmt.Errorf("stat segment: %w", err)
	}
	fileSize := info.Size()

	var pos int64
	// Start from the last index entry that is actually within the file. An
	// index entry past EOF means the index was fsynced but the log was not.
	for i := len(s.index) - 1; i >= 0; i-- {
		if s.index[i].pos < fileSize {
			pos = s.index[i].pos
			s.lastSeq = s.index[i].seq - 1
			s.index = s.index[:i+1]
			break
		}
		s.index = s.index[:i]
	}

	reader := bufio.NewReaderSize(io.NewSectionReader(s.file, pos, fileSize-pos), 256*1024)
	var frame [recordFrameHeaderSize]byte

	for {
		if _, err := io.ReadFull(reader, frame[:]); err != nil {
			break // clean EOF or partial header — stop here
		}
		checksum := binary.LittleEndian.Uint32(frame[0:4])
		bodyLen := binary.LittleEndian.Uint32(frame[4:8])
		if bodyLen < recordBodyHeaderSize || bodyLen > MaxRecordSize {
			break // nonsense length: treat as torn write
		}
		body := make([]byte, bodyLen)
		if _, err := io.ReadFull(reader, body); err != nil {
			break
		}
		if crc32.ChecksumIEEE(body) != checksum {
			break
		}
		s.lastSeq = binary.LittleEndian.Uint64(body[0:8])
		pos += recordFrameHeaderSize + int64(bodyLen)
	}

	s.size = pos
	if pos != fileSize {
		if s.readOnly {
			// Cannot repair a read-only segment; surface the healthy prefix and
			// let reads stop at s.size.
			return nil
		}
		if err := s.file.Truncate(pos); err != nil {
			return fmt.Errorf("truncate torn segment: %w", err)
		}
		if _, err := s.file.Seek(pos, io.SeekStart); err != nil {
			return fmt.Errorf("seek after truncate: %w", err)
		}
	}
	return nil
}

// append writes an encoded record. The caller holds the partition lock and has
// already stamped the record's sequence number.
func (s *segment) append(encoded []byte, seq uint64) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.readOnly {
		return fmt.Errorf("stream: append to read-only segment %d", s.baseSeq)
	}

	// Index before writing so the entry points at this record's start offset.
	if s.sinceIdx >= s.indexInterval || len(s.index) == 0 {
		var e [indexEntrySize]byte
		binary.LittleEndian.PutUint64(e[0:8], seq)
		binary.LittleEndian.PutUint64(e[8:16], uint64(s.size))
		if _, err := s.idxFile.Write(e[:]); err != nil {
			return fmt.Errorf("write index entry: %w", err)
		}
		s.index = append(s.index, indexEntry{seq: seq, pos: s.size})
		s.sinceIdx = 0
	}

	if _, err := s.writer.Write(encoded); err != nil {
		return fmt.Errorf("write record: %w", err)
	}
	s.size += int64(len(encoded))
	s.sinceIdx += int64(len(encoded))
	s.lastSeq = seq
	return nil
}

// flush pushes buffered writes into the page cache. It does not fsync.
func (s *segment) flush() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.writer == nil {
		return nil
	}
	return s.writer.Flush()
}

// sync forces buffered writes to durable storage.
func (s *segment) sync() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.writer != nil {
		if err := s.writer.Flush(); err != nil {
			return err
		}
	}
	if s.idxFile != nil {
		if err := s.idxFile.Sync(); err != nil {
			return err
		}
	}
	return s.file.Sync()
}

// seekPos returns the byte offset to start scanning from in order to reach seq.
func (s *segment) seekPos(seq uint64) int64 {
	// Largest index entry with entry.seq <= seq.
	i := sort.Search(len(s.index), func(i int) bool { return s.index[i].seq > seq })
	if i == 0 {
		return 0
	}
	return s.index[i-1].pos
}

// read returns up to maxRecords records with Seq >= fromSeq, stopping early if
// the accumulated payload exceeds maxBytes. A short result is not an error —
// the caller continues into the next segment.
func (s *segment) read(fromSeq uint64, maxRecords int, maxBytes int64) ([]*Record, error) {
	s.mu.RLock()
	if s.writer != nil {
		// Records may still be sitting in the write buffer. Flushing on the
		// read path keeps "append then immediately read" correct, which is
		// exactly what a chat client does after sending a message.
		s.mu.RUnlock()
		if err := s.flush(); err != nil {
			return nil, err
		}
		s.mu.RLock()
	}
	limit := s.size
	start := s.seekPos(fromSeq)
	s.mu.RUnlock()

	if start >= limit {
		return nil, nil
	}

	reader := bufio.NewReaderSize(io.NewSectionReader(s.file, start, limit-start), 128*1024)
	var frame [recordFrameHeaderSize]byte
	var out []*Record
	var bytesRead int64

	for len(out) < maxRecords {
		if _, err := io.ReadFull(reader, frame[:]); err != nil {
			break
		}
		checksum := binary.LittleEndian.Uint32(frame[0:4])
		bodyLen := binary.LittleEndian.Uint32(frame[4:8])
		if bodyLen < recordBodyHeaderSize || bodyLen > MaxRecordSize {
			return out, ErrCorruptRecord
		}
		body := make([]byte, bodyLen)
		if _, err := io.ReadFull(reader, body); err != nil {
			break
		}
		if crc32.ChecksumIEEE(body) != checksum {
			return out, ErrCorruptRecord
		}

		seq := binary.LittleEndian.Uint64(body[0:8])
		if seq < fromSeq {
			continue // scanning forward from the sparse index entry
		}

		rec, err := decodeRecordBody(body)
		if err != nil {
			return out, err
		}
		out = append(out, rec)

		bytesRead += int64(bodyLen)
		if maxBytes > 0 && bytesRead >= maxBytes {
			break
		}
	}
	return out, nil
}

// truncateTo discards every record with Seq >= seq, leaving the segment ending
// at seq-1. It reports whether anything was removed.
//
// This is the only operation in the log that destroys acknowledged-looking
// data, so it is deliberately narrow: it is reachable only from
// Partition.TruncateTo, which is reachable only from a follower reconciling
// against a leader epoch. Nothing on the client path can call it.
func (s *segment) truncateTo(seq uint64) (bool, error) {
	if s.readOnly {
		return false, fmt.Errorf("stream: cannot truncate read-only segment %d", s.baseSeq)
	}

	// Flush first: the bytes to be discarded may still be in the write buffer,
	// and truncating the file underneath a dirty buffer would resurrect them on
	// the next flush.
	if err := s.flush(); err != nil {
		return false, err
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	if seq > s.lastSeq {
		return false, nil // nothing at or after seq
	}

	pos, err := s.scanForOffsetLocked(seq)
	if err != nil {
		return false, err
	}
	if pos >= s.size {
		return false, nil
	}

	if err := s.file.Truncate(pos); err != nil {
		return false, fmt.Errorf("truncate segment log: %w", err)
	}
	if _, err := s.file.Seek(pos, io.SeekStart); err != nil {
		return false, fmt.Errorf("seek after truncate: %w", err)
	}

	// Drop index entries pointing into the discarded region and rewrite the
	// index, which is append-only in normal operation and so cannot shrink in
	// place.
	keep := sort.Search(len(s.index), func(i int) bool { return s.index[i].seq >= seq })
	s.index = s.index[:keep]
	if err := s.rewriteIndexLocked(); err != nil {
		return false, err
	}

	s.size = pos
	s.lastSeq = seq - 1
	s.sinceIdx = 0
	// The buffered writer holds a stale view of the file offset after an
	// out-of-band Truncate; a fresh one is cheaper than reasoning about it.
	s.writer = bufio.NewWriterSize(s.file, 256*1024)
	return true, nil
}

// scanForOffsetLocked returns the byte offset of the first record with
// Seq >= seq, or s.size when every record precedes it. The caller holds s.mu.
func (s *segment) scanForOffsetLocked(seq uint64) (int64, error) {
	pos := s.seekPos(seq)
	reader := bufio.NewReaderSize(io.NewSectionReader(s.file, pos, s.size-pos), 128*1024)
	var frame [recordFrameHeaderSize]byte

	for pos < s.size {
		if _, err := io.ReadFull(reader, frame[:]); err != nil {
			break
		}
		bodyLen := binary.LittleEndian.Uint32(frame[4:8])
		if bodyLen < recordBodyHeaderSize || bodyLen > MaxRecordSize {
			return 0, ErrCorruptRecord
		}
		body := make([]byte, bodyLen)
		if _, err := io.ReadFull(reader, body); err != nil {
			break
		}
		if binary.LittleEndian.Uint64(body[0:8]) >= seq {
			return pos, nil
		}
		pos += recordFrameHeaderSize + int64(bodyLen)
	}
	return s.size, nil
}

// rewriteIndexLocked replaces the on-disk index with the in-memory entries.
func (s *segment) rewriteIndexLocked() error {
	_, idxPath := segmentPaths(s.dir, s.baseSeq)
	if s.idxFile != nil {
		s.idxFile.Close()
		s.idxFile = nil
	}

	buf := make([]byte, len(s.index)*indexEntrySize)
	for i, e := range s.index {
		b := buf[i*indexEntrySize:]
		binary.LittleEndian.PutUint64(b[0:8], e.seq)
		binary.LittleEndian.PutUint64(b[8:16], uint64(e.pos))
	}
	if err := os.WriteFile(idxPath, buf, 0644); err != nil {
		return fmt.Errorf("rewrite segment index: %w", err)
	}

	idx, err := os.OpenFile(idxPath, os.O_CREATE|os.O_RDWR|os.O_APPEND, 0644)
	if err != nil {
		return fmt.Errorf("reopen segment index: %w", err)
	}
	s.idxFile = idx
	return nil
}

// reopenWritable turns a sealed segment back into a writable one. Truncation is
// the only caller: rolling back into a sealed segment means the records that
// caused it to be sealed are gone.
func (s *segment) reopenWritable() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.readOnly {
		return nil
	}

	logPath, idxPath := segmentPaths(s.dir, s.baseSeq)
	f, err := os.OpenFile(logPath, os.O_RDWR|os.O_APPEND, 0644)
	if err != nil {
		return fmt.Errorf("reopen segment log for write: %w", err)
	}
	idx, err := os.OpenFile(idxPath, os.O_CREATE|os.O_RDWR|os.O_APPEND, 0644)
	if err != nil {
		f.Close()
		return fmt.Errorf("reopen segment index for write: %w", err)
	}

	s.file.Close()
	s.file = f
	s.idxFile = idx
	s.writer = bufio.NewWriterSize(f, 256*1024)
	s.readOnly = false
	return nil
}

// contains reports whether seq could live in this segment.
func (s *segment) contains(seq uint64) bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return seq >= s.baseSeq && seq <= s.lastSeq
}

func (s *segment) lastSequence() uint64 {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.lastSeq
}

func (s *segment) bytes() int64 {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.size
}

// isEmpty reports whether the segment holds no records.
func (s *segment) isEmpty() bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.lastSeq < s.baseSeq
}

func (s *segment) close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.writer != nil {
		s.writer.Flush()
	}
	if s.idxFile != nil {
		s.idxFile.Close()
	}
	return s.file.Close()
}

// remove closes and deletes both backing files. Used by retention.
func (s *segment) remove() error {
	logPath, idxPath := segmentPaths(s.dir, s.baseSeq)
	if err := s.close(); err != nil {
		return err
	}
	if err := os.Remove(logPath); err != nil && !os.IsNotExist(err) {
		return err
	}
	if err := os.Remove(idxPath); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

// firstTimestamp returns the timestamp of the segment's first record, used by
// time-based retention. Returns 0 for an empty segment.
func (s *segment) firstTimestamp() int64 {
	recs, err := s.read(s.baseSeq, 1, 0)
	if err != nil || len(recs) == 0 {
		return 0
	}
	return recs[0].Timestamp
}

// listSegmentBases returns the base sequences of every segment in dir, ascending.
func listSegmentBases(dir string) ([]uint64, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	var bases []uint64
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		if base, ok := parseSegmentBase(e.Name()); ok {
			bases = append(bases, base)
		}
	}
	sort.Slice(bases, func(i, j int) bool { return bases[i] < bases[j] })
	return bases, nil
}
