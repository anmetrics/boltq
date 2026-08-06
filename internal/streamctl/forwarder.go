package streamctl

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"sync/atomic"
	"time"

	"github.com/boltq/boltq/internal/cluster"
	"github.com/boltq/boltq/internal/stream"
)

// Forwarding exists because a client's WebSocket cannot be redirected.
//
// A redirect works when a connection talks to one partition. A chat socket
// subscribes to dozens of conversations whose partitions are led by different
// nodes, so there is no single node to redirect it to — the question "which
// node should this client be connected to" has no answer. The connection
// therefore stays wherever it landed, and individual writes travel.
//
// Only writes travel. A node that holds a replica already has the data, so
// reads, history and tailing are served locally; that is what a replica is for.
// This keeps the forwarded volume proportional to messages sent rather than to
// messages delivered, which in a chat workload differ by the fan-out factor —
// often a hundredfold.

// ForwardRequest is the wire form of a forwarded append.
type ForwardRequest struct {
	Topic     string            `json:"topic"`
	Partition int32             `json:"partition"`
	Key       []byte            `json:"key,omitempty"`
	Payload   []byte            `json:"payload,omitempty"`
	Headers   map[string]string `json:"headers,omitempty"`
	Flags     uint8             `json:"flags,omitempty"`
}

// ForwardResponse is what the leader reports back.
type ForwardResponse struct {
	Topic     string `json:"topic"`
	Partition int32  `json:"partition"`
	Seq       uint64 `json:"seq"`
	Timestamp int64  `json:"timestamp"`
}

// ForwarderConfig configures write routing.
type ForwarderConfig struct {
	// NodeID is this node, used to recognise a request that has come back to
	// where it started.
	NodeID string
	// APIKey authenticates to the peer's internal endpoint.
	APIKey string
	// Timeout bounds one forwarded write. It should be comfortably under the
	// client's own send timeout, so a slow peer surfaces as a failed message
	// rather than a hung socket. Default 5s.
	Timeout time.Duration
}

// ForwarderStats reports routing volume, which is the number that tells an
// operator whether partition placement matches where clients actually land.
type ForwarderStats struct {
	Forwarded uint64 `json:"forwarded"`
	Failed    uint64 `json:"failed"`
	NoLeader  uint64 `json:"no_leader"`
}

// Forwarder routes writes to the node leading their partition.
type Forwarder struct {
	cfg  ForwarderConfig
	meta *cluster.MetadataStore
	http *http.Client

	forwarded atomic.Uint64
	failed    atomic.Uint64
	noLeader  atomic.Uint64
}

// NewForwarder creates a write router.
func NewForwarder(meta *cluster.MetadataStore, cfg ForwarderConfig) *Forwarder {
	if cfg.Timeout <= 0 {
		cfg.Timeout = 5 * time.Second
	}
	return &Forwarder{
		cfg:  cfg,
		meta: meta,
		http: &http.Client{
			Timeout: cfg.Timeout,
			Transport: &http.Transport{
				// Chat traffic is many small writes to a handful of peers.
				// Without a generous idle pool every forwarded message pays a
				// TCP and TLS handshake, which costs more than the write.
				MaxIdleConns:        512,
				MaxIdleConnsPerHost: 64,
				IdleConnTimeout:     90 * time.Second,
			},
		},
	}
}

// Stats returns routing counters.
func (f *Forwarder) Stats() ForwarderStats {
	return ForwarderStats{
		Forwarded: f.forwarded.Load(),
		Failed:    f.failed.Load(),
		NoLeader:  f.noLeader.Load(),
	}
}

// Forward implements stream.WriteForwarder.
func (f *Forwarder) Forward(ctx context.Context, topic string, partition int32, rec *stream.Record) (stream.AppendResult, error) {
	a, ok := f.meta.Assignment(topic, partition)
	if !ok || a.Leader == "" {
		f.noLeader.Add(1)
		// No leader means the partition is offline — every in-sync replica is
		// gone. Failing here is the honest answer; the alternative is picking a
		// replica ourselves, which is exactly the unclean election the control
		// plane refuses to make on its own.
		return stream.AppendResult{}, fmt.Errorf("stream: %s/%d has no leader", topic, partition)
	}
	if a.Leader == f.cfg.NodeID {
		// The metadata says we lead it but the local partition refused. That is
		// the reconciler lagging one step behind a promotion, and it resolves
		// itself within a sweep. Reporting it plainly beats forwarding to
		// ourselves and looping.
		f.failed.Add(1)
		return stream.AppendResult{}, fmt.Errorf("stream: %s/%d is assigned here but not yet promoted", topic, partition)
	}

	leader, ok := f.meta.Broker(a.Leader)
	if !ok || leader.AdminAddr == "" {
		f.noLeader.Add(1)
		return stream.AppendResult{}, fmt.Errorf("stream: leader %s of %s/%d has no reachable address", a.Leader, topic, partition)
	}

	body, err := json.Marshal(ForwardRequest{
		Topic:     topic,
		Partition: partition,
		Key:       rec.Key,
		Payload:   rec.Payload,
		Headers:   rec.Headers,
		Flags:     rec.Flags,
	})
	if err != nil {
		f.failed.Add(1)
		return stream.AppendResult{}, err
	}

	url := fmt.Sprintf("http://%s/internal/append", leader.AdminAddr)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		f.failed.Add(1)
		return stream.AppendResult{}, err
	}
	req.Header.Set("Content-Type", "application/json")
	if f.cfg.APIKey != "" {
		req.Header.Set("X-API-Key", f.cfg.APIKey)
	}

	resp, err := f.http.Do(req)
	if err != nil {
		f.failed.Add(1)
		return stream.AppendResult{}, fmt.Errorf("forward %s/%d to %s: %w", topic, partition, a.Leader, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		f.failed.Add(1)
		// 421 means the peer has also been demoted — leadership moved twice in
		// quick succession. The caller retries, by which time metadata has
		// caught up; forwarding onward from here would risk a loop.
		return stream.AppendResult{}, fmt.Errorf("forward %s/%d to %s: status %d",
			topic, partition, a.Leader, resp.StatusCode)
	}

	var out ForwardResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		f.failed.Add(1)
		return stream.AppendResult{}, err
	}

	f.forwarded.Add(1)
	// The sequence and timestamp are the leader's. Returning them unchanged is
	// what lets the caller's local echo resolve to the same coordinates every
	// other client will see.
	rec.Seq = out.Seq
	rec.Timestamp = out.Timestamp
	return stream.AppendResult{
		Topic:     out.Topic,
		Partition: out.Partition,
		Seq:       out.Seq,
		Timestamp: out.Timestamp,
	}, nil
}
