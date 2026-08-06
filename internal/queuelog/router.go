package queuelog

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"sync"
	"time"

	"github.com/boltq/boltq/internal/broker"
	"github.com/boltq/boltq/internal/stream"
)

// Header keys the router writes onto records. They are the record's routing
// provenance, and dead-lettering depends on them: a message that has been
// dead-lettered must still know where it came from and why.
const (
	HeaderExchange     = "x-exchange"
	HeaderRoutingKey   = "x-routing-key"
	HeaderDeathReason  = "x-death-reason"
	HeaderDeathQueue   = "x-death-queue"
	HeaderDeathCount   = "x-death-count"
	HeaderMessageID    = "x-message-id"
	HeaderFirstDeathAt = "x-first-death-at"
)

var (
	// ErrNoSuchExchange means the named exchange was never declared.
	ErrNoSuchExchange = errors.New("queuelog: no such exchange")
	// ErrUnroutable means no binding matched and no alternate exchange was
	// configured. Returning it rather than dropping is the difference between a
	// publisher that can react and one that cannot.
	ErrUnroutable = errors.New("queuelog: message matched no binding")
)

// QueueSpec declares a queue's delivery policy.
//
// This is the AMQP queue-arguments surface, expressed as a struct instead of a
// map of magic x- keys: the same knobs, checked at compile time.
type QueueSpec struct {
	Name       string
	Partitions int32
	// Group defaults to Name.
	Group       string
	AckTimeout  time.Duration
	MaxDelivery int
	MaxInFlight int

	// DeadLetterExchange is where records go once MaxDelivery is exhausted or a
	// consumer rejects them outright. Routing dead letters through an exchange
	// rather than into a fixed "<queue>_dead_letter" is what makes the policy
	// composable: several queues can share one dead-letter queue, or each can
	// have its own, without the broker knowing which.
	DeadLetterExchange string
	// DeadLetterRoutingKey overrides the original routing key on the way out.
	// Empty keeps the original, which is what lets one dead-letter exchange
	// separate its inputs by where they came from.
	DeadLetterRoutingKey string
}

// Router is the exchange layer: it declares exchanges, bindings and queues, and
// routes published records into the queues whose bindings match.
//
// It reuses broker.Exchange for the matching rules rather than reimplementing
// them. Direct, fanout, topic and headers matching — including the '*' and '#'
// wildcard semantics — are already correct there, and a second implementation
// would only be a second thing to keep in sync.
type Router struct {
	log     *stream.Log
	cursors *stream.CursorStore

	mu        sync.RWMutex
	exchanges map[string]*broker.Exchange
	queues    map[string]*Queue
	specs     map[string]QueueSpec
	closed    bool
}

// NewRouter creates a router over a stream log.
//
// The default exchange ("") is declared as direct, so publishing with a routing
// key equal to a queue name delivers to that queue — the AMQP behaviour every
// client depends on.
func NewRouter(log *stream.Log, cursors *stream.CursorStore) (*Router, error) {
	if log == nil {
		return nil, errors.New("queuelog: a stream log is required")
	}
	r := &Router{
		log:       log,
		cursors:   cursors,
		exchanges: make(map[string]*broker.Exchange),
		queues:    make(map[string]*Queue),
		specs:     make(map[string]QueueSpec),
	}
	r.exchanges[""] = &broker.Exchange{Name: "", Type: broker.ExchangeDirect, Durable: true}
	return r, nil
}

// DeclareExchange creates an exchange, or returns the existing one when the
// type matches. A type mismatch is an error rather than a silent redeclaration,
// because changing an exchange's type changes where every existing binding
// sends its messages.
func (r *Router) DeclareExchange(name string, typ broker.ExchangeType, durable bool) (*broker.Exchange, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return nil, stream.ErrClosed
	}

	if ex, ok := r.exchanges[name]; ok {
		if ex.Type != typ {
			return nil, fmt.Errorf("queuelog: exchange %q already declared as %s (requested %s)",
				name, ex.Type, typ)
		}
		return ex, nil
	}
	ex := &broker.Exchange{Name: name, Type: typ, Durable: durable}
	r.exchanges[name] = ex
	return ex, nil
}

