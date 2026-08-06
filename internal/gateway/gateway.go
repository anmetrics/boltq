package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"

	"github.com/boltq/boltq/internal/ephemeral"
	"github.com/boltq/boltq/internal/fanout"
	"github.com/boltq/boltq/internal/identity"
	"github.com/boltq/boltq/internal/presence"
	"github.com/boltq/boltq/internal/stream"
)

// Config tunes the gateway.
type Config struct {
	// ReadLimit caps an inbound frame. It bounds how much memory one hostile
	// client can make the server allocate per message.
	ReadLimit int64
	// WriteTimeout bounds a single socket write. A client that stops reading
	// must not pin a goroutine and its buffered records indefinitely.
	WriteTimeout time.Duration
	// PongTimeout is how long to wait for a pong before declaring the socket
	// dead. Mobile clients vanish without closing, so an idle socket must be
	// reaped by liveness probing, not by waiting for a FIN that never comes.
	PongTimeout time.Duration
	// PingInterval is how often to probe. It must be well under PongTimeout.
	PingInterval time.Duration
	// SendBuffer bounds the per-connection outbound queue.
	SendBuffer int
	// ResumeWindow is how long a detached session can be resumed.
	ResumeWindow time.Duration
	// MaxSubscriptions caps concurrent subscriptions per session — each one
	// costs a goroutine, so an unbounded count is a denial-of-service vector.
	MaxSubscriptions int
	// HistoryLimit caps records returned by one history request.
	HistoryLimit int
	// AllowedOrigins restricts the WebSocket Origin header. Empty disables the
	// check, which is correct for native mobile clients (they send no Origin)
	// but must be set when browsers connect.
	AllowedOrigins []string
	// NodeID and Region identify this process in the presence registry.
	NodeID string
	Region string
}

func (c *Config) applyDefaults() {
	if c.ReadLimit <= 0 {
		c.ReadLimit = 1 << 20 // 1MB
	}
	if c.WriteTimeout <= 0 {
		c.WriteTimeout = 10 * time.Second
	}
	if c.PongTimeout <= 0 {
		c.PongTimeout = 90 * time.Second
	}
	if c.PingInterval <= 0 {
		c.PingInterval = c.PongTimeout / 3
	}
	if c.SendBuffer <= 0 {
		c.SendBuffer = 256
	}
	if c.ResumeWindow <= 0 {
		c.ResumeWindow = 5 * time.Minute
	}
	if c.MaxSubscriptions <= 0 {
		c.MaxSubscriptions = 200
	}
	if c.HistoryLimit <= 0 {
		c.HistoryLimit = 200
	}
}

// Gateway serves the WebSocket edge.
type Gateway struct {
	cfg      Config
	log      *stream.Log
	cursors  *stream.CursorStore
	deliver  *fanout.Deliverer
	presence *presence.Registry
	// directory publishes this node's sessions to whichever node owns the
	// user's presence shard. Nil without a control plane, where the local
	// registry is the whole cluster's view.
	directory *presence.Directory
	signals   *ephemeral.Hub
	policy    *identity.Policy
	verifier  *identity.Verifier
	// apiKey preserves the shared-key path for trusted backend services. A
	// connection authenticated this way becomes an anonymous principal.
	apiKey string

	sessions *SessionStore
	upgrader websocket.Upgrader

	mu    sync.Mutex
	stats Stats

	// lifecycle guards the connection WaitGroup. sync.WaitGroup forbids an Add
	// that races a Wait, and a request arriving while Close runs would do
	// exactly that. Serving takes the read lock and Close takes the write lock,
	// so no Add can begin once Wait is about to start.
	lifecycle    sync.RWMutex
	shuttingDown bool

	wg   sync.WaitGroup
	once sync.Once
}

// Stats counts gateway activity.
type Stats struct {
	Connections     uint64 `json:"connections"`
	Resumed         uint64 `json:"resumed"`
	AuthFailures    uint64 `json:"auth_failures"`
	Forbidden       uint64 `json:"forbidden"`
	FramesIn        uint64 `json:"frames_in"`
	FramesOut       uint64 `json:"frames_out"`
	RecordsOut      uint64 `json:"records_out"`
	SlowClientDrops uint64 `json:"slow_client_drops"`
	Sessions        int    `json:"sessions"`
	Attached        int    `json:"attached"`
}

