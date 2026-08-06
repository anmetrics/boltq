// Package fanout turns "send a message to a conversation" into the right set
// of log appends.
//
// The delivery model is pull-based, not push-based. A connected client tails
// the partitions it cares about; sending a message is therefore just an append,
// and every reader wakes up on it. There is no separate "deliver" step that
// could succeed for the log and fail for a socket.
//
// What remains is deciding *which* logs to append to, and that depends on group
// size:
//
//   - Small conversations use fan-out on write. The message goes into the
//     conversation log, and a small pointer record goes into each member's
//     inbox. The inbox gives a client one stream to tail for "anything new
//     anywhere", which is what makes a cold app start cheap.
//
//   - Large conversations use fan-out on read. Only the conversation log is
//     written; members tail it directly. Writing 50,000 pointer records because
//     one person said "hi" in a big channel is how a broadcast turns into an
//     outage, so above a threshold the write is simply not done.
//
// The conversation log is written either way, so it is always the ordering
// authority and always the complete history.
package fanout

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/boltq/boltq/internal/dedup"
	"github.com/boltq/boltq/internal/presence"
	"github.com/boltq/boltq/internal/stream"
)

var (
	// ErrInFlight means an identical submission is still being processed. The
	// client should retry after a short delay rather than assume failure.
	ErrInFlight = errors.New("fanout: duplicate submission in flight")
	// ErrNoMembers means the conversation has no members to deliver to.
	ErrNoMembers = errors.New("fanout: conversation has no members")
	// ErrEmptyConversation means no conversation ID was supplied.
	ErrEmptyConversation = errors.New("fanout: conversation id is required")
)

// MemberLister enumerates a conversation's participants. The application owns
// the social graph; this is how fanout asks about it.
type MemberLister interface {
	Members(ctx context.Context, tenant, conversationID string) ([]string, error)
}

// MemberListerFunc adapts a function to MemberLister.
type MemberListerFunc func(ctx context.Context, tenant, conversationID string) ([]string, error)

// Members implements MemberLister.
func (f MemberListerFunc) Members(ctx context.Context, tenant, conversationID string) ([]string, error) {
	return f(ctx, tenant, conversationID)
}

// Kind distinguishes conversation shapes, which map to different topics.
type Kind string

const (
	// KindDirect is a 1:1 conversation.
	KindDirect Kind = "direct"
	// KindGroup is a many-party conversation.
	KindGroup Kind = "group"
)

// Topic naming. These constants are the contract shared with the ACL rules in
// identity.ChatPolicyRules and with every client SDK; changing one means
// changing all three.
const (
	TopicDirectPrefix = "chat.direct."
	TopicGroupPrefix  = "chat.group."
	TopicInboxPrefix  = "chat.inbox."
)

// ConversationTopic returns the log topic for a conversation.
func ConversationTopic(kind Kind, conversationID string) string {
	if kind == KindGroup {
		return TopicGroupPrefix + conversationID
	}
	return TopicDirectPrefix + conversationID
}

// InboxTopic returns a user's personal inbox topic.
func InboxTopic(userID string) string { return TopicInboxPrefix + userID }

// ConversationFromTopic extracts the conversation ID from a conversation topic.
func ConversationFromTopic(topic string) (Kind, string, bool) {
	if rest, ok := strings.CutPrefix(topic, TopicGroupPrefix); ok && rest != "" {
		return KindGroup, rest, true
	}
	if rest, ok := strings.CutPrefix(topic, TopicDirectPrefix); ok && rest != "" {
		return KindDirect, rest, true
	}
	return "", "", false
}

// Headers written onto fan-out records. Clients rely on these names.
const (
	HeaderSender      = "sender"
	HeaderClientMsgID = "client_msg_id"
	HeaderKind        = "kind"
	HeaderConvID      = "conversation"
	HeaderConvSeq     = "conv_seq"
	HeaderConvPart    = "conv_partition"
	HeaderMessageID   = "message_id"
	HeaderPointer     = "pointer"
)

