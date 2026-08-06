// Package dedup provides idempotency for message submission.
//
// Mobile networks fail in the worst possible way: the request arrives, the
// server processes it, and the response is lost. The client cannot distinguish
// this from "the request never arrived", so it retries — and without dedup the
// user sees their message twice. Retrying is correct client behaviour; making
// it safe is the server's job.
//
// The mechanism is a claim table keyed by (sender, client-supplied ID). The
// first submission claims the key and stores the outcome; a retry finds the
// claim and receives the original outcome instead of creating a second message.
package dedup

import (
	"sync"
	"time"
)

// Result is the outcome recorded for a claimed key.
type Result struct {
	// MessageID is the server-assigned ID of the original message.
	MessageID string `json:"message_id"`
	// Topic, Partition and Seq locate the original record in the log, so a
	// retry can be answered with the same coordinates the first attempt got.
	Topic     string `json:"topic"`
	Partition int32  `json:"partition"`
	Seq       uint64 `json:"seq"`
	Timestamp int64  `json:"timestamp"`
}

type entry struct {
	result   Result
	expires  time.Time
	complete bool // false while the first attempt is still in flight
}

// Config tunes the dedup table.
type Config struct {
	// TTL is how long a claim is remembered. It must exceed the longest
	// plausible client retry window: a phone that loses signal mid-send may
	// retry minutes later, and a TTL shorter than that reopens the duplicate.
	TTL time.Duration
	// SweepInterval is how often expired claims are collected.
	SweepInterval time.Duration
	// MaxEntries caps memory. Zero means unlimited.
	MaxEntries int
}

func (c *Config) applyDefaults() {
	if c.TTL <= 0 {
		c.TTL = 10 * time.Minute
	}
	if c.SweepInterval <= 0 {
		c.SweepInterval = time.Minute
	}
}

const shardCount = 64

type shard struct {
	mu      sync.Mutex
	entries map[string]*entry
}

// Table is the claim table.
type Table struct {
	cfg    Config
	shards [shardCount]*shard

	stopOnce sync.Once
	stop     chan struct{}
}

// New creates a dedup table and starts its sweeper.
func New(cfg Config) *Table {
	cfg.applyDefaults()
	t := &Table{cfg: cfg, stop: make(chan struct{})}
	for i := range t.shards {
		t.shards[i] = &shard{entries: make(map[string]*entry)}
	}
	go t.sweepLoop()
	return t
}

// Key identifies a submission attempt.
//
// Sender is part of the key so that two users choosing the same client-side ID
// — trivially possible when clients generate short IDs or restart a counter —
// cannot collide and silently swallow one another's messages.
type Key struct {
	Sender      string
	ClientMsgID string
}

func (k Key) String() string { return k.Sender + "\x00" + k.ClientMsgID }

func hashKey(s string) uint32 {
	var h uint32 = 2166136261
	for i := 0; i < len(s); i++ {
		h ^= uint32(s[i])
		h *= 16777619
	}
	return h % shardCount
}

// Claim attempts to reserve a key.
//
// It returns:
//
//	claimed=true             the caller owns this key and must go on to publish,
//	                         then call Complete or Abandon
//	claimed=false, ok=true   a duplicate of a finished submission; result holds
//	                         the original outcome
//	claimed=false, ok=false  a duplicate of a submission still in flight; the
//	                         caller should tell the client to retry shortly
//
// The three-way answer exists because two retries can race. Returning "already
// done" for an in-flight claim would hand the client a zero-valued result, and
// treating it as a fresh claim would produce the duplicate the table exists to
// prevent.
func (t *Table) Claim(k Key) (result Result, ok bool, claimed bool) {
	ks := k.String()
	sh := t.shards[hashKey(ks)]
	now := time.Now()

	sh.mu.Lock()
	defer sh.mu.Unlock()

	if e, exists := sh.entries[ks]; exists && now.Before(e.expires) {
		if e.complete {
			return e.result, true, false
		}
		return Result{}, false, false
	}

	if t.cfg.MaxEntries > 0 && len(sh.entries) >= t.cfg.MaxEntries/shardCount {
		// Over budget: evict expired entries in this shard before refusing.
		for key, e := range sh.entries {
			if now.After(e.expires) {
				delete(sh.entries, key)
			}
		}
	}

	sh.entries[ks] = &entry{expires: now.Add(t.cfg.TTL)}
	return Result{}, false, true
}

// Complete records the outcome of a claimed key, making subsequent retries
// return it.
func (t *Table) Complete(k Key, r Result) {
	ks := k.String()
	sh := t.shards[hashKey(ks)]

	sh.mu.Lock()
	defer sh.mu.Unlock()

	e, exists := sh.entries[ks]
	if !exists {
		// The claim was swept while the publish was in flight. Re-create it so
		// a retry arriving now is still deduplicated.
		e = &entry{}
		sh.entries[ks] = e
	}
	e.result = r
	e.complete = true
	e.expires = time.Now().Add(t.cfg.TTL)
}

// Abandon releases a claim whose publish failed, so the client's retry is
// treated as a fresh attempt rather than deduplicated against a message that
// was never stored.
func (t *Table) Abandon(k Key) {
	ks := k.String()
	sh := t.shards[hashKey(ks)]

	sh.mu.Lock()
	defer sh.mu.Unlock()
	if e, exists := sh.entries[ks]; exists && !e.complete {
		delete(sh.entries, ks)
	}
}

// Lookup returns a completed result without claiming.
func (t *Table) Lookup(k Key) (Result, bool) {
	ks := k.String()
	sh := t.shards[hashKey(ks)]

	sh.mu.Lock()
	defer sh.mu.Unlock()

	e, exists := sh.entries[ks]
	if !exists || !e.complete || time.Now().After(e.expires) {
		return Result{}, false
	}
	return e.result, true
}

// Len returns the number of tracked claims, including in-flight ones.
func (t *Table) Len() int {
	n := 0
	for _, sh := range t.shards {
		sh.mu.Lock()
		n += len(sh.entries)
		sh.mu.Unlock()
	}
	return n
}

func (t *Table) sweepLoop() {
	ticker := time.NewTicker(t.cfg.SweepInterval)
	defer ticker.Stop()
	for {
		select {
		case <-t.stop:
			return
		case <-ticker.C:
			t.Sweep()
		}
	}
}

// Sweep removes expired claims and returns how many were dropped.
func (t *Table) Sweep() int {
	now := time.Now()
	removed := 0
	for _, sh := range t.shards {
		sh.mu.Lock()
		for k, e := range sh.entries {
			if now.After(e.expires) {
				delete(sh.entries, k)
				removed++
			}
		}
		sh.mu.Unlock()
	}
	return removed
}

// Close stops the sweeper.
func (t *Table) Close() {
	t.stopOnce.Do(func() { close(t.stop) })
}
