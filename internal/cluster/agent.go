package cluster

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"time"
)

// The agent is a node's side of the control-plane conversation. It does two
// things, both small and both load-bearing:
//
//	register   once at boot, so the controller knows this node exists and where
//	           to reach it
//	heartbeat  periodically, so the controller knows it is still alive
//
// Heartbeats deliberately do not go through Raft. A cluster of 200 nodes beating
// every 5 seconds would put 40 writes/second of pure liveness noise through
// consensus, each costing a quorum fsync, for information that is worthless one
// tick later. Only the *conclusion* — this node is dead, fence it — is
// replicated. See the note at the top of controller.go.
//
// Registration, by contrast, must go through Raft: it is durable state that
// survives a controller failover, and a node that only told the current
// controller about itself would vanish the moment leadership moved.

// AgentConfig describes this node to the cluster.
type AgentConfig struct {
	NodeID string

	// The addresses other components need to reach this node. Empty fields are
	// simply not advertised, which is how a node that runs no gateway or no
	// replication listener describes itself honestly.
	AdminAddr   string
	RaftAddr    string
	StreamAddr  string
	GatewayAddr string
	// Rack is the failure domain — an availability zone, a physical rack. It
	// is what lets replica placement avoid putting every copy of a partition
	// somewhere a single power event can take out.
	Rack string

	// Seeds are control-plane HTTP addresses used before this node has learned
	// the cluster's own registry. After registration the agent prefers the
	// address it finds in metadata, which stays correct as leadership moves.
	Seeds []string

	// APIKey authenticates to the control API. It must match the cluster's.
	APIKey string

	// Interval is the heartbeat period. It must be comfortably shorter than the
	// controller's SessionTimeout — roughly a third, so two lost heartbeats do
	// not fence a healthy node. Default 5s.
	Interval time.Duration

	// Timeout bounds a single control-plane request. Default 3s.
	Timeout time.Duration
}

func (c *AgentConfig) applyDefaults() {
	if c.Interval <= 0 {
		c.Interval = 5 * time.Second
	}
	if c.Timeout <= 0 {
		c.Timeout = 3 * time.Second
	}
}

// Agent keeps this node registered and visibly alive.
type Agent struct {
	cfg  AgentConfig
	node ControlNode
	http *http.Client

	stop chan struct{}
	done chan struct{}
}

// NewAgent creates an agent. node may be this process's Raft node, which lets
// the agent skip the network entirely when it is itself the controller.
func NewAgent(node ControlNode, cfg AgentConfig) *Agent {
	cfg.applyDefaults()
	return &Agent{
		cfg:  cfg,
		node: node,
		http: &http.Client{Timeout: cfg.Timeout},
		stop: make(chan struct{}),
		done: make(chan struct{}),
	}
}

// Start registers this node and begins heartbeating.
func (a *Agent) Start() {
	go a.run()
}

// Close stops the agent.
func (a *Agent) Close() {
	select {
	case <-a.stop:
		return
	default:
	}
	close(a.stop)
	<-a.done
}

func (a *Agent) run() {
	defer close(a.done)

	ticker := time.NewTicker(a.cfg.Interval)
	defer ticker.Stop()

	// Register before the first heartbeat: a heartbeat for a node the
	// controller has never heard of has nothing to attach to.
	registered := a.register() == nil

	for {
		select {
		case <-a.stop:
			return
		case <-ticker.C:
			if !registered {
				// Registration fails while no leader is elected, which is
				// normal during a cold start. Keep retrying on the heartbeat
				// tick rather than with a separate backoff loop.
				registered = a.register() == nil
				if !registered {
					continue
				}
			}
			if err := a.heartbeat(); err != nil {
				// A failed heartbeat usually means leadership just moved. The
				// next tick finds the new controller through metadata.
				continue
			}
		}
	}
}

