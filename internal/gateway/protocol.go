// Package gateway exposes BoltQ's streaming model to end-user devices over
// WebSocket.
//
// The existing TCP protocol assumes a trusted, long-lived backend client on a
// stable network. A phone is neither: it cannot open a raw TCP socket from a
// browser, it changes IP address when it moves between wifi and cellular, and
// it is suspended by the operating system without warning. The gateway is the
// edge built for that client — WebSocket transport, per-user authorisation on
// every operation, and a resumable session so a thirty-second tunnel outage
// costs a reconnect rather than a resynchronisation of the user's entire
// message history.
package gateway

import "encoding/json"

// Protocol version. The client sends it on connect; a mismatch is refused
// rather than negotiated, because a client that does not understand the frame
// format cannot be told anything useful about why.
const ProtocolVersion = 1

// Op identifies a client request or server event.
type Op string

// Client -> server operations.
const (
	// OpHello is the first frame; it carries the protocol version and an
	// optional resume token.
	OpHello Op = "hello"
	// OpSubscribe starts streaming a topic partition from a position.
	OpSubscribe Op = "subscribe"
	// OpUnsubscribe stops streaming.
	OpUnsubscribe Op = "unsubscribe"
	// OpSend submits a message to a conversation.
	OpSend Op = "send"
	// OpCommit advances a device's cursor.
	OpCommit Op = "commit"
	// OpHistory fetches a page of past records.
	OpHistory Op = "history"
	// OpTyping publishes an ephemeral typing signal.
	OpTyping Op = "typing"
	// OpPresence reports a foreground/background transition.
	OpPresence Op = "presence"
	// OpWatchPresence subscribes to contacts' presence changes.
	OpWatchPresence Op = "watch_presence"
	// OpPing is an application-level keepalive, distinct from the WebSocket
	// control frame — it also refreshes the presence heartbeat.
	OpPing Op = "ping"
)

// Server -> client operations.
const (
	// OpWelcome answers OpHello with the session identity.
	OpWelcome Op = "welcome"
	// OpAck answers a request that produced no data.
	OpAck Op = "ack"
	// OpError answers a failed request.
	OpError Op = "error"
	// OpRecord delivers one or more log records.
	OpRecord Op = "record"
	// OpSent answers OpSend with the assigned coordinates.
	OpSent Op = "sent"
	// OpSignal delivers an ephemeral signal.
	OpSignal Op = "signal"
	// OpPresenceEvent delivers a watched user's presence change.
	OpPresenceEvent Op = "presence_event"
	// OpPong answers OpPing.
	OpPong Op = "pong"
	// OpGap warns that requested records were removed by retention, so the
	// client must resynchronise rather than assume it has a complete history.
	OpGap Op = "gap"
)

// Frame is the envelope for every message in both directions.
//
// A single envelope with an op discriminator — rather than distinct message
// types — keeps the client's read loop to one unmarshal, and lets the request
// ID correlate a response with its request without a second channel.
type Frame struct {
	Op Op `json:"op"`
	// ID correlates a response with its request. Server-initiated frames
	// (records, signals, presence events) carry no ID.
	ID string `json:"id,omitempty"`

	// Request fields.
	Version   int    `json:"version,omitempty"`
	Token     string `json:"token,omitempty"`
	Resume    string `json:"resume,omitempty"`
	DeviceID  string `json:"device_id,omitempty"`
	UserAgent string `json:"user_agent,omitempty"`

	Topic     string `json:"topic,omitempty"`
	Partition *int32 `json:"partition,omitempty"`
	FromSeq   uint64 `json:"from_seq,omitempty"`
	Seq       uint64 `json:"seq,omitempty"`
	Limit     int    `json:"limit,omitempty"`

	Conversation string   `json:"conversation,omitempty"`
	Kind         string   `json:"kind,omitempty"`
	ClientMsgID  string   `json:"client_msg_id,omitempty"`
	Typing       *bool    `json:"typing,omitempty"`
	State        string   `json:"state,omitempty"`
	Users        []string `json:"users,omitempty"`

	// Payload is opaque application bytes. It is base64-encoded by
	// encoding/json, which is correct for the ciphertext an end-to-end
	// encrypted app sends and costs 33% only on the wire, not at rest.
	Payload []byte            `json:"payload,omitempty"`
	Headers map[string]string `json:"headers,omitempty"`

	// Response fields.
	Session  string          `json:"session,omitempty"`
	Records  []RecordFrame   `json:"records,omitempty"`
	Signal   *SignalFrame    `json:"signal,omitempty"`
	Presence *PresenceFrame  `json:"presence,omitempty"`
	Sent     *SentFrame      `json:"sent,omitempty"`
	Error    *ErrorFrame     `json:"error,omitempty"`
	Resumed  bool            `json:"resumed,omitempty"`
	FirstSeq uint64          `json:"first_seq,omitempty"`
	NextSeq  uint64          `json:"next_seq,omitempty"`
	Extra    json.RawMessage `json:"extra,omitempty"`
}

