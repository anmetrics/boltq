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
)

// A group is one Raft consensus instance: transport, log store, snapshots and
// the state machine they drive.
//
// It exists because BoltQ runs two of them, and that separation is the whole
// point rather than an implementation detail.
//
// Kafka arrived at the same shape with KRaft: cluster metadata lives in its own
// replicated log with a small quorum, and brokers observe it without voting.
// Everything else — the actual data — travels by a different path. The reason is
// not tidiness. A single group means every node that needs *metadata* also
// receives, stores and applies every *data* record committed anywhere in the
// cluster. Scale the node count and that cost grows linearly, on machines that
// may serve none of that data.
//
// The two groups here never need commands ordered against each other: metadata
// commands touch the metadata store, queue commands touch the broker, and
// neither reads the other's state. Ordering between them would be meaningless,
// which is exactly the condition under which splitting is safe.
type group struct {
	raft          *raft.Raft
	nodeID        string
	addr          string
	name          string
	transport     *raft.NetworkTransport
	store         *raftboltdb.BoltStore
	snapshotStore raft.SnapshotStore
}

// groupConfig configures one consensus instance.
type groupConfig struct {
	// Name distinguishes the groups in logs and on disk. Two groups sharing a
	// directory would corrupt each other's state, so it is also the subdirectory.
	Name   string
	NodeID string
	Addr   string
	Dir    string
	// Bootstrap forms a new single-node cluster, and only ever on empty state.
	Bootstrap         bool
	SnapshotThreshold uint64
}

func newGroup(cfg groupConfig, fsm raft.FSM) (*group, error) {
	if cfg.NodeID == "" {
		return nil, fmt.Errorf("cluster: node_id is required for group %q", cfg.Name)
	}
	if cfg.Addr == "" {
		return nil, fmt.Errorf("cluster: addr is required for group %q", cfg.Name)
	}

	dir := filepath.Join(cfg.Dir, cfg.NodeID, cfg.Name)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("cluster: mkdir %s: %w", dir, err)
	}

	raftCfg := raft.DefaultConfig()
	raftCfg.LocalID = raft.ServerID(cfg.NodeID)
	raftCfg.SnapshotThreshold = cfg.SnapshotThreshold
	if raftCfg.SnapshotThreshold == 0 {
		raftCfg.SnapshotThreshold = 8192
	}
	raftCfg.Logger = hclog.New(&hclog.LoggerOptions{
		Name:   "raft-" + cfg.Name,
		Level:  hclog.Info,
		Output: os.Stderr,
	})

	tcpAddr, err := net.ResolveTCPAddr("tcp", cfg.Addr)
	if err != nil {
		return nil, fmt.Errorf("cluster: resolve %s addr: %w", cfg.Name, err)
	}
	transport, err := raft.NewTCPTransport(cfg.Addr, tcpAddr, 3, 10*time.Second, os.Stderr)
	if err != nil {
		return nil, fmt.Errorf("cluster: %s transport: %w", cfg.Name, err)
	}

	store, err := raftboltdb.NewBoltStore(filepath.Join(dir, "raft.db"))
	if err != nil {
		return nil, fmt.Errorf("cluster: %s bolt store: %w", cfg.Name, err)
	}
	snapshots, err := raft.NewFileSnapshotStore(dir, 2, os.Stderr)
	if err != nil {
		return nil, fmt.Errorf("cluster: %s snapshot store: %w", cfg.Name, err)
	}

	r, err := raft.NewRaft(raftCfg, fsm, store, store, snapshots, transport)
	if err != nil {
		return nil, fmt.Errorf("cluster: new raft %s: %w", cfg.Name, err)
	}

	g := &group{
		raft: r, nodeID: cfg.NodeID, addr: cfg.Addr, name: cfg.Name,
		transport: transport, store: store, snapshotStore: snapshots,
	}

	if cfg.Bootstrap {
		hasState, err := raft.HasExistingState(store, store, snapshots)
		if err != nil {
			return nil, fmt.Errorf("cluster: %s check state: %w", cfg.Name, err)
		}
		if !hasState {
			configuration := raft.Configuration{Servers: []raft.Server{{
				ID:      raft.ServerID(cfg.NodeID),
				Address: raft.ServerAddress(cfg.Addr),
			}}}
			if f := r.BootstrapCluster(configuration); f.Error() != nil {
				return nil, fmt.Errorf("cluster: bootstrap %s: %w", cfg.Name, f.Error())
			}
			log.Printf("[cluster] bootstrapped %s group as %s at %s", cfg.Name, cfg.NodeID, cfg.Addr)
		}
	}

	log.Printf("[cluster] %s group: node %s at %s", cfg.Name, cfg.NodeID, cfg.Addr)
	return g, nil
}

