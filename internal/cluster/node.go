package cluster

import (
	"fmt"
	"log"
	"net"
	"os"
	"path/filepath"
	"time"

	"github.com/hashicorp/go-hclog"
	"github.com/hashicorp/raft"
	raftboltdb "github.com/hashicorp/raft-boltdb/v2"

	"github.com/boltq/boltq/internal/broker"
	"github.com/boltq/boltq/internal/config"
)

// RaftNode manages the Raft consensus instance.
type RaftNode struct {
	raft          *raft.Raft
	fsm           *BrokerFSM
	nodeID        string
	raftAddr      string
	transport     *raft.NetworkTransport
	logStore      raft.LogStore
	stableStore   raft.StableStore
	snapshotStore raft.SnapshotStore
	localBroker   *broker.Broker
}

// NewRaftNode creates and starts a new Raft node.
func NewRaftNode(cfg config.ClusterConfig, localBroker *broker.Broker) (*RaftNode, error) {
	if cfg.NodeID == "" {
		return nil, fmt.Errorf("cluster: node_id is required")
	}
	if cfg.RaftAddr == "" {
		cfg.RaftAddr = "0.0.0.0:9100"
	}
	if cfg.RaftDir == "" {
		cfg.RaftDir = "./data/raft"
	}

	// Ensure raft data directory exists.
	nodeDir := filepath.Join(cfg.RaftDir, cfg.NodeID)
	if err := os.MkdirAll(nodeDir, 0o755); err != nil {
		return nil, fmt.Errorf("cluster: mkdir raft dir: %w", err)
	}

	// Raft configuration.
	raftCfg := raft.DefaultConfig()
	raftCfg.LocalID = raft.ServerID(cfg.NodeID)
	raftCfg.SnapshotThreshold = cfg.SnapshotThreshold
	if raftCfg.SnapshotThreshold == 0 {
		raftCfg.SnapshotThreshold = 8192
	}
	raftCfg.Logger = hclog.New(&hclog.LoggerOptions{
		Name:   "raft",
		Level:  hclog.Info,
		Output: os.Stderr,
	})

	// TCP transport.
	addr, err := net.ResolveTCPAddr("tcp", cfg.RaftAddr)
	if err != nil {
		return nil, fmt.Errorf("cluster: resolve raft addr: %w", err)
	}
	transport, err := raft.NewTCPTransport(cfg.RaftAddr, addr, 3, 10*time.Second, os.Stderr)
	if err != nil {
		return nil, fmt.Errorf("cluster: tcp transport: %w", err)
	}

	// BoltDB log/stable store.
	boltStore, err := raftboltdb.NewBoltStore(filepath.Join(nodeDir, "raft.db"))
	if err != nil {
		return nil, fmt.Errorf("cluster: bolt store: %w", err)
	}

	// File snapshot store (retain 2 snapshots).
	snapshotStore, err := raft.NewFileSnapshotStore(nodeDir, 2, os.Stderr)
	if err != nil {
		return nil, fmt.Errorf("cluster: snapshot store: %w", err)
	}

	// FSM.
	fsm := NewBrokerFSM(localBroker)

	// Create Raft instance.
	r, err := raft.NewRaft(raftCfg, fsm, boltStore, boltStore, snapshotStore, transport)
	if err != nil {
		return nil, fmt.Errorf("cluster: new raft: %w", err)
	}

	node := &RaftNode{
		raft:          r,
		fsm:           fsm,
		nodeID:        cfg.NodeID,
		raftAddr:      cfg.RaftAddr,
		transport:     transport,
		logStore:      boltStore,
		stableStore:   boltStore,
		snapshotStore: snapshotStore,
		localBroker:   localBroker,
	}

	// Bootstrap if this is the first node.
	if cfg.Bootstrap {
		hasState, err := raft.HasExistingState(boltStore, boltStore, snapshotStore)
		if err != nil {
			return nil, fmt.Errorf("cluster: check existing state: %w", err)
		}
		if !hasState {
			configuration := raft.Configuration{
				Servers: []raft.Server{
					{
						ID:      raft.ServerID(cfg.NodeID),
						Address: raft.ServerAddress(cfg.RaftAddr),
					},
				},
			}
			if f := r.BootstrapCluster(configuration); f.Error() != nil {
				return nil, fmt.Errorf("cluster: bootstrap: %w", f.Error())
			}
			log.Printf("[cluster] bootstrapped cluster as %s at %s", cfg.NodeID, cfg.RaftAddr)
		}
	}

	log.Printf("[cluster] raft node %s started at %s", cfg.NodeID, cfg.RaftAddr)
	return node, nil
}