// Options assembles a Gateway.
type Options struct {
	Log      *stream.Log
	Cursors  *stream.CursorStore
	Deliver  *fanout.Deliverer
	Presence *presence.Registry
	// Directory, when set, makes presence visible to the rest of the cluster.
	// Without it a session is known only to the node holding the socket, and
	// every other node's fan-out and push decisions are made blind to it.
	Directory *presence.Directory
	Signals   *ephemeral.Hub
	Policy    *identity.Policy
	Verifier  *identity.Verifier
	APIKey    string
	Config    Config
}

// New builds a Gateway.
func New(opts Options) (*Gateway, error) {
	if opts.Log == nil {
		return nil, errors.New("gateway: a stream log is required")
	}
	if opts.Cursors == nil {
		return nil, errors.New("gateway: a cursor store is required")
	}
	if opts.Policy == nil {
		return nil, errors.New("gateway: an authorisation policy is required")
	}
	if opts.Verifier == nil && opts.APIKey == "" {
		return nil, errors.New("gateway: a token verifier or an API key is required")
	}
	opts.Config.applyDefaults()

	g := &Gateway{
		cfg:       opts.Config,
		log:       opts.Log,
		cursors:   opts.Cursors,
		deliver:   opts.Deliver,
		presence:  opts.Presence,
		directory: opts.Directory,
		signals:   opts.Signals,
		policy:    opts.Policy,
		verifier:  opts.Verifier,
		apiKey:    opts.APIKey,
		sessions:  NewSessionStore(opts.Config.ResumeWindow),
	}

	allowed := make(map[string]bool, len(opts.Config.AllowedOrigins))
	for _, o := range opts.Config.AllowedOrigins {
		allowed[strings.ToLower(o)] = true
	}
	g.upgrader = websocket.Upgrader{
		ReadBufferSize:  4096,
		WriteBufferSize: 4096,
		CheckOrigin: func(r *http.Request) bool {
			if len(allowed) == 0 {
				return true
			}
			origin := r.Header.Get("Origin")
			if origin == "" {
				return true // native client, no Origin header
			}
			return allowed[strings.ToLower(origin)]
		},
	}
	return g, nil
}

// connWriter serialises writes to a WebSocket.
//
// gorilla/websocket permits only one concurrent writer, and this gateway has
// many: the read loop's responses, one goroutine per subscription, the signal
// pump, and the ping ticker. A single queue with one drain goroutine is the
// only sane way to hold that invariant.
type connWriter struct {
	conn    *websocket.Conn
	queue   chan Frame
	timeout time.Duration
	done    chan struct{}
	once    sync.Once
	// dropped counts frames discarded because the client stopped reading.
	dropped uint64
	mu      sync.Mutex
}

func newConnWriter(conn *websocket.Conn, buffer int, timeout time.Duration) *connWriter {
	return &connWriter{
		conn:    conn,
		queue:   make(chan Frame, buffer),
		timeout: timeout,
		done:    make(chan struct{}),
	}
}

// send enqueues a frame. It never blocks: a client that has stopped reading is
// disconnected rather than allowed to apply backpressure to the whole server.
func (w *connWriter) send(f Frame) bool {
	select {
	case <-w.done:
		return false
	default:
	}
	select {
	case w.queue <- f:
		return true
	default:
		w.mu.Lock()
		w.dropped++
		w.mu.Unlock()
		// A full queue means the client is hopelessly behind. Closing is
		// kinder than silently losing records: the client reconnects and
		// resumes from its committed cursor with nothing missing.
		w.close()
		return false
	}
}

func (w *connWriter) run() {
	for {
		select {
		case <-w.done:
			return
		case f := <-w.queue:
			w.conn.SetWriteDeadline(time.Now().Add(w.timeout))
			if err := w.conn.WriteJSON(f); err != nil {
				w.close()
				return
			}
		}
	}
}

func (w *connWriter) ping() error {
	w.conn.SetWriteDeadline(time.Now().Add(w.timeout))
	return w.conn.WriteMessage(websocket.PingMessage, nil)
}

// shutdown waits, bounded, for queued frames to be written, then closes.
func (w *connWriter) shutdown(wait time.Duration) {
	deadline := time.Now().Add(wait)
	for time.Now().Before(deadline) {
		select {
		case <-w.done:
			return // already closed by an error on the write path
		default:
		}
		if len(w.queue) == 0 {
			// The drain goroutine may still be mid-write on the last frame.
			time.Sleep(5 * time.Millisecond)
			break
		}
		time.Sleep(2 * time.Millisecond)
	}
	w.close()
}

