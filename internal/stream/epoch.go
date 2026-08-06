package stream

import (
	"encoding/binary"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync"
)

// Leader epochs exist to answer one question a sequence number cannot:
// when a follower and a new leader disagree about what is at sequence N,
// which of them is wrong?
//
// Without epochs the follower only knows "my log ends at N". If the old leader
// accepted records N-3..N and died before replicating them, and the new leader
// never had those records, the follower's tail is garbage that no amount of
// fetching will reconcile — it will simply sit at N while the new leader
// assigns its own N-3..N to *different* records. Two logs, one name.
//
// The epoch cache records, for each leadership term, the sequence at which that
// term began. A follower reconnecting sends its last epoch; the leader replies
// with where that epoch actually ended. Anything the follower holds past that
// point was never committed under a leader the cluster agrees on, and is
// truncated away. This is the mechanism Kafka calls leader epoch + truncation,
// and it is what makes automatic failover safe rather than merely convenient.

// epochEntry marks the start of a leadership term.
type epochEntry struct {
	Epoch uint32
	// StartSeq is the first sequence assigned under this epoch.
	StartSeq uint64
}

const (
	epochCheckpointFile = "leader-epoch-checkpoint"
	epochEntrySize      = 12 // epoch:4 + startSeq:8
)

// UndefinedEpoch is returned when a partition has no epoch history that can
// answer a query — a log written before epochs existed, or one whose early
// history has aged out under retention.
const UndefinedEpoch uint32 = 0

// leaderEpochCache is the ordered epoch history of one partition, persisted
// beside its segments.
//
// It is small by construction: one entry per leadership change, not per record.
// A partition that fails over daily for a year holds 365 entries.
type leaderEpochCache struct {
	mu      sync.RWMutex
	dir     string
	entries []epochEntry // ascending by Epoch and by StartSeq
}

// openEpochCache loads a partition's epoch history, creating an empty one when
// the checkpoint does not exist.
func openEpochCache(dir string) (*leaderEpochCache, error) {
	c := &leaderEpochCache{dir: dir}
	data, err := os.ReadFile(filepath.Join(dir, epochCheckpointFile))
	if err != nil {
		if os.IsNotExist(err) {
			return c, nil
		}
		return nil, fmt.Errorf("read epoch checkpoint: %w", err)
	}
	// A short tail means a torn rewrite; drop it. The entries before it are
	// still a valid prefix of the history, and a missing recent epoch only
	// costs an over-conservative truncation, never a divergent log.
	n := len(data) / epochEntrySize
	c.entries = make([]epochEntry, 0, n)
	for i := 0; i < n; i++ {
		b := data[i*epochEntrySize:]
		c.entries = append(c.entries, epochEntry{
			Epoch:    binary.LittleEndian.Uint32(b[0:4]),
			StartSeq: binary.LittleEndian.Uint64(b[4:12]),
		})
	}
	sort.Slice(c.entries, func(i, j int) bool { return c.entries[i].Epoch < c.entries[j].Epoch })
	return c, nil
}

// flushLocked rewrites the checkpoint atomically. The caller holds c.mu.
//
// Rewrite-and-rename rather than append: the file is at most a few kilobytes,
// and truncation has to *remove* entries, which an append-only file cannot do.
func (c *leaderEpochCache) flushLocked() error {
	path := filepath.Join(c.dir, epochCheckpointFile)
	if len(c.entries) == 0 {
		if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
			return err
		}
		return nil
	}

	buf := make([]byte, len(c.entries)*epochEntrySize)
	for i, e := range c.entries {
		b := buf[i*epochEntrySize:]
		binary.LittleEndian.PutUint32(b[0:4], e.Epoch)
		binary.LittleEndian.PutUint64(b[4:12], e.StartSeq)
	}

	tmp := path + ".tmp"
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0644)
	if err != nil {
		return fmt.Errorf("open epoch checkpoint: %w", err)
	}
	if _, err := f.Write(buf); err != nil {
		f.Close()
		os.Remove(tmp)
		return fmt.Errorf("write epoch checkpoint: %w", err)
	}
	if err := f.Sync(); err != nil {
		f.Close()
		os.Remove(tmp)
		return fmt.Errorf("sync epoch checkpoint: %w", err)
	}
	if err := f.Close(); err != nil {
		os.Remove(tmp)
		return err
	}
	return os.Rename(tmp, path)
}

