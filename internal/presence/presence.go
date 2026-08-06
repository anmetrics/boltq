// Package presence tracks which devices are connected and where.
//
// Delivering a message to a user requires answering "which process holds that
// user's socket right now?". Without an index, the only answer is to broadcast
// every message to every gateway node and let them discard what is not theirs
// — which turns a 10-node cluster into a 10x write amplifier and collapses
// well before the connection counts a consumer app reaches.
//
// The registry is the index. It maps user -> device -> the node and connection
// holding that device's socket, with a TTL so that a node which dies takes its
// entries with it rather than black-holing traffic forever.
package presence

import (
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// State is a device's coarse availability, as reported by the client.
type State string

const (
	// StateOnline means the device holds a live connection and is in the
	// foreground: deliver immediately, no push notification.
	StateOnline State = "online"
	// StateAway means connected but backgrounded. Deliver over the socket, but
	// a push notification is still warranted for a mention.
	StateAway State = "away"
	// StateOffline means no live connection. Everything goes to the outbox.
	StateOffline State = "offline"
)

// Session is one device's live connection.
type Session struct {
	UserID   string `json:"user_id"`
	DeviceID string `json:"device_id"`
	// NodeID identifies the BoltQ process holding the socket. Delivery is
	// routed here.
	NodeID string `json:"node_id"`
	// Region enables locality-aware routing: prefer a same-region replica and
	// only cross an ocean when the user's device is genuinely on the far side.
	Region string `json:"region,omitempty"`
	// ConnID disambiguates two connections from the same device, which happens
	// during a reconnect before the old socket's close is observed.
	ConnID string `json:"conn_id"`
	// Tenant scopes the session in multi-tenant deployments.
	Tenant string `json:"tenant,omitempty"`

	State     State     `json:"state"`
	Since     time.Time `json:"since"`
	LastSeen  time.Time `json:"last_seen"`
	UserAgent string    `json:"user_agent,omitempty"`
}

// Expired reports whether the session's heartbeat has lapsed.
func (s *Session) Expired(now time.Time, ttl time.Duration) bool {
	return now.Sub(s.LastSeen) > ttl
}

// Event describes a change in a user's presence.
type Event struct {
	Type     EventType `json:"type"`
	UserID   string    `json:"user_id"`
	DeviceID string    `json:"device_id"`
	NodeID   string    `json:"node_id"`
	State    State     `json:"state"`
	// UserOnline is the user's aggregate state after this event — true while
	// any device remains connected. Contact lists care about this, not about
	// individual devices.
	UserOnline bool      `json:"user_online"`
	At         time.Time `json:"at"`
}

// EventType enumerates presence transitions.
type EventType string

const (
	// EventBound is a device connecting.
	EventBound EventType = "bound"
	// EventUnbound is a device disconnecting cleanly.
	EventUnbound EventType = "unbound"
	// EventExpired is a device whose heartbeat lapsed — a crash or a network
	// partition, not a clean goodbye.
	EventExpired EventType = "expired"
	// EventStateChanged is a foreground/background transition.
	EventStateChanged EventType = "state_changed"
)

// Config tunes the registry.
type Config struct {
	// TTL is how long a session survives without a heartbeat. Too short and a
	// brief stall evicts a healthy connection; too long and messages route to
	// a dead node until it lapses. Three missed heartbeats is the usual rule.
	TTL time.Duration
	// SweepInterval is how often expired sessions are collected.
	SweepInterval time.Duration
	// NodeID is this process's identifier, stamped onto local sessions.
	NodeID string
	// Region is this process's region.
	Region string
	// WatcherBuffer bounds each watcher's event queue.
	WatcherBuffer int
}

func (c *Config) applyDefaults() {
	if c.TTL <= 0 {
		c.TTL = 90 * time.Second
	}
	if c.SweepInterval <= 0 {
		c.SweepInterval = 15 * time.Second
	}
	if c.WatcherBuffer <= 0 {
		c.WatcherBuffer = 256
	}
}

// shardCount partitions the user table to keep lock contention off the hot
// path. Presence is written on every heartbeat from every device, so a single
// mutex would serialise the whole fleet.
const shardCount = 64

type shard struct {
	mu    sync.RWMutex
	users map[string]map[string]*Session // userID -> deviceID -> session
}

// Registry is the presence index.
type Registry struct {
	cfg    Config
	shards [shardCount]*shard

	watchMu  sync.RWMutex
	watchers map[int64]*watcher
	nextID   int64

	stopOnce sync.Once
	stop     chan struct{}
}

type watcher struct {
	id     int64
	ch     chan Event
	filter func(Event) bool
	// dropped counts events discarded because the watcher fell behind. A slow
	// watcher must never block a heartbeat, so events are dropped rather than
	// queued without bound; the count makes the loss visible. It is atomic
	// because emit runs concurrently under a read lock.
	dropped atomic.Int64
}

// New creates a presence registry.
func New(cfg Config) *Registry {
	cfg.applyDefaults()
	r := &Registry{
		cfg:      cfg,
		watchers: make(map[int64]*watcher),
		stop:     make(chan struct{}),
	}
	for i := range r.shards {
		r.shards[i] = &shard{users: make(map[string]map[string]*Session)}
	}
	go r.sweepLoop()
	return r
}

// fnv1a is inlined rather than imported so the shard function has no
// allocation and no interface dispatch on the heartbeat path.
func shardFor(userID string) uint32 {
	var h uint32 = 2166136261
	for i := 0; i < len(userID); i++ {
		h ^= uint32(userID[i])
		h *= 16777619
	}
	return h % shardCount
}

func (r *Registry) shard(userID string) *shard { return r.shards[shardFor(userID)] }

// Bind registers a device's connection, replacing any previous session for the
// same device.
//
// Replacement rather than rejection is deliberate: when a phone changes network
// it reconnects before the old socket times out, and the newest connection is
// always the right delivery target.
func (r *Registry) Bind(s Session) (*Session, error) {
	if s.UserID == "" || s.DeviceID == "" {
		return nil, errMissingIdentity
	}
	now := time.Now()
	if s.NodeID == "" {
		s.NodeID = r.cfg.NodeID
	}
	if s.Region == "" {
		s.Region = r.cfg.Region
	}
	if s.State == "" {
		s.State = StateOnline
	}
	s.Since = now
	s.LastSeen = now

	sh := r.shard(s.UserID)
	sh.mu.Lock()
	devices, ok := sh.users[s.UserID]
	if !ok {
		devices = make(map[string]*Session, 2)
		sh.users[s.UserID] = devices
	}
	stored := s
	devices[s.DeviceID] = &stored
	sh.mu.Unlock()

	r.emit(Event{
		Type: EventBound, UserID: s.UserID, DeviceID: s.DeviceID,
		NodeID: s.NodeID, State: s.State, UserOnline: true, At: now,
	})
	return &stored, nil
}

// Touch refreshes a session's heartbeat. It returns false when the session is
// unknown, which tells the caller to re-Bind — the usual cause is that a sweep
// evicted the session during a network stall.
func (r *Registry) Touch(userID, deviceID string) bool {
	now := time.Now()
	sh := r.shard(userID)
	sh.mu.Lock()
	defer sh.mu.Unlock()

	s, ok := sh.users[userID][deviceID]
	if !ok {
		return false
	}
	s.LastSeen = now
	return true
}

// SetState records a foreground/background transition.
func (r *Registry) SetState(userID, deviceID string, state State) bool {
	now := time.Now()
	sh := r.shard(userID)
	sh.mu.Lock()
	s, ok := sh.users[userID][deviceID]
	if !ok {
		sh.mu.Unlock()
		return false
	}
	if s.State == state {
		s.LastSeen = now
		sh.mu.Unlock()
		return true
	}
	s.State = state
	s.LastSeen = now
	nodeID := s.NodeID
	online := len(sh.users[userID]) > 0
	sh.mu.Unlock()

	r.emit(Event{
		Type: EventStateChanged, UserID: userID, DeviceID: deviceID,
		NodeID: nodeID, State: state, UserOnline: online, At: now,
	})
	return true
}

// Unbind removes a device's session.
//
// connID guards against a reconnect race: a delayed close for an old
// connection must not evict the new one that has already replaced it. Pass an
// empty connID to remove unconditionally.
func (r *Registry) Unbind(userID, deviceID, connID string) bool {
	now := time.Now()
	sh := r.shard(userID)

	sh.mu.Lock()
	devices := sh.users[userID]
	s, ok := devices[deviceID]
	if !ok {
		sh.mu.Unlock()
		return false
	}
	if connID != "" && s.ConnID != connID {
		sh.mu.Unlock()
		return false // a newer connection owns this device
	}
	delete(devices, deviceID)
	if len(devices) == 0 {
		delete(sh.users, userID)
	}
	stillOnline := len(devices) > 0
	nodeID := s.NodeID
	sh.mu.Unlock()

	r.emit(Event{
		Type: EventUnbound, UserID: userID, DeviceID: deviceID,
		NodeID: nodeID, State: StateOffline, UserOnline: stillOnline, At: now,
	})
	return true
}

// Sessions returns a user's live sessions, newest first.
func (r *Registry) Sessions(userID string) []Session {
	sh := r.shard(userID)
	sh.mu.RLock()
	devices := sh.users[userID]
	out := make([]Session, 0, len(devices))
	for _, s := range devices {
		out = append(out, *s)
	}
	sh.mu.RUnlock()

	sort.Slice(out, func(i, j int) bool { return out[i].Since.After(out[j].Since) })
	return out
}

// Session returns one device's session.
func (r *Registry) Session(userID, deviceID string) (Session, bool) {
	sh := r.shard(userID)
	sh.mu.RLock()
	defer sh.mu.RUnlock()
	s, ok := sh.users[userID][deviceID]
	if !ok {
		return Session{}, false
	}
	return *s, true
}

// Online reports whether the user has any live device.
func (r *Registry) Online(userID string) bool {
	sh := r.shard(userID)
	sh.mu.RLock()
	defer sh.mu.RUnlock()
	return len(sh.users[userID]) > 0
}

// Route is a delivery target for one device.
type Route struct {
	UserID   string
	DeviceID string
	NodeID   string
	Region   string
	ConnID   string
	Local    bool // true when the socket lives in this process
}

// Routes returns delivery targets for a user, local sessions first.
//
// Ordering matters: a local delivery is an in-process channel send, while a
// remote one costs a network hop. Trying local first keeps the common case —
// both parties pinned to the same node by a sticky load balancer — free.
func (r *Registry) Routes(userID string) []Route {
	sessions := r.Sessions(userID)
	routes := make([]Route, 0, len(sessions))
	for _, s := range sessions {
		routes = append(routes, Route{
			UserID: s.UserID, DeviceID: s.DeviceID, NodeID: s.NodeID,
			Region: s.Region, ConnID: s.ConnID, Local: s.NodeID == r.cfg.NodeID,
		})
	}
	sort.SliceStable(routes, func(i, j int) bool { return routes[i].Local && !routes[j].Local })
	return routes
}

// RoutesForUsers batches Routes across many users — the group fan-out path,
// where asking one user at a time would mean thousands of lock acquisitions
// per message.
func (r *Registry) RoutesForUsers(userIDs []string) map[string][]Route {
	out := make(map[string][]Route, len(userIDs))
	for _, u := range userIDs {
		if rs := r.Routes(u); len(rs) > 0 {
			out[u] = rs
		}
	}
	return out
}

// OfflineUsers filters a list down to those with no live device — the set that
// needs a push notification rather than a socket write.
func (r *Registry) OfflineUsers(userIDs []string) []string {
	var out []string
	for _, u := range userIDs {
		if !r.Online(u) {
			out = append(out, u)
		}
	}
	return out
}

// Stats summarises the registry.
type Stats struct {
	Users    int            `json:"users"`
	Sessions int            `json:"sessions"`
	ByNode   map[string]int `json:"by_node"`
	ByRegion map[string]int `json:"by_region"`
	ByState  map[State]int  `json:"by_state"`
	Watchers int            `json:"watchers"`
}

// Stats walks every shard. It is O(sessions) and meant for the admin endpoint,
// not for a per-message call.
func (r *Registry) Stats() Stats {
	st := Stats{
		ByNode:   make(map[string]int),
		ByRegion: make(map[string]int),
		ByState:  make(map[State]int),
	}
	for _, sh := range r.shards {
		sh.mu.RLock()
		st.Users += len(sh.users)
		for _, devices := range sh.users {
			for _, s := range devices {
				st.Sessions++
				st.ByNode[s.NodeID]++
				if s.Region != "" {
					st.ByRegion[s.Region]++
				}
				st.ByState[s.State]++
			}
		}
		sh.mu.RUnlock()
	}

	r.watchMu.RLock()
	st.Watchers = len(r.watchers)
	r.watchMu.RUnlock()
	return st
}

// Watch subscribes to presence events. The returned cancel function must be
// called to release the watcher.
//
// filter runs inside the emitting goroutine, so keep it cheap — it typically
// checks whether the changed user is in the watcher's contact list.
func (r *Registry) Watch(filter func(Event) bool) (<-chan Event, func()) {
	r.watchMu.Lock()
	r.nextID++
	w := &watcher{id: r.nextID, ch: make(chan Event, r.cfg.WatcherBuffer), filter: filter}
	r.watchers[w.id] = w
	r.watchMu.Unlock()

	var once sync.Once
	cancel := func() {
		once.Do(func() {
			r.watchMu.Lock()
			delete(r.watchers, w.id)
			r.watchMu.Unlock()
			close(w.ch)
		})
	}
	return w.ch, cancel
}

// WatchUsers subscribes to events about a fixed set of users — the contact-list
// case.
func (r *Registry) WatchUsers(userIDs []string) (<-chan Event, func()) {
	set := make(map[string]bool, len(userIDs))
	for _, u := range userIDs {
		set[u] = true
	}
	return r.Watch(func(e Event) bool { return set[e.UserID] })
}

func (r *Registry) emit(e Event) {
	r.watchMu.RLock()
	defer r.watchMu.RUnlock()

	for _, w := range r.watchers {
		if w.filter != nil && !w.filter(e) {
			continue
		}
		select {
		case w.ch <- e:
		default:
			w.dropped.Add(1)
		}
	}
}

func (r *Registry) sweepLoop() {
	ticker := time.NewTicker(r.cfg.SweepInterval)
	defer ticker.Stop()
	for {
		select {
		case <-r.stop:
			return
		case <-ticker.C:
			r.Sweep()
		}
	}
}

// Sweep evicts sessions whose heartbeat has lapsed. It returns the number
// removed. Exported so tests and operators can force a pass.
func (r *Registry) Sweep() int {
	now := time.Now()
	ttl := r.cfg.TTL
	var expired []Event

	for _, sh := range r.shards {
		sh.mu.Lock()
		for userID, devices := range sh.users {
			for deviceID, s := range devices {
				if !s.Expired(now, ttl) {
					continue
				}
				delete(devices, deviceID)
				expired = append(expired, Event{
					Type: EventExpired, UserID: userID, DeviceID: deviceID,
					NodeID: s.NodeID, State: StateOffline, At: now,
				})
			}
			if len(devices) == 0 {
				delete(sh.users, userID)
			}
		}
		sh.mu.Unlock()
	}

	// Emit outside the shard locks: a watcher's filter is caller-supplied and
	// must not run while a shard is held.
	for i := range expired {
		expired[i].UserOnline = r.Online(expired[i].UserID)
		r.emit(expired[i])
	}
	return len(expired)
}

// EvictNode removes every session belonging to a node. Call it when a node
// leaves the cluster so its users are marked offline immediately rather than
// after a TTL of black-holed deliveries.
func (r *Registry) EvictNode(nodeID string) int {
	now := time.Now()
	var evicted []Event

	for _, sh := range r.shards {
		sh.mu.Lock()
		for userID, devices := range sh.users {
			for deviceID, s := range devices {
				if s.NodeID != nodeID {
					continue
				}
				delete(devices, deviceID)
				evicted = append(evicted, Event{
					Type: EventExpired, UserID: userID, DeviceID: deviceID,
					NodeID: nodeID, State: StateOffline, At: now,
				})
			}
			if len(devices) == 0 {
				delete(sh.users, userID)
			}
		}
		sh.mu.Unlock()
	}

	for i := range evicted {
		evicted[i].UserOnline = r.Online(evicted[i].UserID)
		r.emit(evicted[i])
	}
	return len(evicted)
}

// Close stops the sweeper and releases every watcher.
func (r *Registry) Close() {
	r.stopOnce.Do(func() {
		close(r.stop)
		r.watchMu.Lock()
		for id, w := range r.watchers {
			delete(r.watchers, id)
			close(w.ch)
		}
		r.watchMu.Unlock()
	})
}

// PresenceTopic is the conventional topic on which a user's presence is
// published, matching the ACL rules in identity.ChatPolicyRules.
func PresenceTopic(userID string) string {
	return "presence." + userID
}

// UserFromPresenceTopic extracts the user ID from a presence topic.
func UserFromPresenceTopic(topic string) (string, bool) {
	rest, ok := strings.CutPrefix(topic, "presence.")
	if !ok || rest == "" {
		return "", false
	}
	if i := strings.IndexByte(rest, '.'); i >= 0 {
		rest = rest[:i]
	}
	return rest, true
}

type presenceError string

func (e presenceError) Error() string { return string(e) }

const errMissingIdentity presenceError = "presence: user_id and device_id are required"