func (w *connWriter) close() {
	w.once.Do(func() {
		close(w.done)
		w.conn.Close()
	})
}

func (w *connWriter) dropCount() uint64 {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.dropped
}

// ServeHTTP upgrades an HTTP request to a gateway session.
func (g *Gateway) ServeHTTP(rw http.ResponseWriter, r *http.Request) {
	g.lifecycle.RLock()
	if g.shuttingDown {
		g.lifecycle.RUnlock()
		http.Error(rw, "gateway shutting down", http.StatusServiceUnavailable)
		return
	}
	g.lifecycle.RUnlock()

	// Authenticate before upgrading. Completing a WebSocket handshake for a
	// request that is about to be rejected wastes a round trip and gives an
	// unauthenticated peer a socket, however briefly.
	principal, err := g.authenticate(r)
	if err != nil {
		g.bump(func(s *Stats) { s.AuthFailures++ })
		http.Error(rw, "unauthorized", http.StatusUnauthorized)
		return
	}

	conn, err := g.upgrader.Upgrade(rw, r, nil)
	if err != nil {
		return // Upgrade has already written a response
	}

	// Re-check under the lock and register in one step: a shutdown starting
	// between the check above and the Add would otherwise race Wait.
	g.lifecycle.RLock()
	if g.shuttingDown {
		g.lifecycle.RUnlock()
		conn.Close()
		return
	}
	g.wg.Add(1)
	g.lifecycle.RUnlock()

	go func() {
		defer g.wg.Done()
		g.serveConn(conn, principal, r)
	}()
}

// authenticate resolves a principal from the request.
//
// Three sources are accepted, in order: the Authorization header (correct for
// native clients), the `token` query parameter (the only option available to a
// browser's WebSocket API, which cannot set headers), and the shared API key
// for trusted backends.
func (g *Gateway) authenticate(r *http.Request) (*identity.Principal, error) {
	raw := ""
	if h := r.Header.Get("Authorization"); h != "" {
		if v, ok := strings.CutPrefix(h, "Bearer "); ok {
			raw = v
		} else {
			raw = h
		}
	}
	if raw == "" {
		raw = r.URL.Query().Get("token")
	}
	if raw == "" {
		return nil, identity.ErrNoPrincipal
	}

	if g.apiKey != "" && raw == g.apiKey {
		return &identity.Principal{Anonymous: true}, nil
	}
	if g.verifier == nil {
		return nil, identity.ErrBadSignature
	}
	return g.verifier.Verify(raw)
}

func (g *Gateway) serveConn(conn *websocket.Conn, principal *identity.Principal, r *http.Request) {
	g.bump(func(s *Stats) { s.Connections++ })

	conn.SetReadLimit(g.cfg.ReadLimit)
	conn.SetReadDeadline(time.Now().Add(g.cfg.PongTimeout))
	conn.SetPongHandler(func(string) error {
		return conn.SetReadDeadline(time.Now().Add(g.cfg.PongTimeout))
	})

	writer := newConnWriter(conn, g.cfg.SendBuffer, g.cfg.WriteTimeout)
	go writer.run()
	// Drain before closing: a rejection path enqueues an error frame and then
	// returns immediately, and closing the socket first would leave the client
	// with an unexplained disconnect instead of a reason.
	defer writer.shutdown(2 * time.Second)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	c := &conning{
		gw:        g,
		conn:      conn,
		writer:    writer,
		principal: principal,
		ctx:       ctx,
		cancel:    cancel,
		subs:      make(map[string]context.CancelFunc),
		remote:    r.RemoteAddr,
	}
	defer c.teardown()

	// The first frame must be hello. Refusing anything else keeps the session
	// lifecycle unambiguous: there is exactly one point at which a session is
	// created or resumed.
	var first Frame
	if err := conn.ReadJSON(&first); err != nil {
		return
	}
	if first.Op != OpHello {
		writer.send(errFrame(first.ID, CodeBadRequest, "first frame must be hello", false))
		return
	}
	if !c.handleHello(first) {
		return
	}

	go c.pingLoop(g.cfg.PingInterval)

	for {
		var f Frame
		if err := conn.ReadJSON(&f); err != nil {
			if websocket.IsUnexpectedCloseError(err, websocket.CloseNormalClosure, websocket.CloseGoingAway) {
				log.Printf("[gateway] %s read error: %v", c.principal, err)
			}
			return
		}
		g.bump(func(s *Stats) { s.FramesIn++ })

		// Re-check expiry on every frame. A connection can outlive its token by
		// hours otherwise, which would make token lifetimes meaningless for
		// exactly the long-lived connections that need them most.
		if c.principal.Expired(time.Now()) {
			writer.send(errFrame(f.ID, CodeUnauthenticated, "token expired, reconnect with a fresh token", false))
			return
		}
		if !c.dispatch(f) {
			return
		}
	}
}

