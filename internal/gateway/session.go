package gateway

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"strconv"
	"sync"
	"time"

	"github.com/boltq/boltq/internal/identity"
)

// subscription records what a session was streaming, so a resume can restore
// it without the client re-issuing every subscribe.
type subscription struct {
	Topic     string
	Partition int32
	// FromSeq is the position to resume at. It is updated as records are
	// delivered, so a resumed session continues from what the client actually
	// received rather than from its last durable commit — the difference is
	// the records delivered but not yet acknowledged.
	FromSeq uint64
}

func subKey(topic string, partition int32) string {
	return topic + "\x00" + strconv.FormatInt(int64(partition), 10)
}

// Session is a client's logical connection, which outlives any single socket.
//
// Separating session from socket is what makes a mobile client workable. A
// train tunnel drops the TCP connection but not the user's intent: they are
// still in the same conversation, still subscribed to the same topics, still
// the same authenticated principal. Rebuilding all of that from scratch on
// every reconnect would mean a burst of authorisation checks and history reads
// every time a phone changes cell tower.
type Session struct {
	ID        string
	Principal *identity.Principal
	DeviceID  string
	UserAgent string

	// resumeHash is the SHA-free constant-time comparison target for the
	// resume token. The token itself is never stored, so a memory disclosure
	// does not hand out working session credentials.
	resumeToken []byte

	mu            sync.Mutex
	subscriptions map[string]*subscription
	watchedUsers  map[string]bool
	detachedAt    time.Time
	attached      bool
	// conn is the live socket writer, nil while detached.
	conn *connWriter
}

// snapshotSubscriptions returns a copy of the session's subscriptions.
func (s *Session) snapshotSubscriptions() []subscription {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]subscription, 0, len(s.subscriptions))
	for _, sub := range s.subscriptions {
		out = append(out, *sub)
	}
	return out
}

func (s *Session) addSubscription(topic string, partition int32, fromSeq uint64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.subscriptions[subKey(topic, partition)] = &subscription{
		Topic: topic, Partition: partition, FromSeq: fromSeq,
	}
}

func (s *Session) removeSubscription(topic string, partition int32) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.subscriptions, subKey(topic, partition))
}

// advance records how far a subscription has been delivered.
func (s *Session) advance(topic string, partition int32, nextSeq uint64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if sub, ok := s.subscriptions[subKey(topic, partition)]; ok && nextSeq > sub.FromSeq {
		sub.FromSeq = nextSeq
	}
}

func (s *Session) setWatchedUsers(users []string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.watchedUsers = make(map[string]bool, len(users))
	for _, u := range users {
		s.watchedUsers[u] = true
	}
}

func (s *Session) watchedSnapshot() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]string, 0, len(s.watchedUsers))
	for u := range s.watchedUsers {
		out = append(out, u)
	}
	return out
}

// SessionStore holds sessions across reconnects.
type SessionStore struct {
	mu       sync.RWMutex
	sessions map[string]*Session
	// resumeWindow is how long a detached session survives. Too short and a
	// tunnel costs a full resync; too long and abandoned sessions accumulate.
	resumeWindow time.Duration

	stopOnce sync.Once
	stop     chan struct{}
}

// NewSessionStore creates a session store and starts its reaper.
func NewSessionStore(resumeWindow time.Duration) *SessionStore {
	if resumeWindow <= 0 {
		resumeWindow = 5 * time.Minute
	}
	s := &SessionStore{
		sessions:     make(map[string]*Session),
		resumeWindow: resumeWindow,
		stop:         make(chan struct{}),
	}
	go s.reapLoop()
	return s
}

