package cache

import (
	"sync"
	"time"
)

// Entry represents a single cache entry with optional TTL.
type Entry struct {
	Key       string `json:"key"`
	Value     []byte `json:"value"`
	CreatedAt int64  `json:"created_at"` // UnixNano
	ExpiresAt int64  `json:"expires_at"` // UnixNano, 0 = no expiry
	TTL       int64  `json:"ttl"`        // Original TTL in milliseconds, 0 = no expiry
}

// IsExpired returns true if the entry has a TTL and it has passed.
func (e *Entry) IsExpired() bool {
	if e.ExpiresAt == 0 {
		return false
	}
	return time.Now().UnixNano() > e.ExpiresAt
}

// RemainingTTL returns the remaining TTL in milliseconds, -1 if no expiry, 0 if expired.
func (e *Entry) RemainingTTL() int64 {
	if e.ExpiresAt == 0 {
		return -1
	}
	remaining := e.ExpiresAt - time.Now().UnixNano()
	if remaining <= 0 {
		return 0
	}
	return remaining / int64(time.Millisecond)
}

// Stats holds cache statistics.
type Stats struct {
	KeyCount   int   `json:"key_count"`
	MemoryUsed int64 `json:"memory_used"` // approximate bytes
	Hits       int64 `json:"hits"`
	Misses     int64 `json:"misses"`
	Sets       int64 `json:"sets"`
	Deletes    int64 `json:"deletes"`
	Expired    int64 `json:"expired"`
}

// Store is a thread-safe in-memory KV store with TTL support and LRU-style eviction.
type Store struct {
	mu       sync.RWMutex
	data     map[string]*Entry
	maxKeys  int
	memUsed  int64

	// Atomic-like stats (protected by mu for simplicity since we hold it anyway)
	hits    int64
	misses  int64
	sets    int64
	deletes int64
	expired int64
}

// NewStore creates a new cache store. maxKeys=0 means unlimited.
func NewStore(maxKeys int) *Store {
	return &Store{
		data:    make(map[string]*Entry),
		maxKeys: maxKeys,
	}
}

// Set stores a key-value pair with an optional TTL in milliseconds (0 = no expiry).
func (s *Store) Set(key string, value []byte, ttlMs int64) {
	s.mu.Lock()
	defer s.mu.Unlock()

	now := time.Now().UnixNano()
	var expiresAt int64
	if ttlMs > 0 {
		expiresAt = now + ttlMs*int64(time.Millisecond)
	}

	// Update memory tracking
	if old, exists := s.data[key]; exists {
		s.memUsed -= int64(len(old.Key) + len(old.Value))
	}

	entry := &Entry{
		Key:       key,
		Value:     value,
		CreatedAt: now,
		ExpiresAt: expiresAt,
		TTL:       ttlMs,
	}
	s.data[key] = entry
	s.memUsed += int64(len(key) + len(value))
	s.sets++

	// Evict if over capacity (simple random eviction — fast and good enough)
	if s.maxKeys > 0 && len(s.data) > s.maxKeys {
		s.evictOne()
	}
}

// Get retrieves a value by key. Returns nil if not found or expired.
func (s *Store) Get(key string) *Entry {
	s.mu.Lock()
	defer s.mu.Unlock()

	entry, exists := s.data[key]
	if !exists {
		s.misses++
		return nil
	}

	if entry.IsExpired() {
		s.removeEntry(key, entry)
		s.expired++
		s.misses++
		return nil
	}

	s.hits++
	return entry
}

// Del removes a key. Returns true if the key existed.
func (s *Store) Del(key string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()

	entry, exists := s.data[key]
	if !exists {
		return false
	}

	s.removeEntry(key, entry)
	s.deletes++
	return true
}

// Exists checks if a key exists and is not expired.
func (s *Store) Exists(key string) bool {
	s.mu.RLock()
	entry, exists := s.data[key]
	s.mu.RUnlock()

	if !exists {
		return false
	}
	if entry.IsExpired() {
		// Lazy delete
		s.mu.Lock()
		if e, ok := s.data[key]; ok && e.IsExpired() {
			s.removeEntry(key, e)
			s.expired++
		}
		s.mu.Unlock()
		return false
	}
	return true
}