// conning is one live connection's state.
type conning struct {
	gw        *Gateway
	conn      *websocket.Conn
	writer    *connWriter
	principal *identity.Principal
	session   *Session

	ctx    context.Context
	cancel context.CancelFunc
	remote string

	mu       sync.Mutex
	subs     map[string]context.CancelFunc
	sigSubs  []*ephemeral.Subscription
	presStop func()
	bound    bool
	connID   string
}

func (c *conning) send(f Frame) bool {
	c.gw.bump(func(s *Stats) { s.FramesOut++ })
	return c.writer.send(f)
}

func (c *conning) handleHello(f Frame) bool {
	if f.Version != 0 && f.Version != ProtocolVersion {
		c.send(errFrame(f.ID, CodeUnsupported,
			fmt.Sprintf("protocol version %d not supported (server speaks %d)", f.Version, ProtocolVersion), false))
		return false
	}

	deviceID := f.DeviceID
	if deviceID == "" {
		deviceID = c.principal.DeviceID
	}
	if deviceID == "" {
		c.send(errFrame(f.ID, CodeBadRequest, "device_id is required", false))
		return false
	}

	var resumeToken string
	resumed := false

	if f.Resume != "" {
		// Resume token format is "<sessionID>.<secret>".
		sessionID, secret, ok := strings.Cut(f.Resume, ".")
		if ok {
			if sess, good := c.gw.sessions.Resume(sessionID, secret, c.principal); good {
				c.session = sess
				resumed = true
				resumeToken = f.Resume
				c.gw.bump(func(s *Stats) { s.Resumed++ })
			}
		}
	}

	if c.session == nil {
		sess, secret := c.gw.sessions.Create(c.principal, deviceID, f.UserAgent)
		c.session = sess
		resumeToken = sess.ID + "." + secret
	}

	c.session.mu.Lock()
	c.session.conn = c.writer
	c.session.DeviceID = deviceID
	c.session.mu.Unlock()

	c.connID = randomToken(8)

	if c.gw.presence != nil && !c.principal.Anonymous {
		sess := presence.Session{
			UserID: c.principal.UserID, DeviceID: deviceID,
			NodeID: c.gw.cfg.NodeID, Region: c.gw.cfg.Region,
			ConnID: c.connID, Tenant: c.principal.Tenant,
			State: presence.StateOnline, UserAgent: f.UserAgent,
		}
		// Publish to whichever node owns this user's shard, so the rest of the
		// cluster can see the session. NodeID stays this node: it holds the
		// socket, and that is where deliveries must be routed regardless of
		// which node records the fact.
		//
		// Best-effort on purpose. A failure here means other nodes cannot see
		// the user for a while; refusing the connection over it would turn a
		// degraded lookup into a failed login.
		if c.gw.directory != nil {
			if err := c.gw.directory.Report(context.Background(), sess); err != nil {
				log.Printf("[gateway] publish presence for %s: %v", c.principal.UserID, err)
			}
		}
		if _, err := c.gw.presence.Bind(sess); err != nil {
			c.send(errFrame(f.ID, CodeBadRequest, err.Error(), false))
			return false
		}
		c.mu.Lock()
		c.bound = true
		c.mu.Unlock()
	}

	c.send(Frame{
		Op: OpWelcome, ID: f.ID, Version: ProtocolVersion,
		Session: c.session.ID, Token: resumeToken, Resumed: resumed,
	})

	// Restore streaming for a resumed session so the client does not have to
	// re-issue every subscribe — and, more importantly, so no records appended
	// during the outage are missed.
	if resumed {
		for _, sub := range c.session.snapshotSubscriptions() {
			c.startSubscription(sub.Topic, sub.Partition, sub.FromSeq, "")
		}
		if users := c.session.watchedSnapshot(); len(users) > 0 {
			c.startPresenceWatch(users)
		}
	}
	return true
}