// DeclareQueue creates or opens a queue and wires its dead-letter policy.
func (r *Router) DeclareQueue(spec QueueSpec) (*Queue, error) {
	if spec.Name == "" {
		return nil, errors.New("queuelog: queue name is required")
	}

	r.mu.Lock()
	if r.closed {
		r.mu.Unlock()
		return nil, stream.ErrClosed
	}
	if q, ok := r.queues[spec.Name]; ok {
		r.mu.Unlock()
		return q, nil
	}
	r.mu.Unlock()

	// Open outside the lock: it touches the filesystem, and holding the router
	// lock across a topic creation would stall every publisher on the node.
	q, err := Open(r.log, r.cursors, spec.Name, Config{
		Group:       spec.Group,
		AckTimeout:  spec.AckTimeout,
		MaxDelivery: spec.MaxDelivery,
		MaxInFlight: spec.MaxInFlight,
		Partitions:  spec.Partitions,
		DeadLetter:  r.deadLetterFunc(spec),
	})
	if err != nil {
		return nil, err
	}

	r.mu.Lock()
	defer r.mu.Unlock()
	// Another goroutine may have declared the same queue while we were opening.
	// Keep the first one and discard ours, so two callers never end up with two
	// windows over the same partitions — which would deliver every record twice.
	if existing, ok := r.queues[spec.Name]; ok {
		q.Close()
		return existing, nil
	}
	r.queues[spec.Name] = q
	r.specs[spec.Name] = spec
	return q, nil
}

// deadLetterFunc builds the dead-letter route for a queue.
//
// A queue with no dead-letter exchange returns an error from the route, which
// the share partition treats as "keep the record available". That is deliberate:
// discarding is a policy an operator must opt into by declaring a dead-letter
// exchange that goes nowhere, not a default that loses messages silently.
func (r *Router) deadLetterFunc(spec QueueSpec) DeadLetterFunc {
	if spec.DeadLetterExchange == "" {
		return nil
	}
	return func(topic string, partition int32, rec *stream.Record, reason string) error {
		key := spec.DeadLetterRoutingKey
		if key == "" {
			key = headerOr(rec, HeaderRoutingKey, topic)
		}

		dead := &stream.Record{
			Key:     rec.Key,
			Payload: rec.Payload,
			Headers: cloneHeaders(rec.Headers),
			Flags:   rec.Flags,
		}
		if dead.Headers == nil {
			dead.Headers = make(map[string]string)
		}
		dead.Headers[HeaderDeathReason] = reason
		dead.Headers[HeaderDeathQueue] = topic
		dead.Headers[HeaderDeathCount] = strconv.Itoa(deathCount(rec) + 1)
		if _, ok := dead.Headers[HeaderFirstDeathAt]; !ok {
			dead.Headers[HeaderFirstDeathAt] = strconv.FormatInt(rec.Timestamp, 10)
		}

		_, err := r.Publish(context.Background(), spec.DeadLetterExchange, key, dead)
		if errors.Is(err, ErrUnroutable) {
			// An unroutable dead letter is not worth redelivering forever: the
			// operator declared a dead-letter exchange and bound nothing to it,
			// which is an explicit "discard". Retrying would pin the window base
			// and stall the queue instead.
			return nil
		}
		return err
	}
}

// Publish routes a record through an exchange to every matching queue.
//
// Each matching queue gets its own append, because each has its own log and its
// own delivery state — a record cannot be shared across queues the way an
// in-memory broker shares a pointer. In exchange, a dead consumer on one queue
// cannot affect any other, and every queue's history is independently
// replayable.
func (r *Router) Publish(ctx context.Context, exchange, routingKey string, rec *stream.Record) ([]stream.AppendResult, error) {
	r.mu.RLock()
	if r.closed {
		r.mu.RUnlock()
		return nil, stream.ErrClosed
	}
	ex, ok := r.exchanges[exchange]
	r.mu.RUnlock()
	if !ok {
		return nil, fmt.Errorf("%w: %q", ErrNoSuchExchange, exchange)
	}

	if rec.Headers == nil {
		rec.Headers = make(map[string]string)
	}
	rec.Headers[HeaderExchange] = exchange
	rec.Headers[HeaderRoutingKey] = routingKey

	targets := r.route(ex, routingKey, rec.Headers)
	if len(targets) == 0 {
		return nil, fmt.Errorf("%w: exchange %q key %q", ErrUnroutable, exchange, routingKey)
	}

	out := make([]stream.AppendResult, 0, len(targets))
	for _, name := range targets {
		r.mu.RLock()
		q := r.queues[name]
		r.mu.RUnlock()
		if q == nil {
			// A binding to a queue that was never declared. Skipping it rather
			// than failing keeps one stale binding from breaking every publish
			// that happens to match it.
			continue
		}

		// Each queue gets its own copy: Append stamps Seq, Epoch and Timestamp
		// into the record, so publishing the same pointer twice would leave the
		// first result describing the second append.
		copyRec := &stream.Record{
			Key:     rec.Key,
			Payload: rec.Payload,
			Headers: cloneHeaders(rec.Headers),
			Flags:   rec.Flags,
		}
		res, err := q.Publish(ctx, copyRec)
		if err != nil {
			return out, fmt.Errorf("queuelog: publish to %s: %w", name, err)
		}
		out = append(out, res)
	}

	if len(out) == 0 {
		return nil, fmt.Errorf("%w: exchange %q key %q matched only undeclared queues",
			ErrUnroutable, exchange, routingKey)
	}
	return out, nil
}

