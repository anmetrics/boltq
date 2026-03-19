package cluster

import (
	"encoding/json"

	"github.com/boltq/boltq/pkg/protocol"
)

// CommandType identifies the type of Raft log entry command.
type CommandType uint8

const (
	CmdRaftPublish      CommandType = 1
	CmdRaftAck          CommandType = 2
	CmdRaftNack         CommandType = 3
	CmdRaftPublishTopic CommandType = 4
	CmdRaftConsume      CommandType = 5
	CmdRaftPromote      CommandType = 6
	CmdRaftPurge        CommandType = 7
	CmdRaftPurgeDL      CommandType = 8
	CmdRaftSubscribe       CommandType = 9
	CmdRaftUnsubscribe     CommandType = 10
	CmdRaftExchangeDeclare CommandType = 11
	CmdRaftExchangeDelete  CommandType = 12
	CmdRaftBindQueue       CommandType = 13
	CmdRaftUnbindQueue     CommandType = 14
	CmdRaftPublishExchange CommandType = 15
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
