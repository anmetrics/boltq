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
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// CursorKey identifies one reader's position in one partition.
//
// Member is what makes multi-device work: a WhatsApp user with a phone, a
// laptop and two tablets is one Group ("user:123") with four Members, each
// holding an independent position. A message is only eligible for removal once
// every member of every group has passed it — or retention expires it, which
// is the escape hatch for a device that never comes back.
type CursorKey struct {
	Topic     string
	Partition int32
	Group     string // logical reader, e.g. "user:123" or "push-dispatcher"
	Member    string // instance within the group, e.g. a device ID; may be empty
}

func (k CursorKey) String() string {
	return fmt.Sprintf("%s\x00%d\x00%s\x00%s", k.Topic, k.Partition, k.Group, k.Member)
}

func parseCursorKey(s string) (CursorKey, error) {
	parts := strings.Split(s, "\x00")
	if len(parts) != 4 {
		return CursorKey{}, fmt.Errorf("stream: malformed cursor key")
	}
	var pid int32
	if _, err := fmt.Sscanf(parts[1], "%d", &pid); err != nil {
		return CursorKey{}, fmt.Errorf("stream: malformed cursor partition")
	}
	return CursorKey{Topic: parts[0], Partition: pid, Group: parts[2], Member: parts[3]}, nil
}

const (
	cursorFileName      = "cursors.log"
	cursorTempName      = "cursors.log.compact"
	cursorRecordHeader  = 8 // crc32 + bodyLen
	defaultCompactBytes = 8 << 20
)

// CursorStore persists reader positions.
//
// It is an append-only log of (key, seq) pairs compacted in place, rather than
// a B-tree: commits are the highest-frequency write in the system (one per
// message read per device) and are overwhelmingly "same key, larger value".
// An append costs one buffered write; the log is rewritten only when it grows
// past a threshold, at which point only the latest value per key survives.
//
// Durability model: commits are buffered and flushed, not fsynced. Losing the
// last few milliseconds of cursor commits after a crash causes a device to
// re-receive a handful of messages it already had — which client-side dedup by
// message ID already handles. Paying an fsync per commit to avoid a benign
// redelivery would be the wrong trade.
type CursorStore struct {
	dir  string
	path string

	mu        sync.RWMutex
	positions map[string]uint64
	file      *os.File
	writer    *bufio.Writer
	size      int64
	closed    bool

	compactBytes int64
	compacting   atomic.Bool
	dirty        atomic.Bool
}

// OpenCursorStore opens or creates the cursor store under dir.
func OpenCursorStore(dir string) (*CursorStore, error) {
	if err := os.MkdirAll(dir, 0755); err != nil {
		return nil, fmt.Errorf("create cursor dir: %w", err)
	}
	path := filepath.Join(dir, cursorFileName)

	c := &CursorStore{
		dir:          dir,
		path:         path,
		positions:    make(map[string]uint64),
		compactBytes: defaultCompactBytes,
	}
	if err := c.load(); err != nil {
		return nil, err
	}

	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR|os.O_APPEND, 0644)
	if err != nil {
		return nil, fmt.Errorf("open cursor log: %w", err)
	}
	c.file = f
	c.writer = bufio.NewWriterSize(f, 64*1024)

	if info, err := f.Stat(); err == nil {
		c.size = info.Size()
	}
	return c, nil
}

// load replays the cursor log into memory, stopping at the first corrupt
// record. Later records win, so replay order is the compaction rule.
func (c *CursorStore) load() error {
	f, err := os.Open(c.path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("open cursor log for read: %w", err)
	}
	defer f.Close()

	reader := bufio.NewReaderSize(f, 256*1024)
	var header [cursorRecordHeader]byte

	for {
		if _, err := io.ReadFull(reader, header[:]); err != nil {
			break
		}
		checksum := binary.LittleEndian.Uint32(header[0:4])
		bodyLen := binary.LittleEndian.Uint32(header[4:8])
		if bodyLen < 10 || bodyLen > 64<<10 {
			break
		}
		body := make([]byte, bodyLen)
		if _, err := io.ReadFull(reader, body); err != nil {
			break
		}
		if crc32.ChecksumIEEE(body) != checksum {
			break
		}

		seq := binary.LittleEndian.Uint64(body[0:8])
		keyLen := int(binary.LittleEndian.Uint16(body[8:10]))
		if 10+keyLen != len(body) {
			break
		}
		c.positions[string(body[10:])] = seq
	}
	return nil
}

