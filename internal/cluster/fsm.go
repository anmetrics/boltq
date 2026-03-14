package cluster

import (
	"encoding/json"
	"fmt"
	"io"

	"github.com/hashicorp/raft"

	"github.com/boltq/boltq/internal/broker"
	"github.com/boltq/boltq/pkg/protocol"
)

// ApplyResponse is returned from FSM.Apply to the caller via raft.ApplyFuture.
type ApplyResponse struct {
	Error       error
	Message     *protocol.Message // returned for consume commands
	PurgedCount int64             // returned for purge commands
}

// BrokerFSM implements raft.FSM. It applies Raft log entries to the local broker.
type BrokerFSM struct {
	broker *broker.Broker
}

// NewBrokerFSM creates a new FSM wrapping the given broker.
func NewBrokerFSM(b *broker.Broker) *BrokerFSM {
	return &BrokerFSM{broker: b}
}

// Apply is called by Raft when a log entry is committed by a quorum.
func (f *BrokerFSM) Apply(log *raft.Log) interface{} {
	cmd, err := DecodeCommand(log.Data)
	if err != nil {
		return &ApplyResponse{Error: fmt.Errorf("decode command: %w", err)}
	}

	switch cmd.Type {
	case CmdRaftPublish:
		err := f.broker.Publish(cmd.Topic, cmd.Message)
		return &ApplyResponse{Error: err}

	case CmdRaftPublishTopic:
		err := f.broker.PublishTopic(cmd.Topic, cmd.Message)
		return &ApplyResponse{Error: err}

	case CmdRaftConsume:
		msg := f.broker.TryConsume(cmd.Topic)
		return &ApplyResponse{Message: msg}

	case CmdRaftAck:
		err := f.broker.Ack(cmd.MessageID)
		return &ApplyResponse{Error: err}

	case CmdRaftNack:
		err := f.broker.Nack(cmd.MessageID)
		return &ApplyResponse{Error: err}

	case CmdRaftPromote:
		err := f.broker.PromoteDelayed(cmd.MessageID)
		return &ApplyResponse{Error: err}

	case CmdRaftPurge:
		count, err := f.broker.PurgeQueue(cmd.Topic)
		return &ApplyResponse{Error: err, PurgedCount: count}

	case CmdRaftPurgeDL:
		count, err := f.broker.PurgeDeadLetters(cmd.Topic)
		return &ApplyResponse{Error: err, PurgedCount: count}

	case CmdRaftSubscribe:
		f.broker.RegisterDurableSub(cmd.Topic, cmd.SubscriberID)
		return &ApplyResponse{}

	case CmdRaftUnsubscribe:
		f.broker.UnregisterDurableSub(cmd.Topic, cmd.SubscriberID)
		return &ApplyResponse{}

	default:
		return &ApplyResponse{Error: fmt.Errorf("unknown command type: %d", cmd.Type)}
	}
}

// fsmSnapshotData is the JSON-serializable snapshot payload.
type fsmSnapshotData struct {
	State broker.FullState `json:"state"`
}

// Snapshot returns a point-in-time snapshot of the FSM state.
func (f *BrokerFSM) Snapshot() (raft.FSMSnapshot, error) {
	data := f.broker.SnapshotFullState()
	return &FSMSnapshot{data: fsmSnapshotData{State: data}}, nil
}

// Restore replaces the FSM state from a snapshot.
func (f *BrokerFSM) Restore(rc io.ReadCloser) error {
	defer rc.Close()
	var data fsmSnapshotData
	if err := json.NewDecoder(rc).Decode(&data); err != nil {
		return fmt.Errorf("restore snapshot: %w", err)
	}
	f.broker.RestoreFullState(data.State)
	return nil
}
