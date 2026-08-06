package cluster

import (
	"time"

	"github.com/boltq/boltq/internal/broker"
	"github.com/boltq/boltq/internal/config"
)

// RaftNode is this node's membership in the queue-plane consensus group.
//
// Every queue operation — publish, consume, ack — is replicated here, which
// makes the queue plane strongly consistent and also caps it at the throughput
// of consensus. That trade is appropriate for work queues and job distribution;
// it is not appropriate for a chat firehose, which is why the messaging plane
// does not use this group at all.
//
// This group is separate from the metadata group on purpose. Sharing one meant
// every node needing metadata also received and applied every queue write in
// the cluster — a cost that grew with node count, on machines that served none
// of that data.
type RaftNode struct {
	group       *group
	fsm         *BrokerFSM
	localBroker *broker.Broker
}

// NewRaftNode starts the queue-plane consensus group.
func NewRaftNode(cfg config.ClusterConfig, localBroker *broker.Broker) (*RaftNode, error) {
	addr := cfg.RaftAddr
	if addr == "" {
		addr = "0.0.0.0:9100"
	}
	if cfg.RaftDir == "" {
		cfg.RaftDir = "./data/raft"
	}

	fsm := NewBrokerFSM(localBroker)
	g, err := newGroup(groupConfig{
		Name:              "queue",
		NodeID:            cfg.NodeID,
		Addr:              addr,
		Dir:               cfg.RaftDir,
		Bootstrap:         cfg.Bootstrap,
		SnapshotThreshold: cfg.SnapshotThreshold,
	}, fsm)
	if err != nil {
		return nil, err
	}
	return &RaftNode{group: g, fsm: fsm, localBroker: localBroker}, nil
}

// Apply submits a queue command and waits for quorum commit.
func (n *RaftNode) Apply(cmd *RaftCommand, timeout time.Duration) (*ApplyResponse, error) {
	return n.group.Apply(cmd, timeout)
}

// Metadata returns the control-plane state carried by this FSM.
//
// Deprecated: control-plane state lives in MetadataNode. This remains only so a
// deployment that has not yet split its groups keeps working through an
// upgrade; it is the combined-mode path and will be removed once no cluster
// runs it.
func (n *RaftNode) Metadata() *MetadataStore { return n.fsm.Metadata() }

func (n *RaftNode) Join(nodeID, addr string) error         { return n.group.Join(nodeID, addr) }
func (n *RaftNode) JoinNonVoter(nodeID, addr string) error { return n.group.JoinNonVoter(nodeID, addr) }
func (n *RaftNode) Leave(nodeID string) error              { return n.group.Leave(nodeID) }
func (n *RaftNode) IsLeader() bool                         { return n.group.IsLeader() }
func (n *RaftNode) VerifyLeader() error                    { return n.group.VerifyLeader() }
func (n *RaftNode) LeaderAddr() string                     { return n.group.LeaderAddr() }
func (n *RaftNode) LeaderID() string                       { return n.group.LeaderID() }
func (n *RaftNode) NodeID() string                         { return n.group.NodeID() }
func (n *RaftNode) Status() ClusterStatus                  { return n.group.Status() }
func (n *RaftNode) Shutdown() error                        { return n.group.Shutdown() }

// ClusterStatus reports one consensus group's view of itself.
type ClusterStatus struct {
	NodeID    string   `json:"node_id"`
	RaftAddr  string   `json:"raft_addr"`
	State     string   `json:"state"` // Leader, Follower, Candidate
	Leader    string   `json:"leader"`
	LeaderID  string   `json:"leader_id"`
	Term      uint64   `json:"term"`
	LastIndex uint64   `json:"last_index"`
	Peers     []string `json:"peers"`
}