func encodeCursorRecord(key string, seq uint64) []byte {
	bodyLen := 10 + len(key)
	buf := make([]byte, cursorRecordHeader+bodyLen)
	body := buf[cursorRecordHeader:]
	binary.LittleEndian.PutUint64(body[0:8], seq)
	binary.LittleEndian.PutUint16(body[8:10], uint16(len(key)))
	copy(body[10:], key)
	binary.LittleEndian.PutUint32(buf[0:4], crc32.ChecksumIEEE(body))
	binary.LittleEndian.PutUint32(buf[4:8], uint32(bodyLen))
	return buf
}

// Commit records that the reader has consumed everything below seq.
//
// Commits are monotonic: a lower sequence than the one already stored is
// ignored rather than applied. Out-of-order commits are normal when a device
// has several in-flight requests, and honouring a stale one would silently
// replay messages the user has already seen.
func (c *CursorStore) Commit(key CursorKey, seq uint64) error {
	k := key.String()

	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return ErrClosed
	}
	if cur, ok := c.positions[k]; ok && seq <= cur {
		c.mu.Unlock()
		return nil
	}
	c.positions[k] = seq

	rec := encodeCursorRecord(k, seq)
	if _, err := c.writer.Write(rec); err != nil {
		c.mu.Unlock()
		return fmt.Errorf("write cursor: %w", err)
	}
	c.size += int64(len(rec))
	needCompact := c.size > c.compactBytes
	c.mu.Unlock()

	c.dirty.Store(true)
	if needCompact {
		c.maybeCompact()
	}
	return nil
}

// Position returns the committed sequence for key, and whether one exists.
func (c *CursorStore) Position(key CursorKey) (uint64, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	seq, ok := c.positions[key.String()]
	return seq, ok
}

// PositionOr returns the committed sequence, or def when there is none.
func (c *CursorStore) PositionOr(key CursorKey, def uint64) uint64 {
	if seq, ok := c.Position(key); ok {
		return seq
	}
	return def
}

// GroupMembers lists the members of a group holding a cursor on a partition,
// with their positions.
func (c *CursorStore) GroupMembers(topic string, partition int32, group string) map[string]uint64 {
	prefix := CursorKey{Topic: topic, Partition: partition, Group: group}.String()
	prefix = strings.TrimSuffix(prefix, "\x00") + "\x00"

	c.mu.RLock()
	defer c.mu.RUnlock()

	out := make(map[string]uint64)
	for k, v := range c.positions {
		if !strings.HasPrefix(k, prefix) {
			continue
		}
		key, err := parseCursorKey(k)
		if err != nil {
			continue
		}
		out[key.Member] = v
	}
	return out
}

// SlowestInGroup returns the minimum committed sequence across a group's
// members. It is the watermark below which every member has consumed — the
// safe point for group-scoped cleanup.
func (c *CursorStore) SlowestInGroup(topic string, partition int32, group string) (uint64, bool) {
	members := c.GroupMembers(topic, partition, group)
	if len(members) == 0 {
		return 0, false
	}
	min := ^uint64(0)
	for _, v := range members {
		if v < min {
			min = v
		}
	}
	return min, true
}

// Delete removes a cursor. Used when a device is unlinked.
func (c *CursorStore) Delete(key CursorKey) error {
	k := key.String()
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return ErrClosed
	}
	if _, ok := c.positions[k]; !ok {
		c.mu.Unlock()
		return nil
	}
	delete(c.positions, k)
	c.mu.Unlock()

	c.dirty.Store(true)
	// A deletion cannot be expressed as an append in this format, so it forces
	// a compaction. Device unlinking is rare enough that this is acceptable.
	return c.Compact()
}

// Flush pushes buffered commits into the page cache.
func (c *CursorStore) Flush() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed || c.writer == nil {
		return nil
	}
	return c.writer.Flush()
}

// Sync flushes and fsyncs. Called on graceful shutdown, where losing cursors is
// avoidable and therefore not acceptable.
func (c *CursorStore) Sync() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed || c.file == nil {
		return nil
	}
	if err := c.writer.Flush(); err != nil {
		return err
	}
	return c.file.Sync()
}

