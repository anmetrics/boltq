package metrics

import (
	"encoding/json"
	"sync/atomic"
)

// Metrics holds all server metrics using atomic counters for lock-free access.
type Metrics struct {
	MessagesPublished int64 `json:"messages_published"`
	MessagesConsumed  int64 `json:"messages_consumed"`
	MessagesAcked     int64 `json:"messages_acked"`
	MessagesNacked    int64 `json:"messages_nacked"`
	RetryCount        int64 `json:"retry_count"`
	DeadLetterCount   int64 `json:"dead_letter_count"`
}

var global = &Metrics{}

// Global returns the global metrics instance.
func Global() *Metrics {
	return global
}

func (m *Metrics) IncPublished()   { atomic.AddInt64(&m.MessagesPublished, 1) }
func (m *Metrics) IncConsumed()    { atomic.AddInt64(&m.MessagesConsumed, 1) }
func (m *Metrics) IncAcked()       { atomic.AddInt64(&m.MessagesAcked, 1) }
func (m *Metrics) IncNacked()      { atomic.AddInt64(&m.MessagesNacked, 1) }
func (m *Metrics) IncRetry()       { atomic.AddInt64(&m.RetryCount, 1) }
func (m *Metrics) IncDeadLetter()  { atomic.AddInt64(&m.DeadLetterCount, 1) }

// Snapshot returns a copy of the current metrics.
func (m *Metrics) Snapshot() Metrics {
	return Metrics{
		MessagesPublished: atomic.LoadInt64(&m.MessagesPublished),
		MessagesConsumed:  atomic.LoadInt64(&m.MessagesConsumed),
		MessagesAcked:     atomic.LoadInt64(&m.MessagesAcked),
		MessagesNacked:    atomic.LoadInt64(&m.MessagesNacked),
		RetryCount:        atomic.LoadInt64(&m.RetryCount),
		DeadLetterCount:   atomic.LoadInt64(&m.DeadLetterCount),
	}
}

// JSON returns the metrics as JSON bytes.
func (m *Metrics) JSON() ([]byte, error) {
	snap := m.Snapshot()
	return json.Marshal(snap)
}

// Prometheus returns metrics in Prometheus text format.
func (m *Metrics) Prometheus() string {
	snap := m.Snapshot()
	return "# HELP boltq_messages_published Total messages published\n" +
		"# TYPE boltq_messages_published counter\n" +
		promLine("boltq_messages_published", snap.MessagesPublished) +
		"# HELP boltq_messages_consumed Total messages consumed\n" +
		"# TYPE boltq_messages_consumed counter\n" +
		promLine("boltq_messages_consumed", snap.MessagesConsumed) +
		"# HELP boltq_messages_acked Total messages acknowledged\n" +
		"# TYPE boltq_messages_acked counter\n" +
		promLine("boltq_messages_acked", snap.MessagesAcked) +
		"# HELP boltq_messages_nacked Total messages negatively acknowledged\n" +
		"# TYPE boltq_messages_nacked counter\n" +
		promLine("boltq_messages_nacked", snap.MessagesNacked) +
		"# HELP boltq_retry_count Total retry count\n" +
		"# TYPE boltq_retry_count counter\n" +
		promLine("boltq_retry_count", snap.RetryCount) +
		"# HELP boltq_dead_letter_count Total dead letter count\n" +
		"# TYPE boltq_dead_letter_count counter\n" +
		promLine("boltq_dead_letter_count", snap.DeadLetterCount)
}

func promLine(name string, val int64) string {
	return name + " " + itoa(val) + "\n"
}

func itoa(i int64) string {
	if i == 0 {
		return "0"
	}
	var buf [20]byte
	pos := len(buf)
	neg := false
	if i < 0 {
		neg = true
		i = -i
	}
	for i > 0 {
		pos--
		buf[pos] = byte('0' + i%10)
		i /= 10
	}
	if neg {
		pos--
		buf[pos] = '-'
	}
	return string(buf[pos:])
}