// Config tunes the deliverer.
type Config struct {
	// FanoutOnWriteLimit is the largest conversation that still receives
	// per-member inbox pointers. Above it, members tail the conversation log.
	//
	// The default of 256 is chosen so the worst-case write amplification of a
	// single send stays in the low hundreds — comfortably inside one batch —
	// while covering essentially every direct chat and most group chats in a
	// dating or messaging app.
	FanoutOnWriteLimit int

	// InboxPartitions is the partition count for inbox topics. Each user's
	// inbox is its own topic, so one partition is enough and keeps the file
	// handle count proportional to users rather than users times partitions.
	InboxPartitions int32

	// ConversationPartitions is the partition count for conversation topics.
	ConversationPartitions int32

	// Tenant is the default tenant when a request does not name one.
	Tenant string
}

func (c *Config) applyDefaults() {
	if c.FanoutOnWriteLimit <= 0 {
		c.FanoutOnWriteLimit = 256
	}
	if c.InboxPartitions <= 0 {
		c.InboxPartitions = 1
	}
	if c.ConversationPartitions <= 0 {
		c.ConversationPartitions = 16
	}
}

// Deliverer performs conversation sends.
type Deliverer struct {
	log      *stream.Log
	members  MemberLister
	presence presence.Lookup
	dedupe   *dedup.Table
	cfg      Config
}

// Options assembles a Deliverer's dependencies. presence and dedupe are
// optional: without presence, every recipient is treated as offline (so the
// outbox does the work); without dedupe, retries are not collapsed.
type Options struct {
	Log     *stream.Log
	Members MemberLister
	// Presence answers whether a recipient is online. A presence.Directory
	// routes that question to the node owning each user's shard; a
	// presence.LocalLookup answers from this node alone.
	Presence presence.Lookup
	Dedup    *dedup.Table
	Config   Config
}

// New builds a Deliverer.
func New(opts Options) (*Deliverer, error) {
	if opts.Log == nil {
		return nil, errors.New("fanout: a stream log is required")
	}
	if opts.Members == nil {
		return nil, errors.New("fanout: a member lister is required")
	}
	opts.Config.applyDefaults()
	return &Deliverer{
		log:      opts.Log,
		members:  opts.Members,
		presence: opts.Presence,
		dedupe:   opts.Dedup,
		cfg:      opts.Config,
	}, nil
}

// SendRequest is one message submission.
type SendRequest struct {
	Tenant         string
	Kind           Kind
	ConversationID string
	SenderID       string
	// ClientMsgID is the client's idempotency key. Strongly recommended:
	// without it a retry over a flaky connection duplicates the message.
	ClientMsgID string
	// Payload is opaque. For an end-to-end encrypted app it is ciphertext the
	// server cannot and need not read.
	Payload []byte
	Headers map[string]string
}

// SendResult reports where the message landed.
type SendResult struct {
	MessageID      string    `json:"message_id"`
	ConversationID string    `json:"conversation_id"`
	Topic          string    `json:"topic"`
	Partition      int32     `json:"partition"`
	Seq            uint64    `json:"seq"`
	Timestamp      int64     `json:"timestamp"`
	Recipients     int       `json:"recipients"`
	InboxWrites    int       `json:"inbox_writes"`
	OfflineUsers   []string  `json:"offline_users,omitempty"`
	Strategy       Strategy  `json:"strategy"`
	Duplicate      bool      `json:"duplicate"`
	At             time.Time `json:"-"`
}

// Strategy names the fan-out mode used for a send.
type Strategy string

const (
	// StrategyOnWrite wrote a pointer into every member's inbox.
	StrategyOnWrite Strategy = "fanout_on_write"
	// StrategyOnRead wrote only the conversation log.
	StrategyOnRead Strategy = "fanout_on_read"
)