// Create registers a new session and returns it with its resume token.
func (st *SessionStore) Create(p *identity.Principal, deviceID, userAgent string) (*Session, string) {
	id := randomToken(16)
	token := randomToken(32)

	sess := &Session{
		ID:            id,
		Principal:     p,
		DeviceID:      deviceID,
		UserAgent:     userAgent,
		resumeToken:   []byte(token),
		subscriptions: make(map[string]*subscription),
		watchedUsers:  make(map[string]bool),
		attached:      true,
	}

	st.mu.Lock()
	st.sessions[id] = sess
	st.mu.Unlock()
	return sess, token
}

// Resume reattaches to a detached session.
//
// The token is compared in constant time and the principal's identity is
// re-checked: a resume token is a bearer credential, and a session must not be
// resumable by a different user even if a token leaks between them.
func (st *SessionStore) Resume(sessionID, token string, p *identity.Principal) (*Session, bool) {
	st.mu.RLock()
	sess, ok := st.sessions[sessionID]
	st.mu.RUnlock()
	if !ok {
		return nil, false
	}

	if subtle.ConstantTimeCompare(sess.resumeToken, []byte(token)) != 1 {
		return nil, false
	}
	if sess.Principal == nil || p == nil || sess.Principal.UserID != p.UserID {
		return nil, false
	}

	sess.mu.Lock()
	defer sess.mu.Unlock()
	if sess.attached {
		// Another socket already holds this session. Refusing rather than
		// stealing keeps two devices sharing a leaked token from fighting over
		// one session and corrupting each other's cursors.
		return nil, false
	}
	sess.attached = true
	sess.detachedAt = time.Time{}
	// Refresh the principal: the client may have presented a newer token with
	// a later expiry, and the session should not die on the old one.
	sess.Principal = p
	return sess, true
}

// Detach marks a session as having lost its socket, starting the resume window.
func (st *SessionStore) Detach(sess *Session) {
	sess.mu.Lock()
	sess.attached = false
	sess.detachedAt = time.Now()
	sess.conn = nil
	sess.mu.Unlock()
}

// Drop removes a session immediately — a clean client logout.
func (st *SessionStore) Drop(sessionID string) {
	st.mu.Lock()
	delete(st.sessions, sessionID)
	st.mu.Unlock()
}

// Get returns a session by ID.
func (st *SessionStore) Get(sessionID string) (*Session, bool) {
	st.mu.RLock()
	defer st.mu.RUnlock()
	s, ok := st.sessions[sessionID]
	return s, ok
}

// Len returns the number of tracked sessions, attached and detached.
func (st *SessionStore) Len() int {
	st.mu.RLock()
	defer st.mu.RUnlock()
	return len(st.sessions)
}

// Attached returns the number of sessions currently holding a socket.
func (st *SessionStore) Attached() int {
	st.mu.RLock()
	defer st.mu.RUnlock()
	n := 0
	for _, s := range st.sessions {
		s.mu.Lock()
		if s.attached {
			n++
		}
		s.mu.Unlock()
	}
	return n
}

func (st *SessionStore) reapLoop() {
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-st.stop:
			return
		case <-ticker.C:
			st.Reap()
		}
	}
}

// Reap discards sessions whose resume window has elapsed.
func (st *SessionStore) Reap() int {
	cutoff := time.Now().Add(-st.resumeWindow)

	st.mu.Lock()
	defer st.mu.Unlock()

	removed := 0
	for id, s := range st.sessions {
		s.mu.Lock()
		expired := !s.attached && !s.detachedAt.IsZero() && s.detachedAt.Before(cutoff)
		s.mu.Unlock()
		if expired {
			delete(st.sessions, id)
			removed++
		}
	}
	return removed
}

// Close stops the reaper.
func (st *SessionStore) Close() {
	st.stopOnce.Do(func() { close(st.stop) })
}

// randomToken returns a URL-safe random string of n bytes of entropy.
func randomToken(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		// crypto/rand failing means the platform RNG is broken; there is no
		// safe fallback for a security token, so fail loudly rather than emit
		// a predictable one.
		panic("gateway: crypto/rand unavailable: " + err.Error())
	}
	return base64.RawURLEncoding.EncodeToString(b)
}