func (c *conning) dispatch(f Frame) bool {
	switch f.Op {
	case OpPing:
		if c.gw.presence != nil && c.session != nil {
			c.gw.presence.Touch(c.principal.UserID, c.session.DeviceID)
		}
		c.send(Frame{Op: OpPong, ID: f.ID})
	case OpSubscribe:
		c.handleSubscribe(f)
	case OpUnsubscribe:
		c.handleUnsubscribe(f)
	case OpHistory:
		c.handleHistory(f)
	case OpSend:
		c.handleSend(f)
	case OpCommit:
		c.handleCommit(f)
	case OpTyping:
		c.handleTyping(f)
	case OpPresence:
		c.handlePresence(f)
	case OpWatchPresence:
		c.handleWatchPresence(f)
	case OpHello:
		c.send(errFrame(f.ID, CodeConflict, "session already established", false))
	default:
		c.send(errFrame(f.ID, CodeBadRequest, "unknown op: "+string(f.Op), false))
	}
	return true
}

// authorize checks the policy and reports the failure to the client.
func (c *conning) authorize(reqID, topic string, action identity.Action) bool {
	if err := c.gw.policy.Authorize(c.ctx, c.principal, topic, action); err != nil {
		c.gw.bump(func(s *Stats) { s.Forbidden++ })
		c.send(errFrame(reqID, CodeForbidden, "access denied", false))
		return false
	}
	return true
}

func (c *conning) handleSubscribe(f Frame) {
	if f.Topic == "" {
		c.send(errFrame(f.ID, CodeBadRequest, "topic is required", false))
		return
	}
	if !c.authorize(f.ID, f.Topic, identity.ActionRead) {
		return
	}

	c.mu.Lock()
	n := len(c.subs)
	c.mu.Unlock()
	if n >= c.gw.cfg.MaxSubscriptions {
		c.send(errFrame(f.ID, CodeRateLimited, "subscription limit reached", false))
		return
	}

	topic, err := c.gw.log.Topic(f.Topic)
	if err != nil {
		// Subscribing before the first message is normal — a new conversation
		// has no log yet. Create it so the client can attach and wait.
		if topic, err = c.gw.log.GetOrCreateTopic(f.Topic); err != nil {
			c.send(errFrame(f.ID, CodeInternal, err.Error(), true))
			return
		}
	}

	// Without an explicit partition, resolve the one owning the conversation —
	// the client should not need to know the partition map.
	partition := int32(0)
	if f.Partition != nil {
		partition = *f.Partition
	} else if _, convID, ok := fanout.ConversationFromTopic(f.Topic); ok {
		partition = topic.PartitionForKey([]byte(convID))
	} else if c.principal.UserID != "" {
		partition = topic.PartitionForKey([]byte(c.principal.UserID))
	}

	p, err := topic.Partition(partition)
	if err != nil {
		c.send(errFrame(f.ID, CodeNotFound, err.Error(), false))
		return
	}

	// Resolve the starting position: explicit, else the device's committed
	// cursor, else the head. Defaulting to the head rather than the beginning
	// matters — a client attaching to a busy channel should not be flooded
	// with its entire history.
	from := f.FromSeq
	if from == 0 {
		key := c.cursorKey(f.Topic, partition)
		from = c.gw.cursors.PositionOr(key, p.NextSeq())
	}
	if from < p.FirstSeq() {
		c.send(Frame{
			Op: OpGap, ID: f.ID, Topic: f.Topic, Partition: &partition,
			FirstSeq: p.FirstSeq(), NextSeq: p.NextSeq(),
			Error: &ErrorFrame{Code: CodeGap, Message: "requested position was removed by retention"},
		})
		from = p.FirstSeq()
	}

	c.session.addSubscription(f.Topic, partition, from)
	c.startSubscription(f.Topic, partition, from, f.ID)
}