// route resolves an exchange to matching queue names. The default exchange
// routes straight to the queue named by the routing key.
func (r *Router) route(ex *broker.Exchange, routingKey string, headers map[string]string) []string {
	if ex.Name == "" {
		r.mu.RLock()
		_, ok := r.queues[routingKey]
		r.mu.RUnlock()
		if !ok {
			return nil
		}
		return []string{routingKey}
	}
	return ex.Route(routingKey, headers)
}

// Bind attaches a queue to an exchange.
func (r *Router) Bind(exchange, queue, bindingKey string, headers map[string]string, matchAll bool) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return stream.ErrClosed
	}

	ex, ok := r.exchanges[exchange]
	if !ok {
		return fmt.Errorf("%w: %q", ErrNoSuchExchange, exchange)
	}
	if _, ok := r.queues[queue]; !ok {
		return fmt.Errorf("queuelog: no such queue %q", queue)
	}
	ex.Bind(&broker.Binding{
		Exchange: exchange, Queue: queue, BindingKey: bindingKey,
		Headers: headers, MatchAll: matchAll,
	})
	return nil
}

// Unbind removes a binding.
func (r *Router) Unbind(exchange, queue, bindingKey string) error {
	r.mu.RLock()
	ex, ok := r.exchanges[exchange]
	r.mu.RUnlock()
	if !ok {
		return fmt.Errorf("%w: %q", ErrNoSuchExchange, exchange)
	}
	ex.Unbind(queue, bindingKey)
	return nil
}

// Queue returns a declared queue.
func (r *Router) Queue(name string) (*Queue, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	q, ok := r.queues[name]
	if !ok {
		return nil, fmt.Errorf("queuelog: no such queue %q", name)
	}
	return q, nil
}

// QueueNames lists declared queues.
func (r *Router) QueueNames() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]string, 0, len(r.queues))
	for name := range r.queues {
		out = append(out, name)
	}
	return out
}

// Stats returns per-queue delivery statistics.
func (r *Router) Stats() []Stats {
	r.mu.RLock()
	qs := make([]*Queue, 0, len(r.queues))
	for _, q := range r.queues {
		qs = append(qs, q)
	}
	r.mu.RUnlock()

	out := make([]Stats, 0, len(qs))
	for _, q := range qs {
		out = append(out, q.Stats())
	}
	return out
}

// Close shuts every queue down. The log is not closed: the router does not own
// it, and the stream plane may still be serving readers from it.
func (r *Router) Close() {
	r.mu.Lock()
	if r.closed {
		r.mu.Unlock()
		return
	}
	r.closed = true
	qs := make([]*Queue, 0, len(r.queues))
	for _, q := range r.queues {
		qs = append(qs, q)
	}
	r.mu.Unlock()

	for _, q := range qs {
		q.Close()
	}
}

func cloneHeaders(h map[string]string) map[string]string {
	if len(h) == 0 {
		return nil
	}
	out := make(map[string]string, len(h))
	for k, v := range h {
		out[k] = v
	}
	return out
}

func headerOr(rec *stream.Record, key, def string) string {
	if v, ok := rec.Headers[key]; ok && v != "" {
		return v
	}
	return def
}

func deathCount(rec *stream.Record) int {
	n, err := strconv.Atoi(rec.Headers[HeaderDeathCount])
	if err != nil || n < 0 {
		return 0
	}
	return n
}