// Keys returns all non-expired keys matching a simple glob pattern.
// Supports: * (match all), prefix* , *suffix, *contains*
// If pattern is empty or "*", returns all keys.
func (s *Store) Keys(pattern string) []string {
	s.mu.RLock()
	defer s.mu.RUnlock()

	var keys []string
	for k, entry := range s.data {
		if entry.IsExpired() {
			continue
		}
		if matchPattern(pattern, k) {
			keys = append(keys, k)
		}
	}
	return keys
}

// Expire sets a new TTL on an existing key. Returns false if key doesn't exist.
func (s *Store) Expire(key string, ttlMs int64) bool {
	s.mu.Lock()
	defer s.mu.Unlock()

	entry, exists := s.data[key]
	if !exists || entry.IsExpired() {
		return false
	}

	if ttlMs > 0 {
		entry.ExpiresAt = time.Now().UnixNano() + ttlMs*int64(time.Millisecond)
		entry.TTL = ttlMs
	} else {
		entry.ExpiresAt = 0
		entry.TTL = 0
	}
	return true
}

// Incr atomically increments a numeric value. Creates key with value "0" + delta if not exists.
// Returns the new value after increment.
func (s *Store) Incr(key string, delta int64) (int64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	var current int64
	entry, exists := s.data[key]

	if exists && !entry.IsExpired() {
		val, err := parseInt64(entry.Value)
		if err != nil {
			return 0, err
		}
		current = val
	}

	current += delta
	newVal := formatInt64(current)

	if exists {
		s.memUsed -= int64(len(entry.Value))
	}

	now := time.Now().UnixNano()
	var expiresAt int64
	if exists && entry.ExpiresAt > 0 {
		expiresAt = entry.ExpiresAt // preserve existing TTL
	}

	s.data[key] = &Entry{
		Key:       key,
		Value:     newVal,
		CreatedAt: now,
		ExpiresAt: expiresAt,
	}
	s.memUsed += int64(len(newVal))
	if !exists {
		s.memUsed += int64(len(key))
	}
	s.sets++
	return current, nil
}

// FlushAll removes all entries.
func (s *Store) FlushAll() int {
	s.mu.Lock()
	defer s.mu.Unlock()

	count := len(s.data)
	s.data = make(map[string]*Entry)
	s.memUsed = 0
	return count
}

// CleanExpired removes all expired entries. Called periodically by scheduler.
func (s *Store) CleanExpired() int {
	s.mu.Lock()
	defer s.mu.Unlock()

	count := 0
	for key, entry := range s.data {
		if entry.IsExpired() {
			s.removeEntry(key, entry)
			s.expired++
			count++
		}
	}
	return count
}

// GetStats returns current cache statistics.
func (s *Store) GetStats() Stats {
	s.mu.RLock()
	defer s.mu.RUnlock()

	return Stats{
		KeyCount:   len(s.data),
		MemoryUsed: s.memUsed,
		Hits:       s.hits,
		Misses:     s.misses,
		Sets:       s.sets,
		Deletes:    s.deletes,
		Expired:    s.expired,
	}
}

// Snapshot returns all non-expired entries (for persistence/replication).
func (s *Store) Snapshot() []*Entry {
	s.mu.RLock()
	defer s.mu.RUnlock()

	entries := make([]*Entry, 0, len(s.data))
	for _, entry := range s.data {
		if !entry.IsExpired() {
			entries = append(entries, entry)
		}
	}
	return entries
}

// Restore loads entries from a snapshot (for recovery/replication).
func (s *Store) Restore(entries []*Entry) {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.data = make(map[string]*Entry, len(entries))
	s.memUsed = 0
	for _, entry := range entries {
		if !entry.IsExpired() {
			s.data[entry.Key] = entry
			s.memUsed += int64(len(entry.Key) + len(entry.Value))
		}
	}
}

// Close releases resources.
func (s *Store) Close() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.data = nil
	s.memUsed = 0
}

// --- internal helpers ---

func (s *Store) removeEntry(key string, entry *Entry) {
	s.memUsed -= int64(len(key) + len(entry.Value))
	delete(s.data, key)
}

func (s *Store) evictOne() {
	// Simple random eviction: pick the first key from map iteration
	// Go maps iterate in random order, so this is effectively random
	for key, entry := range s.data {
		s.removeEntry(key, entry)
		return
	}
}
