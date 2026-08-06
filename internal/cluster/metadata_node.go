package cluster

import (
	"time"

	"github.com/boltq/boltq/internal/config"
)

// MetadataNode is this node's membership in the control-plane consensus group.
//
// Every node in the cluster joins it. Controllers join as voters — three or
// five of them, no more, because each additional voter makes every metadata
// commit wait for one more machine without making the cluster survive one more
// failure. Data nodes join as non-voters: they receive the metadata they need to
// know what to lead and what to replicate, and they never slow a decision down.
//
// That asymmetry is the whole design. A thousand data nodes can follow a
// three-voter control plane, because following is cheap and deciding is not.
type MetadataNode struct {
	group *group
	fsm   *MetadataFSM
}

// NewMetadataNode starts the control-plane consensus group.
func NewMetadataNode(cfg config.ClusterConfig, addr string) (*MetadataNode, error) {
	fsm := NewMetadataFSM()
	g, err := newGroup(groupConfig{
		Name:              "metadata",
		NodeID:            cfg.NodeID,
		Addr:              addr,
		Dir:               cfg.RaftDir,
		Bootstrap:         cfg.Bootstrap,
		SnapshotThreshold: cfg.SnapshotThreshold,
	}, fsm)
	if err != nil {
		return nil, err
	}
	return &MetadataNode{group: g, fsm: fsm}, nil
}

// Metadata returns the replicated control-plane state.
//
// Readable on every node, voter or not. Routing a client to a partition leader
// must not require a round trip to consensus — and it does not, because every
// node already holds a copy that is at most one commit behind. The assignment
// Version is what turns "slightly stale" into something a caller can detect.
func (n *MetadataNode) Metadata() *MetadataStore { return n.fsm.Store() }

// Apply submits a control-plane command. Only the controller may call it
// successfully; everyone else routes through the Agent.
func (n *MetadataNode) Apply(cmd *RaftCommand, timeout time.Duration) (*ApplyResponse, error) {
	return n.group.Apply(cmd, timeout)
}

func (n *MetadataNode) Join(nodeID, addr string) error { return n.group.Join(nodeID, addr) }
func (n *MetadataNode) JoinNonVoter(nodeID, addr string) error {
	return n.group.JoinNonVoter(nodeID, addr)
}
func (n *MetadataNode) Leave(nodeID string) error { return n.group.Leave(nodeID) }
func (n *MetadataNode) IsLeader() bool            { return n.group.IsLeader() }
func (n *MetadataNode) VerifyLeader() error       { return n.group.VerifyLeader() }
func (n *MetadataNode) LeaderID() string          { return n.group.LeaderID() }
func (n *MetadataNode) LeaderAddr() string        { return n.group.LeaderAddr() }
func (n *MetadataNode) NodeID() string            { return n.group.NodeID() }
func (n *MetadataNode) Status() ClusterStatus     { return n.group.Status() }
func (n *MetadataNode) Shutdown() error           { return n.group.Shutdown() }