func (c *CursorStore) maybeCompact() {
	if !c.compacting.CompareAndSwap(false, true) {
		return
	}
	go func() {
		defer c.compacting.Store(false)
		if err := c.Compact(); err != nil {
			// Compaction failing is not fatal: the log keeps growing and gets
			// retried on the next threshold crossing.
			fmt.Fprintf(os.Stderr, "[stream] cursor compaction failed: %v\n", err)
		}
	}()
}

// Compact rewrites the cursor log with one record per live key.
func (c *CursorStore) Compact() error {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return ErrClosed
	}
	snapshot := make(map[string]uint64, len(c.positions))
	for k, v := range c.positions {
		snapshot[k] = v
	}
	if err := c.writer.Flush(); err != nil {
		c.mu.Unlock()
		return err
	}
	c.mu.Unlock()

	tmpPath := filepath.Join(c.dir, cursorTempName)
	tmp, err := os.OpenFile(tmpPath, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0644)
	if err != nil {
		return fmt.Errorf("create compaction temp: %w", err)
	}

	w := bufio.NewWriterSize(tmp, 256*1024)
	keys := make([]string, 0, len(snapshot))
	for k := range snapshot {
		keys = append(keys, k)
	}
	sort.Strings(keys) // deterministic output aids debugging and diffing

	var size int64
	for _, k := range keys {
		rec := encodeCursorRecord(k, snapshot[k])
		if _, err := w.Write(rec); err != nil {
			tmp.Close()
			os.Remove(tmpPath)
			return fmt.Errorf("write compacted cursor: %w", err)
		}
		size += int64(len(rec))
	}
	if err := w.Flush(); err != nil {
		tmp.Close()
		os.Remove(tmpPath)
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		os.Remove(tmpPath)
		return err
	}
	tmp.Close()

	// Swap under the lock. Commits arriving during the rewrite above went into
	// the old file and into c.positions; they are present in neither the
	// snapshot nor the new file, so they must be re-appended after the swap.
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		os.Remove(tmpPath)
		return ErrClosed
	}

	var missed [][2]interface{}
	for k, v := range c.positions {
		if old, ok := snapshot[k]; !ok || old != v {
			missed = append(missed, [2]interface{}{k, v})
		}
	}

	c.writer.Flush()
	c.file.Close()

	if err := os.Rename(tmpPath, c.path); err != nil {
		// Reopen the original so the store stays usable.
		f, oerr := os.OpenFile(c.path, os.O_CREATE|os.O_RDWR|os.O_APPEND, 0644)
		if oerr == nil {
			c.file = f
			c.writer = bufio.NewWriterSize(f, 64*1024)
		}
		return fmt.Errorf("swap compacted cursor log: %w", err)
	}

	f, err := os.OpenFile(c.path, os.O_CREATE|os.O_RDWR|os.O_APPEND, 0644)
	if err != nil {
		return fmt.Errorf("reopen cursor log: %w", err)
	}
	c.file = f
	c.writer = bufio.NewWriterSize(f, 64*1024)
	c.size = size

	for _, kv := range missed {
		rec := encodeCursorRecord(kv[0].(string), kv[1].(uint64))
		if _, err := c.writer.Write(rec); err != nil {
			return fmt.Errorf("replay missed cursor: %w", err)
		}
		c.size += int64(len(rec))
	}
	return c.writer.Flush()
}

// Count returns how many cursors are tracked.
func (c *CursorStore) Count() int {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return len(c.positions)
}

// StartAutoFlush flushes buffered commits on an interval, bounding how much
// cursor progress a crash can undo. Returns a stop function.
func (c *CursorStore) StartAutoFlush(interval time.Duration) func() {
	if interval <= 0 {
		interval = time.Second
	}
	done := make(chan struct{})
	go func() {
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-done:
				return
			case <-ticker.C:
				if c.dirty.CompareAndSwap(true, false) {
					c.Flush()
				}
			}
		}
	}()
	return func() { close(done) }
}

// Close flushes, fsyncs and releases the cursor log.
func (c *CursorStore) Close() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return nil
	}
	c.closed = true
	if c.writer != nil {
		c.writer.Flush()
	}
	if c.file != nil {
		c.file.Sync()
		return c.file.Close()
	}
	return nil
}
