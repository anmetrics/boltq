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

	// Control-plane results. Changed distinguishes "applied and something moved"
	// from "applied and was already true", which is what lets the controller
	// avoid logging a failover that did not happen.
	Broker     *BrokerInfo
	Assignment *PartitionAssignment
	Changed    bool
}

// BrokerFSM implements raft.FSM. It applies Raft log entries to the local
// broker (queue plane) and to the metadata store (stream control plane).
//
// One FSM serves both planes because they must share a single ordered log: a
// partition reassignment and a queue write that were committed in some order
// have to be applied in that same order on every node, and two FSMs would give
// them two independent orders.
type BrokerFSM struct {
	broker   *broker.Broker
	metadata *MetadataStore
}

// NewBrokerFSM creates a new FSM wrapping the given broker.
func NewBrokerFSM(b *broker.Broker) *BrokerFSM {
	return &BrokerFSM{broker: b, metadata: NewMetadataStore()}
}

// Metadata returns the replicated control-plane state.
func (f *BrokerFSM) Metadata() *MetadataStore { return f.metadata }

// Apply is called by Raft when a log entry is committed by a quorum.
func (f *BrokerFSM) Apply(log *raft.Log) interface{} {
	cmd, err := DecodeCommand(log.Data)
	if err != nil {
		return &ApplyResponse{Error: fmt.Errorf("decode command: %w", err)}
	}
	return f.ApplyCommand(cmd)
}

// ApplyCommand applies an already-decoded command. Apply is the Raft entry
// point; this is the same logic reachable without a raft.Log wrapper, which is
// what lets the control plane's decisions be tested without a consensus
// cluster.
func (f *BrokerFSM) ApplyCommand(cmd *RaftCommand) *ApplyResponse {
	switch cmd.Type {
	case CmdRaftPublish:
		// Idempotency check: if message already exists, skip to avoid duplicates.
		if f.broker.HasMessage(cmd.Message.ID) {
			return &ApplyResponse{}
		}
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
		// Idempotency check: only promote if it's still in the delayed list.
		if !f.broker.HasDelayedMessage(cmd.MessageID) {
			return &ApplyResponse{}
		}
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

	case CmdRaftExchangeDeclare:
		err := f.broker.ExchangeDeclare(cmd.ExchangeName, broker.ExchangeType(cmd.ExchangeType), cmd.Durable)
		return &ApplyResponse{Error: err}

	case CmdRaftExchangeDelete:
		err := f.broker.ExchangeDelete(cmd.ExchangeName)
		return &ApplyResponse{Error: err}

	case CmdRaftBindQueue:
		err := f.broker.BindQueue(cmd.ExchangeName, cmd.QueueName, cmd.BindingKey, cmd.MatchHeaders, cmd.MatchAll)
		return &ApplyResponse{Error: err}

	case CmdRaftUnbindQueue:
		err := f.broker.UnbindQueue(cmd.ExchangeName, cmd.QueueName, cmd.BindingKey)
		return &ApplyResponse{Error: err}

	case CmdRaftPublishExchange:
		err := f.broker.PublishExchange(cmd.ExchangeName, cmd.RoutingKey, cmd.Message)
		return &ApplyResponse{Error: err}

	case CmdMetaRegisterBroker, CmdMetaUnregisterBroker, CmdMetaFenceBroker,
		CmdMetaCreateTopic, CmdMetaAssignLeader, CmdMetaUpdateISR, CmdMetaReassign:
		return f.applyMeta(cmd)

	default:
		return &ApplyResponse{Error: fmt.Errorf("unknown command type: %d", cmd.Type)}
	}
}

// applyMeta applies a control-plane command to the metadata store.
//
// Combined mode only. New deployments run the control plane in its own group
// with its own FSM; this path exists so a cluster that predates the split keeps
// working across the upgrade. It shares applyMetaCommand with MetadataFSM so a
// command cannot mean two different things depending on which group carried it.
func (f *BrokerFSM) applyMeta(cmd *RaftCommand) *ApplyResponse {
	return applyMetaCommand(f.metadata, cmd)
}

// fsmSnapshotData is the JSON-serializable snapshot payload.
//
// Metadata is a pointer so a snapshot written by an older binary — which has no
// metadata field at all — restores as "no control-plane state" rather than as
// an empty one that would wipe every partition assignment on restart.
type fsmSnapshotData struct {
	State    broker.FullState `json:"state"`
	Metadata *MetadataState   `json:"metadata,omitempty"`
}

// Snapshot returns a point-in-time snapshot of the FSM state.
func (f *BrokerFSM) Snapshot() (raft.FSMSnapshot, error) {
	data := f.broker.SnapshotFullState()
	meta := f.metadata.Snapshot()
	return &FSMSnapshot{data: fsmSnapshotData{State: data, Metadata: &meta}}, nil
}

// Restore replaces the FSM state from a snapshot.
func (f *BrokerFSM) Restore(rc io.ReadCloser) error {
	defer rc.Close()
	var data fsmSnapshotData
	if err := json.NewDecoder(rc).Decode(&data); err != nil {
		return fmt.Errorf("restore snapshot: %w", err)
	}
	f.broker.RestoreFullState(data.State)
	if data.Metadata != nil {
		f.metadata.Restore(*data.Metadata)
	}
	return nil
}