// Apply submits a command and waits for a quorum to commit it.
func (g *group) Apply(cmd *RaftCommand, timeout time.Duration) (*ApplyResponse, error) {
	data, err := cmd.Encode()
	if err != nil {
		return nil, fmt.Errorf("cluster: encode command: %w", err)
	}
	future := g.raft.Apply(data, timeout)
	if err := future.Error(); err != nil {
		return nil, fmt.Errorf("cluster: %s apply: %w", g.name, err)
	}
	resp, ok := future.Response().(*ApplyResponse)
	if !ok {
		return nil, fmt.Errorf("cluster: %s unexpected response type", g.name)
	}
	return resp, nil
}

func (g *group) Join(nodeID, addr string) error {
	f := g.raft.AddVoter(raft.ServerID(nodeID), raft.ServerAddress(addr), 0, 10*time.Second)
	if err := f.Error(); err != nil {
		return fmt.Errorf("cluster: %s add voter %s at %s: %w", g.name, nodeID, addr, err)
	}
	log.Printf("[cluster] %s group: voter %s at %s joined", g.name, nodeID, addr)
	return nil
}

func (g *group) JoinNonVoter(nodeID, addr string) error {
	f := g.raft.AddNonvoter(raft.ServerID(nodeID), raft.ServerAddress(addr), 0, 10*time.Second)
	if err := f.Error(); err != nil {
		return fmt.Errorf("cluster: %s add non-voter %s at %s: %w", g.name, nodeID, addr, err)
	}
	log.Printf("[cluster] %s group: non-voter %s at %s joined", g.name, nodeID, addr)
	return nil
}

func (g *group) Leave(nodeID string) error {
	f := g.raft.RemoveServer(raft.ServerID(nodeID), 0, 10*time.Second)
	if err := f.Error(); err != nil {
		return fmt.Errorf("cluster: %s remove %s: %w", g.name, nodeID, err)
	}
	log.Printf("[cluster] %s group: node %s removed", g.name, nodeID)
	return nil
}

func (g *group) IsLeader() bool      { return g.raft.State() == raft.Leader }
func (g *group) VerifyLeader() error { return g.raft.VerifyLeader().Error() }
func (g *group) NodeID() string      { return g.nodeID }

func (g *group) LeaderAddr() string {
	addr, _ := g.raft.LeaderWithID()
	return string(addr)
}

func (g *group) LeaderID() string {
	_, id := g.raft.LeaderWithID()
	return string(id)
}

func (g *group) Status() ClusterStatus {
	stats := g.raft.Stats()
	var term, lastIndex uint64
	fmt.Sscanf(stats["term"], "%d", &term)
	fmt.Sscanf(stats["last_log_index"], "%d", &lastIndex)

	var peers []string
	if f := g.raft.GetConfiguration(); f.Error() == nil {
		for _, srv := range f.Configuration().Servers {
			peers = append(peers, fmt.Sprintf("%s@%s", srv.ID, srv.Address))
		}
	}
	return ClusterStatus{
		NodeID:    g.nodeID,
		RaftAddr:  g.addr,
		State:     g.raft.State().String(),
		Leader:    g.LeaderAddr(),
		LeaderID:  g.LeaderID(),
		Term:      term,
		LastIndex: lastIndex,
		Peers:     peers,
	}
}

// Shutdown stops consensus and releases the resources the group holds.
//
// Closing the store matters beyond tidiness: bbolt takes an exclusive lock on
// its file, so a store left open means a node restarted on the same data
// directory blocks forever waiting for a lock its own dead predecessor still
// holds. In a process that is exiting this is invisible; in a test, or in any
// supervisor that restarts a node in place, it is a hang with no error message.
//
// Order matters. Raft stops first so nothing is mid-write, then the transport
// so no peer can start a new request, and only then the store.
func (g *group) Shutdown() error {
	log.Printf("[cluster] shutting down %s group on %s", g.name, g.nodeID)

	err := g.raft.Shutdown().Error()

	if g.transport != nil {
		if cerr := g.transport.Close(); cerr != nil && err == nil {
			err = cerr
		}
	}
	if g.store != nil {
		if cerr := g.store.Close(); cerr != nil && err == nil {
			err = cerr
		}
	}
	return err
}
