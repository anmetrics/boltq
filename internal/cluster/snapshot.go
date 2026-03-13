package cluster

import (
	"encoding/json"

	"github.com/hashicorp/raft"
)

// FSMSnapshot implements raft.FSMSnapshot for BrokerFSM.
type FSMSnapshot struct {
	data fsmSnapshotData
}

// Persist writes the snapshot to the given sink.
func (s *FSMSnapshot) Persist(sink raft.SnapshotSink) error {
	data, err := json.Marshal(s.data)
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

// Release is a no-op.
func (s *FSMSnapshot) Release() {}