// Apply submits a command to the Raft log and waits for it to be committed by a quorum.
func (n *RaftNode) Apply(cmd *RaftCommand, timeout time.Duration) (*ApplyResponse, error) {
	data, err := cmd.Encode()
	if err != nil {
		return nil, fmt.Errorf("cluster: encode command: %w", err)
	}
	future := n.raft.Apply(data, timeout)
	if err := future.Error(); err != nil {
		return nil, fmt.Errorf("cluster: raft apply: %w", err)
	}
	resp, ok := future.Response().(*ApplyResponse)
	if !ok {
		return nil, fmt.Errorf("cluster: unexpected response type")
	}
	return resp, nil
}

// Join adds a voter node to the cluster. Must be called on the leader.
func (n *RaftNode) Join(nodeID, addr string) error {
	f := n.raft.AddVoter(raft.ServerID(nodeID), raft.ServerAddress(addr), 0, 10*time.Second)
	if err := f.Error(); err != nil {
		return fmt.Errorf("cluster: add voter %s at %s: %w", nodeID, addr, err)
	}
	log.Printf("[cluster] voter %s at %s joined the cluster", nodeID, addr)
	return nil
}

// JoinNonVoter adds a non-voter (read replica) to the cluster.
// Non-voters receive replicated log entries but do not participate in elections or quorum.
// This allows scaling read capacity without impacting consensus performance.
func (n *RaftNode) JoinNonVoter(nodeID, addr string) error {
	f := n.raft.AddNonvoter(raft.ServerID(nodeID), raft.ServerAddress(addr), 0, 10*time.Second)
	if err := f.Error(); err != nil {
		return fmt.Errorf("cluster: add non-voter %s at %s: %w", nodeID, addr, err)
	}
	log.Printf("[cluster] non-voter %s at %s joined the cluster (read replica)", nodeID, addr)
	return nil
}

// Leave removes a node from the cluster. Must be called on the leader.
func (n *RaftNode) Leave(nodeID string) error {
	f := n.raft.RemoveServer(raft.ServerID(nodeID), 0, 10*time.Second)
	if err := f.Error(); err != nil {
		return fmt.Errorf("cluster: remove node %s: %w", nodeID, err)
	}
	log.Printf("[cluster] node %s removed from cluster", nodeID)
	return nil
}

// IsLeader returns true if this node is the current Raft leader.
func (n *RaftNode) IsLeader() bool {
	return n.raft.State() == raft.Leader
}

// LeaderAddr returns the address of the current leader, or empty string if unknown.
func (n *RaftNode) LeaderAddr() string {
	addr, _ := n.raft.LeaderWithID()
	return string(addr)
}

// LeaderID returns the ID of the current leader.
func (n *RaftNode) LeaderID() string {
	_, id := n.raft.LeaderWithID()
	return string(id)
}

// NodeID returns this node's ID.
func (n *RaftNode) NodeID() string {
	return n.nodeID
}

// Status returns cluster status information.
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

func (n *RaftNode) Status() ClusterStatus {
	stats := n.raft.Stats()
	var term, lastIndex uint64
	fmt.Sscanf(stats["term"], "%d", &term)
	fmt.Sscanf(stats["last_log_index"], "%d", &lastIndex)

	configFuture := n.raft.GetConfiguration()
	var peers []string
	if configFuture.Error() == nil {
		for _, srv := range configFuture.Configuration().Servers {
			peers = append(peers, fmt.Sprintf("%s@%s", srv.ID, srv.Address))
		}
	}

	return ClusterStatus{
		NodeID:    n.nodeID,
		RaftAddr:  n.raftAddr,
		State:     n.raft.State().String(),
		Leader:    n.LeaderAddr(),
		LeaderID:  n.LeaderID(),
		Term:      term,
		LastIndex: lastIndex,
		Peers:     peers,
	}
}

// Shutdown gracefully shuts down the Raft node.
func (n *RaftNode) Shutdown() error {
	log.Printf("[cluster] shutting down raft node %s", n.nodeID)
	f := n.raft.Shutdown()
	return f.Error()
}