func (c *conning) startSubscription(topicName string, partition int32, from uint64, reqID string) {
	key := subKey(topicName, partition)

	c.mu.Lock()
	if cancel, exists := c.subs[key]; exists {
		cancel() // replace an existing subscription on the same partition
	}
	ctx, cancel := context.WithCancel(c.ctx)
	c.subs[key] = cancel
	c.mu.Unlock()

	topic, err := c.gw.log.Topic(topicName)
	if err != nil {
		c.send(errFrame(reqID, CodeNotFound, err.Error(), false))
		return
	}
	p, err := topic.Partition(partition)
	if err != nil {
		c.send(errFrame(reqID, CodeNotFound, err.Error(), false))
		return
	}

	if reqID != "" {
		c.send(Frame{
			Op: OpAck, ID: reqID, Topic: topicName, Partition: &partition,
			FromSeq: from, FirstSeq: p.FirstSeq(), NextSeq: p.NextSeq(),
		})
	}

	go func() {
		err := p.Tail(ctx, from, 64, func(recs []*stream.Record) error {
			frames := make([]RecordFrame, 0, len(recs))
			for _, r := range recs {
				frames = append(frames, RecordFrame{
					Topic: topicName, Partition: partition, Seq: r.Seq,
					Timestamp: r.Timestamp, Key: string(r.Key),
					Headers: r.Headers, Payload: r.Payload,
				})
			}
			if !c.send(Frame{Op: OpRecord, Topic: topicName, Partition: &partition, Records: frames}) {
				return errClientGone
			}
			c.gw.bump(func(s *Stats) { s.RecordsOut += uint64(len(frames)) })
			c.session.advance(topicName, partition, recs[len(recs)-1].Seq+1)
			return nil
		})
		if err != nil && !errors.Is(err, context.Canceled) && !errors.Is(err, errClientGone) {
			c.send(errFrame("", CodeInternal, err.Error(), true))
		}
	}()
}

var errClientGone = errors.New("gateway: client disconnected")

func (c *conning) handleUnsubscribe(f Frame) {
	partition := int32(0)
	if f.Partition != nil {
		partition = *f.Partition
	}
	key := subKey(f.Topic, partition)

	c.mu.Lock()
	if cancel, ok := c.subs[key]; ok {
		cancel()
		delete(c.subs, key)
	}
	c.mu.Unlock()

	c.session.removeSubscription(f.Topic, partition)
	c.send(ackFrame(f.ID))
}

func (c *conning) handleHistory(f Frame) {
	if f.Topic == "" {
		c.send(errFrame(f.ID, CodeBadRequest, "topic is required", false))
		return
	}
	if !c.authorize(f.ID, f.Topic, identity.ActionRead) {
		return
	}

	limit := f.Limit
	if limit <= 0 || limit > c.gw.cfg.HistoryLimit {
		limit = c.gw.cfg.HistoryLimit
	}

	topic, err := c.gw.log.Topic(f.Topic)
	if err != nil {
		c.send(errFrame(f.ID, CodeNotFound, err.Error(), false))
		return
	}

	partition := int32(0)
	if f.Partition != nil {
		partition = *f.Partition
	} else if _, convID, ok := fanout.ConversationFromTopic(f.Topic); ok {
		partition = topic.PartitionForKey([]byte(convID))
	} else if c.principal.UserID != "" {
		partition = topic.PartitionForKey([]byte(c.principal.UserID))
	}

	p, err := topic.Partition(partition)
	if err != nil {
		c.send(errFrame(f.ID, CodeNotFound, err.Error(), false))
		return
	}

	from := f.FromSeq
	if from == 0 {
		from = p.FirstSeq()
	}

	recs, err := p.ReadFrom(from, limit)
	if err != nil {
		if errors.Is(err, stream.ErrSeqTruncated) {
			c.send(Frame{
				Op: OpGap, ID: f.ID, Topic: f.Topic, Partition: &partition,
				FirstSeq: p.FirstSeq(), NextSeq: p.NextSeq(),
				Error: &ErrorFrame{Code: CodeGap, Message: "requested history was removed by retention"},
			})
			return
		}
		c.send(errFrame(f.ID, CodeInternal, err.Error(), true))
		return
	}

	frames := make([]RecordFrame, 0, len(recs))
	for _, r := range recs {
		frames = append(frames, RecordFrame{
			Topic: f.Topic, Partition: partition, Seq: r.Seq,
			Timestamp: r.Timestamp, Key: string(r.Key),
			Headers: r.Headers, Payload: r.Payload,
		})
	}
	c.send(Frame{
		Op: OpRecord, ID: f.ID, Topic: f.Topic, Partition: &partition,
		Records: frames, FirstSeq: p.FirstSeq(), NextSeq: p.NextSeq(),
	})
}