// Send appends the message and performs fan-out.
//
// Ordering: the conversation append happens first and alone determines the
// message's position in history. Inbox pointers are written afterwards and are
// only an index — if the process died between the two, the message still
// exists and is still visible to anyone reading the conversation. The reverse
// order would risk an inbox pointing at a message that does not exist.
func (d *Deliverer) Send(ctx context.Context, req SendRequest) (*SendResult, error) {
	if req.ConversationID == "" {
		return nil, ErrEmptyConversation
	}
	if req.Kind == "" {
		req.Kind = KindDirect
	}
	if req.Tenant == "" {
		req.Tenant = d.cfg.Tenant
	}

	var dk dedup.Key
	var claimed bool
	if d.dedupe != nil && req.ClientMsgID != "" {
		dk = dedup.Key{Sender: req.SenderID, ClientMsgID: req.ClientMsgID}
		prev, ok, c := d.dedupe.Claim(dk)
		switch {
		case ok:
			// An exact replay of a completed send: hand back the original
			// coordinates so the client's local echo resolves identically.
			return &SendResult{
				MessageID: prev.MessageID, ConversationID: req.ConversationID,
				Topic: prev.Topic, Partition: prev.Partition, Seq: prev.Seq,
				Timestamp: prev.Timestamp, Duplicate: true,
			}, nil
		case !c:
			return nil, ErrInFlight
		}
		claimed = true
	}

	// From here on, any failure must release the claim, or the client's retry
	// would be deduplicated against a message that was never written.
	success := false
	if claimed {
		defer func() {
			if !success {
				d.dedupe.Abandon(dk)
			}
		}()
	}

	members, err := d.members.Members(ctx, req.Tenant, req.ConversationID)
	if err != nil {
		return nil, fmt.Errorf("list members: %w", err)
	}
	if len(members) == 0 {
		return nil, ErrNoMembers
	}

	messageID := newMessageID()
	convTopic := ConversationTopic(req.Kind, req.ConversationID)

	if _, err := d.ensureTopic(convTopic, d.cfg.ConversationPartitions); err != nil {
		return nil, err
	}

	headers := make(map[string]string, len(req.Headers)+5)
	for k, v := range req.Headers {
		headers[k] = v
	}
	headers[HeaderMessageID] = messageID
	headers[HeaderSender] = req.SenderID
	headers[HeaderKind] = string(req.Kind)
	headers[HeaderConvID] = req.ConversationID
	if req.ClientMsgID != "" {
		headers[HeaderClientMsgID] = req.ClientMsgID
	}

	// The conversation ID is the partition key: every message in a conversation
	// lands in the same partition and therefore in a single total order.
	//
	// AppendContext, not Append: when replication is configured with a quorum,
	// this is the call that waits for it. Using the plain Append here would
	// make an operator's min_in_sync setting silently ineffective — the send
	// would be acknowledged from the leader's page cache alone while the
	// configuration and startup logs both claimed replicated durability.
	res, err := d.log.AppendContext(ctx, convTopic, &stream.Record{
		Key:     []byte(req.ConversationID),
		Headers: headers,
		Payload: req.Payload,
	})
	if err != nil {
		return nil, fmt.Errorf("append conversation: %w", err)
	}

	out := &SendResult{
		MessageID:      messageID,
		ConversationID: req.ConversationID,
		Topic:          convTopic,
		Partition:      res.Partition,
		Seq:            res.Seq,
		Timestamp:      res.Timestamp,
		Recipients:     len(members),
		At:             time.Unix(0, res.Timestamp),
	}

	if len(members) <= d.cfg.FanoutOnWriteLimit {
		out.Strategy = StrategyOnWrite
		n, err := d.writeInboxPointers(members, req, out, headers)
		if err != nil {
			// The message is already durable in the conversation log. Failing
			// the whole send now would tell the client it was not delivered,
			// which is worse than a missing index entry — report and continue.
			out.InboxWrites = n
			out.Strategy = StrategyOnWrite
			d.completeDedup(claimed, dk, out)
			success = true
			return out, fmt.Errorf("message stored but inbox fan-out incomplete: %w", err)
		}
		out.InboxWrites = n
	} else {
		out.Strategy = StrategyOnRead
	}

	if d.presence != nil {
		recipients := make([]string, 0, len(members))
		for _, m := range members {
			if m != req.SenderID {
				recipients = append(recipients, m)
			}
		}
		out.OfflineUsers = d.presence.OfflineUsers(ctx, recipients)
	}

	d.completeDedup(claimed, dk, out)
	success = true
	return out, nil
}

