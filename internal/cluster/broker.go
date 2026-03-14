package cluster

import (
	"time"

	"github.com/boltq/boltq/internal/broker"
	"github.com/boltq/boltq/pkg/protocol"
)

// ClusterBroker implements broker.BrokerIface and routes operations through Raft consensus.
// Write operations are replicated through the Raft log. Only the leader accepts operations.
type ClusterBroker struct {
	node         *RaftNode
	localBroker  *broker.Broker
	applyTimeout time.Duration
}

// NewClusterBroker creates a cluster-aware broker.
func NewClusterBroker(node *RaftNode, localBroker *broker.Broker) *ClusterBroker {
	return &ClusterBroker{
		node:         node,
		localBroker:  localBroker,
		applyTimeout: 5 * time.Second,
	}
}

func (cb *ClusterBroker) notLeaderError() error {
	return &NotLeaderError{
		Leader:   cb.node.LeaderAddr(),
		LeaderID: cb.node.LeaderID(),
	}
}

// Publish replicates a message publish through Raft consensus.
func (cb *ClusterBroker) Publish(topic string, msg *protocol.Message) error {
	if !cb.node.IsLeader() {
		return cb.notLeaderError()
	}
	msg.Topic = topic
	cmd := &RaftCommand{Type: CmdRaftPublish, Topic: topic, Message: msg}
	resp, err := cb.node.Apply(cmd, cb.applyTimeout)
	if err != nil {
		return err
	}
	return resp.Error
}

// PublishTopic replicates a pub/sub publish through Raft consensus.
func (cb *ClusterBroker) PublishTopic(topicName string, msg *protocol.Message) error {
	if !cb.node.IsLeader() {
		return cb.notLeaderError()
	}
	msg.Topic = topicName
	cmd := &RaftCommand{Type: CmdRaftPublishTopic, Topic: topicName, Message: msg}
	resp, err := cb.node.Apply(cmd, cb.applyTimeout)
	if err != nil {
		return err
	}
	return resp.Error
}

// Consume retrieves a message through Raft consensus (consume is a state mutation).
func (cb *ClusterBroker) Consume(topic string) *protocol.Message {
	if !cb.node.IsLeader() {
		return nil
	}
	cmd := &RaftCommand{Type: CmdRaftConsume, Topic: topic}
	resp, err := cb.node.Apply(cmd, cb.applyTimeout)
	if err != nil {
		return nil
	}
	return resp.Message
}

// TryConsume retrieves a message through Raft consensus (non-blocking).
func (cb *ClusterBroker) TryConsume(topic string) *protocol.Message {
	if !cb.node.IsLeader() {
		return nil
	}
	cmd := &RaftCommand{Type: CmdRaftConsume, Topic: topic}
	resp, err := cb.node.Apply(cmd, cb.applyTimeout)
	if err != nil {
		return nil
	}
	return resp.Message
}

// Subscribe creates a local pub/sub subscription. Pub/sub is leader-local.
func (cb *ClusterBroker) Subscribe(topicName string, subscriberID string, bufSize int, durable bool) <-chan *protocol.Message {
	return cb.localBroker.Subscribe(topicName, subscriberID, bufSize, durable)
}

// Unsubscribe removes a local pub/sub subscription.
func (cb *ClusterBroker) Unsubscribe(topicName string, subscriberID string) {
	cb.localBroker.Unsubscribe(topicName, subscriberID)
}

// Ack replicates an acknowledgment through Raft consensus.
func (cb *ClusterBroker) Ack(messageID string) error {
	if !cb.node.IsLeader() {
		return cb.notLeaderError()
	}
	cmd := &RaftCommand{Type: CmdRaftAck, MessageID: messageID}
	resp, err := cb.node.Apply(cmd, cb.applyTimeout)
	if err != nil {
		return err
	}
	return resp.Error
}

// Nack replicates a negative acknowledgment through Raft consensus.
func (cb *ClusterBroker) Nack(messageID string) error {
	if !cb.node.IsLeader() {
		return cb.notLeaderError()
	}
	cmd := &RaftCommand{Type: CmdRaftNack, MessageID: messageID}
	resp, err := cb.node.Apply(cmd, cb.applyTimeout)
	if err != nil {
		return err
	}
	return resp.Error
}

// GetPendingMessages returns pending messages (leader-local state).
func (cb *ClusterBroker) GetPendingMessages() map[string]*broker.PendingMessage {
	return cb.localBroker.GetPendingMessages()
}

// RequeueTimedOut requeues a timed-out message through Raft.
func (cb *ClusterBroker) RequeueTimedOut(messageID string) error {
	if !cb.node.IsLeader() {
		return cb.notLeaderError()
	}
	// Requeue is essentially a Nack for timeout purposes.
	cmd := &RaftCommand{Type: CmdRaftNack, MessageID: messageID}
	resp, err := cb.node.Apply(cmd, cb.applyTimeout)
	if err != nil {
		return err
	}
	return resp.Error
}

// PurgeQueue purges a queue (leader-local, not replicated for simplicity).
func (cb *ClusterBroker) PurgeQueue(topic string) (int64, error) {
	if !cb.node.IsLeader() {
		return 0, cb.notLeaderError()
	}
	return cb.localBroker.PurgeQueue(topic)
}

// PurgeDeadLetters purges a dead-letter queue.
func (cb *ClusterBroker) PurgeDeadLetters(topic string) (int64, error) {
	if !cb.node.IsLeader() {
		return 0, cb.notLeaderError()
	}
	return cb.localBroker.PurgeDeadLetters(topic)
}

// Stats returns broker stats augmented with cluster info.
func (cb *ClusterBroker) Stats() broker.Stats {
	return cb.localBroker.Stats()
}

// ProcessAdvancedFeatures processes delayed and expired messages in the local broker.
func (cb *ClusterBroker) ProcessAdvancedFeatures() {
	cb.localBroker.ProcessAdvancedFeatures()
}

// Close shuts down the cluster broker.
func (cb *ClusterBroker) Close() {
	cb.node.Shutdown()
	cb.localBroker.Close()
}

// Node returns the underlying RaftNode for cluster management operations.
func (cb *ClusterBroker) Node() *RaftNode {
	return cb.node
}