// assign records that epoch begins at startSeq.
//
// Assigning an epoch at or below the latest known one is a no-op rather than an
// error: a follower replays records it already has after a reconnect, and each
// replay carries its epoch. Only a genuinely newer epoch moves the history.
func (c *leaderEpochCache) assign(epoch uint32, startSeq uint64) error {
	if epoch == UndefinedEpoch {
		return nil
	}
	c.mu.Lock()
	defer c.mu.Unlock()

	if n := len(c.entries); n > 0 {
		last := c.entries[n-1]
		if epoch <= last.Epoch {
			return nil
		}
		// An epoch that claims to start before the previous one did would make
		// the history non-monotonic and every subsequent lookup meaningless.
		if startSeq < last.StartSeq {
			return fmt.Errorf("stream: epoch %d starts at %d, before epoch %d at %d",
				epoch, startSeq, last.Epoch, last.StartSeq)
		}
	}
	c.entries = append(c.entries, epochEntry{Epoch: epoch, StartSeq: startSeq})
	return c.flushLocked()
}

// latest returns the most recent epoch and where it began, or (0, 0) when the
// history is empty.
func (c *leaderEpochCache) latest() (uint32, uint64) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	if len(c.entries) == 0 {
		return UndefinedEpoch, 0
	}
	last := c.entries[len(c.entries)-1]
	return last.Epoch, last.StartSeq
}

// endOffsetFor answers "where did this epoch end?" for a follower holding
// records written under it. logEndSeq is the caller's next unassigned sequence.
//
// Following Kafka's semantics:
//
//	epoch == latest      → (latest, logEndSeq): the term is still open, so the
//	                       follower may keep everything it has
//	epoch is in history  → (epoch, start of the next epoch): the term ended
//	                       there, and records past it belong to a term the
//	                       follower has not seen
//	epoch is newer       → (latest, start of the epoch after the follower's
//	                       highest known one): the follower is ahead of us,
//	                       which after a failover means it holds uncommitted
//	                       records
//	epoch is unknown     → (UndefinedEpoch, 0): we cannot place it; the caller
//	                       must fall back to a full re-fetch rather than guess
func (c *leaderEpochCache) endOffsetFor(epoch uint32, logEndSeq uint64) (uint32, uint64) {
	c.mu.RLock()
	defer c.mu.RUnlock()

	if len(c.entries) == 0 {
		return UndefinedEpoch, 0
	}
	last := c.entries[len(c.entries)-1]
	if epoch >= last.Epoch {
		return last.Epoch, logEndSeq
	}

	// First entry strictly greater than epoch: its start is where the queried
	// term stopped being authoritative.
	i := sort.Search(len(c.entries), func(i int) bool { return c.entries[i].Epoch > epoch })
	if i == 0 {
		// The follower's epoch predates everything we retain. Saying "0" here
		// would silently order a full truncation; UndefinedEpoch makes the
		// caller handle it explicitly.
		return UndefinedEpoch, 0
	}
	return c.entries[i-1].Epoch, c.entries[i].StartSeq
}

// epochForSeq returns the epoch under which seq was assigned.
func (c *leaderEpochCache) epochForSeq(seq uint64) uint32 {
	c.mu.RLock()
	defer c.mu.RUnlock()
	i := sort.Search(len(c.entries), func(i int) bool { return c.entries[i].StartSeq > seq })
	if i == 0 {
		return UndefinedEpoch
	}
	return c.entries[i-1].Epoch
}

// truncateFromSeq drops every epoch that began at or after seq, called when the
// log itself is truncated back to seq. An epoch whose entire range was
// truncated away must not linger, or a later lookup would report a term that
// holds no records.
func (c *leaderEpochCache) truncateFromSeq(seq uint64) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	i := sort.Search(len(c.entries), func(i int) bool { return c.entries[i].StartSeq >= seq })
	if i == len(c.entries) {
		return nil
	}
	c.entries = c.entries[:i]
	return c.flushLocked()
}

// truncateBeforeSeq drops epoch history that retention has made unreachable,
// keeping the entry that covers seq so records still on disk can be placed.
func (c *leaderEpochCache) truncateBeforeSeq(seq uint64) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	i := sort.Search(len(c.entries), func(i int) bool { return c.entries[i].StartSeq > seq })
	if i <= 1 {
		return nil
	}
	c.entries = append([]epochEntry(nil), c.entries[i-1:]...)
	return c.flushLocked()
}

// snapshot returns a copy of the history, for diagnostics and tests.
func (c *leaderEpochCache) snapshot() []epochEntry {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return append([]epochEntry(nil), c.entries...)
}