// RecordFrame is one log record as delivered to a client.
type RecordFrame struct {
	Topic     string            `json:"topic"`
	Partition int32             `json:"partition"`
	Seq       uint64            `json:"seq"`
	Timestamp int64             `json:"timestamp"`
	Key       string            `json:"key,omitempty"`
	Headers   map[string]string `json:"headers,omitempty"`
	Payload   []byte            `json:"payload,omitempty"`
}

// SentFrame reports where a submitted message landed.
type SentFrame struct {
	MessageID   string `json:"message_id"`
	ClientMsgID string `json:"client_msg_id,omitempty"`
	Topic       string `json:"topic"`
	Partition   int32  `json:"partition"`
	Seq         uint64 `json:"seq"`
	Timestamp   int64  `json:"timestamp"`
	// Duplicate is true when this answers a retry of an already-stored
	// message. The coordinates are the original's, so a client that lost the
	// first response can reconcile its optimistic local echo.
	Duplicate bool `json:"duplicate,omitempty"`
}

// SignalFrame is an ephemeral signal.
type SignalFrame struct {
	Topic   string            `json:"topic"`
	Sender  string            `json:"sender"`
	Kind    string            `json:"kind,omitempty"`
	Headers map[string]string `json:"headers,omitempty"`
	Payload []byte            `json:"payload,omitempty"`
	At      int64             `json:"at"`
}

// PresenceFrame is a watched user's presence change.
type PresenceFrame struct {
	UserID   string `json:"user_id"`
	DeviceID string `json:"device_id,omitempty"`
	State    string `json:"state"`
	Online   bool   `json:"online"`
	At       int64  `json:"at"`
}

// ErrorFrame describes a failure.
type ErrorFrame struct {
	Code    Code   `json:"code"`
	Message string `json:"message"`
	// Retryable tells the client whether backing off and trying again could
	// succeed, so it does not retry a permanent authorisation failure forever
	// nor give up on a transient one.
	Retryable bool `json:"retryable,omitempty"`
}

// Code is a stable machine-readable error identifier. Clients branch on these;
// the human-readable message may change freely.
type Code string

const (
	CodeBadRequest      Code = "bad_request"
	CodeUnauthenticated Code = "unauthenticated"
	CodeForbidden       Code = "forbidden"
	CodeNotFound        Code = "not_found"
	CodeRateLimited     Code = "rate_limited"
	CodeConflict        Code = "conflict"
	CodeInternal        Code = "internal"
	CodeUnsupported     Code = "unsupported_version"
	CodeGap             Code = "history_gap"
)

func errFrame(id string, code Code, msg string, retryable bool) Frame {
	return Frame{
		Op: OpError, ID: id,
		Error: &ErrorFrame{Code: code, Message: msg, Retryable: retryable},
	}
}

func ackFrame(id string) Frame { return Frame{Op: OpAck, ID: id} }