// Apply submits a metadata command, routing it to the controller when this node
// is not it. It satisfies the applier interface the stream reconciler uses.
//
// This exists because partition leadership and Raft leadership are different
// things. A node that leads a partition is the only one that knows which
// replicas are caught up with it — but it is usually not the Raft leader, and
// only the Raft leader can commit. Without this hop, every ISR report from a
// partition leader that is not the controller is silently dropped, and the
// cluster's view of durability quietly stops matching reality.
func (a *Agent) Apply(cmd *RaftCommand, timeout time.Duration) (*ApplyResponse, error) {
	if a.node != nil && a.node.IsLeader() {
		return a.node.Apply(cmd, timeout)
	}

	body, err := json.Marshal(cmd)
	if err != nil {
		return nil, err
	}
	if err := a.postToController("/cluster/meta", body); err != nil {
		return nil, err
	}
	return &ApplyResponse{}, nil
}

// BrokerInfo builds this node's registration payload.
func (a *Agent) BrokerInfo() BrokerInfo {
	return BrokerInfo{
		NodeID:      a.cfg.NodeID,
		AdminAddr:   a.cfg.AdminAddr,
		RaftAddr:    a.cfg.RaftAddr,
		StreamAddr:  a.cfg.StreamAddr,
		GatewayAddr: a.cfg.GatewayAddr,
		Rack:        a.cfg.Rack,
	}
}

// register records this node in the replicated registry.
func (a *Agent) register() error {
	info := a.BrokerInfo()

	// When this node is the controller, apply directly. The round trip through
	// its own HTTP listener would work, but it would also fail during the
	// window where the listener is not yet serving.
	if a.node != nil && a.node.IsLeader() {
		resp, err := a.node.Apply(&RaftCommand{
			Type: CmdMetaRegisterBroker,
			Meta: &MetaCommand{Broker: &info, Timestamp: time.Now().UnixNano()},
		}, a.cfg.Timeout)
		if err != nil {
			return err
		}
		if resp.Error != nil {
			return resp.Error
		}
		log.Printf("[agent] registered %s with the control plane", a.cfg.NodeID)
		return nil
	}

	body, err := json.Marshal(info)
	if err != nil {
		return err
	}
	if err := a.postToController("/cluster/register", body); err != nil {
		return err
	}
	log.Printf("[agent] registered %s with the control plane", a.cfg.NodeID)
	return nil
}

// heartbeat tells the controller this node is still alive.
func (a *Agent) heartbeat() error {
	if a.node != nil && a.node.IsLeader() {
		// The controller running in this process observes itself directly;
		// see Controller.Heartbeat.
		return nil
	}
	body, _ := json.Marshal(map[string]string{"node_id": a.cfg.NodeID})
	return a.postToController("/cluster/heartbeat", body)
}

// postToController sends a request to whichever node currently leads.
//
// The controller's address comes from the replicated registry first — it is
// always current, because leadership changes are themselves replicated — and
// falls back to the configured seeds only when the registry cannot answer,
// which is the case exactly once, before this node has registered.
func (a *Agent) postToController(path string, body []byte) error {
	var lastErr error
	for _, addr := range a.controllerAddrs() {
		url := fmt.Sprintf("http://%s%s", addr, path)
		req, err := http.NewRequestWithContext(context.Background(), http.MethodPost, url, bytes.NewReader(body))
		if err != nil {
			return err
		}
		req.Header.Set("Content-Type", "application/json")
		if a.cfg.APIKey != "" {
			req.Header.Set("X-API-Key", a.cfg.APIKey)
		}

		resp, err := a.http.Do(req)
		if err != nil {
			lastErr = err
			continue
		}
		resp.Body.Close()

		if resp.StatusCode == http.StatusOK {
			return nil
		}
		// 421 means "not the leader" — try the next candidate rather than
		// treating a correct redirect as a failure.
		lastErr = fmt.Errorf("%s returned %d", addr, resp.StatusCode)
	}
	if lastErr == nil {
		lastErr = fmt.Errorf("no control-plane address is reachable")
	}
	return lastErr
}

// controllerAddrs returns candidate control API addresses, best first.
func (a *Agent) controllerAddrs() []string {
	var out []string
	seen := map[string]bool{}
	add := func(s string) {
		if s != "" && !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}

	if a.node != nil {
		if leaderID := a.node.LeaderID(); leaderID != "" {
			if b, ok := a.node.Metadata().Broker(leaderID); ok {
				add(b.AdminAddr)
			}
		}
	}
	for _, s := range a.cfg.Seeds {
		add(s)
	}
	return out
}
