package cluster

import (
	"encoding/json"
	"fmt"
	"io"

	"github.com/hashicorp/raft"
)

// MetadataFSM is the state machine of the control plane: who leads which
// partition, which replicas are in sync, and which brokers exist.
//
// It holds no message data of any kind, and that is its defining property. The
// log behind it stays small — one entry per leadership change, not per record —
// so a node can replay it in seconds, snapshot it in kilobytes, and a hundred
// nodes can follow it without any of them paying for data they do not serve.
//
// This is the split Kafka made with KRaft. Before it, BoltQ ran both planes
// through one FSM, which meant every data node also received and applied every
// queue write committed anywhere in the cluster.
type MetadataFSM struct {
	store *MetadataStore
}

// NewMetadataFSM creates an empty control-plane state machine.
func NewMetadataFSM() *MetadataFSM {
	return &MetadataFSM{store: NewMetadataStore()}
}

// Store returns the replicated metadata.
func (f *MetadataFSM) Store() *MetadataStore { return f.store }

// Apply is called by Raft for each committed entry.
func (f *MetadataFSM) Apply(entry *raft.Log) interface{} {
	cmd, err := DecodeCommand(entry.Data)
	if err != nil {
		return &ApplyResponse{Error: fmt.Errorf("decode command: %w", err)}
	}
	return f.ApplyCommand(cmd)
}

// ApplyCommand applies an already-decoded command.
//
// A queue-plane command reaching this FSM is a routing bug, not a no-op: it
// would mean a caller believes the two groups are interchangeable. Reporting it
// loudly is the only way that surfaces before it corrupts someone's mental
// model of where state lives.
func (f *MetadataFSM) ApplyCommand(cmd *RaftCommand) *ApplyResponse {
	switch cmd.Type {
	case CmdMetaRegisterBroker, CmdMetaUnregisterBroker, CmdMetaFenceBroker,
		CmdMetaCreateTopic, CmdMetaAssignLeader, CmdMetaUpdateISR, CmdMetaReassign:
		return applyMetaCommand(f.store, cmd)
	default:
		return &ApplyResponse{Error: fmt.Errorf(
			"cluster: command %d is not a control-plane command; it belongs to the queue group",
			cmd.Type)}
	}
}

// Snapshot captures the control-plane state.
func (f *MetadataFSM) Snapshot() (raft.FSMSnapshot, error) {
	return &metadataSnapshot{state: f.store.Snapshot()}, nil
}

// Restore replaces the state from a snapshot.
func (f *MetadataFSM) Restore(rc io.ReadCloser) error {
	defer rc.Close()
	var state MetadataState
	if err := json.NewDecoder(rc).Decode(&state); err != nil {
		return fmt.Errorf("restore metadata snapshot: %w", err)
	}
	f.store.Restore(state)
	return nil
}

type metadataSnapshot struct {
	state MetadataState
}

func (s *metadataSnapshot) Persist(sink raft.SnapshotSink) error {
	data, err := json.Marshal(s.state)
	if err != nil {
		sink.Cancel()
		return err
	}
	if _, err := sink.Write(data); err != nil {
		sink.Cancel()
		return err
	}
	return sink.Close()
}

func (s *metadataSnapshot) Release() {}

// applyMetaCommand mutates the metadata store. It is shared with the legacy
// combined FSM so both paths cannot drift — a control-plane command must mean
// exactly the same thing whichever group carries it during a migration.
func applyMetaCommand(store *MetadataStore, cmd *RaftCommand) *ApplyResponse {
	mc := cmd.Meta
	if mc == nil {
		return &ApplyResponse{Error: fmt.Errorf("metadata command %d has no payload", cmd.Type)}
	}

	switch cmd.Type {
	case CmdMetaRegisterBroker:
		if mc.Broker == nil {
			return &ApplyResponse{Error: fmt.Errorf("register broker: no broker info")}
		}
		return &ApplyResponse{Broker: store.applyRegisterBroker(*mc.Broker)}

	case CmdMetaUnregisterBroker:
		return &ApplyResponse{Error: store.applyUnregisterBroker(mc.NodeID)}

	case CmdMetaFenceBroker:
		changed, err := store.applyFenceBroker(mc.NodeID, mc.Fenced, mc.Timestamp)
		return &ApplyResponse{Error: err, Changed: changed}

	case CmdMetaCreateTopic:
		if mc.TopicMeta == nil {
			return &ApplyResponse{Error: fmt.Errorf("create topic: no topic metadata")}
		}
		return &ApplyResponse{Error: store.applyCreateTopic(*mc.TopicMeta, mc.Placements)}

	case CmdMetaAssignLeader:
		if err := store.applyAssignLeader(mc.Topic, mc.Partition, mc.Leader, mc.LeaderEpoch, mc.ISR); err != nil {
			return &ApplyResponse{Error: err}
		}
		a, _ := store.Assignment(mc.Topic, mc.Partition)
		return &ApplyResponse{Assignment: a, Changed: true}

	case CmdMetaUpdateISR:
		if err := store.applyUpdateISR(mc.Topic, mc.Partition, mc.LeaderEpoch, mc.ISR); err != nil {
			return &ApplyResponse{Error: err}
		}
		a, _ := store.Assignment(mc.Topic, mc.Partition)
		return &ApplyResponse{Assignment: a, Changed: true}

	case CmdMetaReassign:
		return &ApplyResponse{Error: store.applyReassign(mc.Topic, mc.Partition, mc.Replicas)}
	}
	return &ApplyResponse{Error: fmt.Errorf("unhandled metadata command: %d", cmd.Type)}
}