func (c *conning) handleSend(f Frame) {
	if c.gw.deliver == nil {
		c.send(errFrame(f.ID, CodeUnsupported, "conversation delivery is not configured", false))
		return
	}
	if f.Conversation == "" {
		c.send(errFrame(f.ID, CodeBadRequest, "conversation is required", false))
		return
	}

	kind := fanout.KindDirect
	if f.Kind == string(fanout.KindGroup) {
		kind = fanout.KindGroup
	}
	topic := fanout.ConversationTopic(kind, f.Conversation)
	if !c.authorize(f.ID, topic, identity.ActionWrite) {
		return
	}

	res, err := c.gw.deliver.Send(c.ctx, fanout.SendRequest{
		Tenant: c.principal.Tenant, Kind: kind,
		ConversationID: f.Conversation, SenderID: c.principal.UserID,
		ClientMsgID: f.ClientMsgID, Payload: f.Payload, Headers: f.Headers,
	})
	if err != nil {
		switch {
		case errors.Is(err, fanout.ErrInFlight):
			c.send(errFrame(f.ID, CodeConflict, "duplicate send in flight, retry shortly", true))
		case errors.Is(err, fanout.ErrNoMembers):
			c.send(errFrame(f.ID, CodeNotFound, "conversation has no members", false))
		case res != nil:
			// Stored, but fan-out was incomplete. The message is durable, so
			// report success and log the index gap rather than telling the
			// client their message failed.
			log.Printf("[gateway] partial fan-out for %s: %v", res.MessageID, err)
		default:
			c.send(errFrame(f.ID, CodeInternal, err.Error(), true))
			return
		}
		if res == nil {
			return
		}
	}

	c.send(Frame{Op: OpSent, ID: f.ID, Sent: &SentFrame{
		MessageID: res.MessageID, ClientMsgID: f.ClientMsgID,
		Topic: res.Topic, Partition: res.Partition, Seq: res.Seq,
		Timestamp: res.Timestamp, Duplicate: res.Duplicate,
	}})
}

func (c *conning) handleCommit(f Frame) {
	if f.Topic == "" || f.Seq == 0 {
		c.send(errFrame(f.ID, CodeBadRequest, "topic and seq are required", false))
		return
	}
	if !c.authorize(f.ID, f.Topic, identity.ActionRead) {
		return
	}

	partition := int32(0)
	if f.Partition != nil {
		partition = *f.Partition
	}
	if err := c.gw.cursors.Commit(c.cursorKey(f.Topic, partition), f.Seq); err != nil {
		c.send(errFrame(f.ID, CodeInternal, err.Error(), true))
		return
	}
	c.send(ackFrame(f.ID))
}

// cursorKey scopes a cursor to the user (group) and device (member), which is
// what gives each of a user's devices an independent read position.
func (c *conning) cursorKey(topic string, partition int32) stream.CursorKey {
	device := c.principal.DeviceID
	if c.session != nil && c.session.DeviceID != "" {
		device = c.session.DeviceID
	}
	return stream.CursorKey{
		Topic: topic, Partition: partition,
		Group: "user:" + c.principal.UserID, Member: device,
	}
}

func (c *conning) handleTyping(f Frame) {
	if c.gw.signals == nil {
		c.send(errFrame(f.ID, CodeUnsupported, "ephemeral signals are not configured", false))
		return
	}
	if f.Conversation == "" {
		c.send(errFrame(f.ID, CodeBadRequest, "conversation is required", false))
		return
	}

	topic := ephemeral.TypingTopic(f.Conversation)
	if !c.authorize(f.ID, topic, identity.ActionWrite) {
		return
	}

	// Subscribe on first use so the client sees other people's typing without
	// a separate subscribe round trip.
	c.ensureSignalSub(topic)

	typing := true
	if f.Typing != nil {
		typing = *f.Typing
	}
	if err := c.gw.signals.PublishTyping(f.Conversation, c.principal.UserID, typing); err != nil {
		if errors.Is(err, ephemeral.ErrRateLimited) {
			c.send(errFrame(f.ID, CodeRateLimited, "typing signals are rate limited", true))
			return
		}
		c.send(errFrame(f.ID, CodeInternal, err.Error(), true))
		return
	}
	c.send(ackFrame(f.ID))
}

func (c *conning) ensureSignalSub(topic string) {
	c.mu.Lock()
	for _, s := range c.sigSubs {
		if s.Topic == topic {
			c.mu.Unlock()
			return
		}
	}
	c.mu.Unlock()

	sub, err := c.gw.signals.SubscribeAs(topic, c.principal.UserID+":"+c.connID, c.principal.UserID)
	if err != nil {
		return
	}

	c.mu.Lock()
	c.sigSubs = append(c.sigSubs, sub)
	c.mu.Unlock()

	go func() {
		for sig := range sub.C {
			c.send(Frame{Op: OpSignal, Signal: &SignalFrame{
				Topic: sig.Topic, Sender: sig.Sender, Kind: sig.Kind,
				Headers: sig.Headers, Payload: sig.Payload, At: sig.At,
			}})
		}
	}()
}