func (d *Deliverer) completeDedup(claimed bool, dk dedup.Key, out *SendResult) {
	if !claimed {
		return
	}
	d.dedupe.Complete(dk, dedup.Result{
		MessageID: out.MessageID, Topic: out.Topic,
		Partition: out.Partition, Seq: out.Seq, Timestamp: out.Timestamp,
	})
}

// writeInboxPointers appends a pointer record to each member's inbox.
//
// The pointer carries no payload — only the coordinates of the real record.
// Copying the body into every inbox would multiply storage by the member count
// and, for an encrypted app, would be duplicating ciphertext for no gain.
func (d *Deliverer) writeInboxPointers(members []string, req SendRequest, res *SendResult, headers map[string]string) (int, error) {
	pointer := map[string]string{
		HeaderPointer:   "1",
		HeaderMessageID: res.MessageID,
		HeaderSender:    req.SenderID,
		HeaderKind:      string(req.Kind),
		HeaderConvID:    req.ConversationID,
		HeaderConvSeq:   fmt.Sprint(res.Seq),
		HeaderConvPart:  fmt.Sprint(res.Partition),
	}
	if v, ok := headers[HeaderClientMsgID]; ok {
		pointer[HeaderClientMsgID] = v
	}

	written := 0
	var firstErr error
	for _, member := range members {
		topic := InboxTopic(member)
		if _, err := d.ensureTopic(topic, d.cfg.InboxPartitions); err != nil {
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		// Copy the header map per member: records outlive this call inside
		// tailers, and a shared map would be mutated underneath them.
		h := make(map[string]string, len(pointer))
		for k, v := range pointer {
			h[k] = v
		}
		// Pointers deliberately do NOT wait for quorum, even when the
		// conversation append does. They are a derived index: if one is lost
		// the message still exists and is still readable from the conversation
		// log. Waiting on replication for each of N recipients would multiply
		// send latency by the group size to protect data that can be rebuilt.
		if _, err := d.log.Append(topic, &stream.Record{
			Key:     []byte(member),
			Headers: h,
		}); err != nil {
			if firstErr == nil {
				firstErr = fmt.Errorf("inbox %s: %w", member, err)
			}
			continue
		}
		written++
	}
	return written, firstErr
}

func (d *Deliverer) ensureTopic(name string, partitions int32) (*stream.Topic, error) {
	if t, err := d.log.Topic(name); err == nil {
		return t, nil
	}
	t, err := d.log.CreateTopic(name, stream.TopicConfig{Partitions: partitions})
	if err != nil {
		return nil, fmt.Errorf("create topic %s: %w", name, err)
	}
	return t, nil
}

// History returns a page of a conversation's messages.
func (d *Deliverer) History(kind Kind, conversationID string, fromSeq uint64, limit int) ([]*stream.Record, error) {
	topic := ConversationTopic(kind, conversationID)
	recs, _, err := d.log.ReadByKey(topic, []byte(conversationID), fromSeq, limit)
	return recs, err
}

// Backlog returns a user's unread inbox pointers starting at fromSeq.
func (d *Deliverer) Backlog(userID string, fromSeq uint64, limit int) ([]*stream.Record, error) {
	recs, _, err := d.log.ReadByKey(InboxTopic(userID), []byte(userID), fromSeq, limit)
	return recs, err
}

// Strategy reports which fan-out mode a conversation of the given size uses.
// Exposed so the admin UI and capacity planning can see the boundary.
func (d *Deliverer) StrategyFor(memberCount int) Strategy {
	if memberCount <= d.cfg.FanoutOnWriteLimit {
		return StrategyOnWrite
	}
	return StrategyOnRead
}
