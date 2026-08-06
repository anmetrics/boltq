package cluster

import (
	"encoding/json"

	"github.com/boltq/boltq/pkg/protocol"
)

// CommandType identifies the type of Raft log entry command.
type CommandType uint8

const (
	CmdRaftPublish         CommandType = 1
	CmdRaftAck             CommandType = 2
	CmdRaftNack            CommandType = 3
	CmdRaftPublishTopic    CommandType = 4
	CmdRaftConsume         CommandType = 5
	CmdRaftPromote         CommandType = 6
	CmdRaftPurge           CommandType = 7
	CmdRaftPurgeDL         CommandType = 8
	CmdRaftSubscribe       CommandType = 9
	CmdRaftUnsubscribe     CommandType = 10
	CmdRaftExchangeDeclare CommandType = 11
	CmdRaftExchangeDelete  CommandType = 12
	CmdRaftBindQueue       CommandType = 13
	CmdRaftUnbindQueue     CommandType = 14
	CmdRaftPublishExchange CommandType = 15
)

// Metadata commands drive the stream control plane. They start at 32 rather
// than 16 so the queue-plane range above can grow without either side having to
// renumber — a renumbered command would be misread by any node still running
// the old binary during a rolling upgrade.
const (
	CmdMetaRegisterBroker   CommandType = 32
	CmdMetaUnregisterBroker CommandType = 33
	CmdMetaFenceBroker      CommandType = 34
	CmdMetaCreateTopic      CommandType = 35
	CmdMetaAssignLeader     CommandType = 36
	CmdMetaUpdateISR        CommandType = 37
	CmdMetaReassign         CommandType = 38
)

// RaftCommand is the payload serialized into each Raft log entry.
type RaftCommand struct {
	Type         CommandType       `json:"type"`
	Topic        string            `json:"topic,omitempty"`
	Message      *protocol.Message `json:"message,omitempty"`
	MessageID    string            `json:"message_id,omitempty"`
	SubscriberID string            `json:"subscriber_id,omitempty"`
	// Exchange fields
	ExchangeName string            `json:"exchange_name,omitempty"`
	ExchangeType string            `json:"exchange_type,omitempty"`
	Durable      bool              `json:"durable,omitempty"`
	BindingKey   string            `json:"binding_key,omitempty"`
	RoutingKey   string            `json:"routing_key,omitempty"`
	QueueName    string            `json:"queue_name,omitempty"`
	MatchHeaders map[string]string `json:"match_headers,omitempty"`
	MatchAll     bool              `json:"match_all,omitempty"`

	// Metadata fields. Meta carries the whole payload for control-plane
	// commands rather than spreading it across the flat fields above, which
	// already serve a different plane and would collide on names like Topic.
	Meta *MetaCommand `json:"meta,omitempty"`
}

// MetaCommand is the payload of a control-plane command.
//
// Timestamps travel inside the command instead of being read from the clock in
// FSM.Apply. Apply runs on every node at a different wall-clock moment, so a
// clock read there would produce state that differs between replicas — the one
// thing a replicated state machine must never do.
type MetaCommand struct {
	Broker    *BrokerInfo `json:"broker,omitempty"`
	NodeID    string      `json:"node_id,omitempty"`
	Fenced    bool        `json:"fenced,omitempty"`
	Timestamp int64       `json:"timestamp,omitempty"`

	Topic     string `json:"topic,omitempty"`
	Partition int32  `json:"partition,omitempty"`

	// TopicMeta and Placements describe a topic being created. Placements is
	// indexed by partition ID; Placements[i] is that partition's replica list
	// in preference order.
	TopicMeta  *TopicMeta `json:"topic_meta,omitempty"`
	Placements [][]string `json:"placements,omitempty"`

	Leader      string   `json:"leader,omitempty"`
	LeaderEpoch uint32   `json:"leader_epoch,omitempty"`
	ISR         []string `json:"isr,omitempty"`
	Replicas    []string `json:"replicas,omitempty"`
}

// Encode serializes the command to JSON bytes.
func (c *RaftCommand) Encode() ([]byte, error) {
	return json.Marshal(c)
}

// DecodeCommand deserializes a command from JSON bytes.
func DecodeCommand(data []byte) (*RaftCommand, error) {
	var cmd RaftCommand
	if err := json.Unmarshal(data, &cmd); err != nil {
		return nil, err
	}
	return &cmd, nil
}