func (c *conning) handlePresence(f Frame) {
	if c.gw.presence == nil || c.session == nil {
		c.send(ackFrame(f.ID))
		return
	}
	state := presence.State(f.State)
	switch state {
	case presence.StateOnline, presence.StateAway, presence.StateOffline:
	default:
		c.send(errFrame(f.ID, CodeBadRequest, "state must be online, away or offline", false))
		return
	}
	c.gw.presence.SetState(c.principal.UserID, c.session.DeviceID, state)
	c.send(ackFrame(f.ID))
}

func (c *conning) handleWatchPresence(f Frame) {
	if c.gw.presence == nil {
		c.send(errFrame(f.ID, CodeUnsupported, "presence is not configured", false))
		return
	}
	// Watching someone's presence is a read of their presence topic, so it
	// goes through the same policy as any other read.
	for _, u := range f.Users {
		if !c.authorize(f.ID, presence.PresenceTopic(u), identity.ActionRead) {
			return
		}
	}

	c.session.setWatchedUsers(f.Users)
	c.startPresenceWatch(f.Users)
	c.send(ackFrame(f.ID))
}

func (c *conning) startPresenceWatch(users []string) {
	c.mu.Lock()
	if c.presStop != nil {
		c.presStop()
		c.presStop = nil
	}
	c.mu.Unlock()

	if len(users) == 0 {
		return
	}

	ch, cancel := c.gw.presence.WatchUsers(users)
	c.mu.Lock()
	c.presStop = cancel
	c.mu.Unlock()

	go func() {
		for ev := range ch {
			c.send(Frame{Op: OpPresenceEvent, Presence: &PresenceFrame{
				UserID: ev.UserID, DeviceID: ev.DeviceID,
				State: string(ev.State), Online: ev.UserOnline,
				At: ev.At.UnixNano(),
			}})
		}
	}()
}

func (c *conning) pingLoop(interval time.Duration) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-c.ctx.Done():
			return
		case <-ticker.C:
			if err := c.writer.ping(); err != nil {
				c.writer.close()
				return
			}
			// A live socket is also a live presence heartbeat; without this a
			// quiet-but-connected client would be swept as offline.
			if c.gw.presence != nil && c.session != nil {
				c.gw.presence.Touch(c.principal.UserID, c.session.DeviceID)
			}
		}
	}
}

// teardown releases everything the connection owns. The session itself is only
// detached, not dropped — that is what makes a resume possible.
func (c *conning) teardown() {
	c.cancel()

	c.mu.Lock()
	for _, cancel := range c.subs {
		cancel()
	}
	c.subs = nil
	for _, s := range c.sigSubs {
		s.Close()
	}
	c.sigSubs = nil
	if c.presStop != nil {
		c.presStop()
		c.presStop = nil
	}
	bound := c.bound
	c.mu.Unlock()

	if bound && c.gw.presence != nil && c.session != nil {
		c.gw.presence.Unbind(c.principal.UserID, c.session.DeviceID, c.connID)
	}
	if drops := c.writer.dropCount(); drops > 0 {
		c.gw.bump(func(s *Stats) { s.SlowClientDrops += drops })
	}
	if c.session != nil {
		c.gw.sessions.Detach(c.session)
	}
	// The socket is not closed here. serveConn defers writer.shutdown, which
	// runs after this and drains any queued frame — typically the error
	// explaining why the connection is ending. Closing here would discard it.
}

func (g *Gateway) bump(f func(*Stats)) {
	g.mu.Lock()
	f(&g.stats)
	g.mu.Unlock()
}

// Stats returns a snapshot of gateway counters.
func (g *Gateway) Stats() Stats {
	g.mu.Lock()
	s := g.stats
	g.mu.Unlock()
	s.Sessions = g.sessions.Len()
	s.Attached = g.sessions.Attached()
	return s
}

// StatsJSON renders Stats for the admin API.
func (g *Gateway) StatsJSON() ([]byte, error) { return json.Marshal(g.Stats()) }

// Close stops accepting connections and waits for live ones to finish.
func (g *Gateway) Close() {
	g.once.Do(func() {
		g.lifecycle.Lock()
		g.shuttingDown = true
		g.lifecycle.Unlock()

		g.sessions.Close()
		g.wg.Wait()
	})
}
