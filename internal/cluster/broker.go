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
	if err := cb.node.VerifyLeader(); err != nil {
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

// PublishConfirm is identical to Publish in cluster mode since Raft Apply already
// guarantees quorum commit before returning.
func (cb *ClusterBroker) PublishConfirm(topic string, msg *protocol.Message) error {
	return cb.Publish(topic, msg)
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
	if err := cb.node.VerifyLeader(); err != nil {
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
	if err := cb.node.VerifyLeader(); err != nil {
		return nil
	}
	cmd := &RaftCommand{Type: CmdRaftConsume, Topic: topic}
	resp, err := cb.node.Apply(cmd, cb.applyTimeout)
	if err != nil {
		return nil
	}
	return resp.Message
}

// Subscribe creates a local pub/sub subscription and replicates durable registration.
func (cb *ClusterBroker) Subscribe(topicName string, subscriberID string, bufSize int, durable bool) <-chan *protocol.Message {
	if durable {
		if !cb.node.IsLeader() {
			return nil
		}
		cmd := &RaftCommand{
			Type:         CmdRaftSubscribe,
			Topic:        topicName,
			SubscriberID: subscriberID,
		}
		cb.node.Apply(cmd, cb.applyTimeout)
	}
	return cb.localBroker.Subscribe(topicName, subscriberID, bufSize, durable)
}

// Unsubscribe removes a local pub/sub subscription and replicates unregistration.
func (cb *ClusterBroker) Unsubscribe(topicName string, subscriberID string) {
	// Replicate unregistration
	if cb.node.IsLeader() {
		cmd := &RaftCommand{
			Type:         CmdRaftUnsubscribe,
			Topic:        topicName,
			SubscriberID: subscriberID,
		}
		cb.node.Apply(cmd, cb.applyTimeout)
	}
	cb.localBroker.Unsubscribe(topicName, subscriberID)
}

// Ack replicates an acknowledgment through Raft consensus.
func (cb *ClusterBroker) Ack(messageID string) error {
	if !cb.node.IsLeader() {
		return cb.notLeaderError()
	}
	if err := cb.node.VerifyLeader(); err != nil {
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

// PurgeQueue purges a queue through Raft consensus.
func (cb *ClusterBroker) PurgeQueue(topic string) (int64, error) {
	if !cb.node.IsLeader() {
		return 0, cb.notLeaderError()
	}
	cmd := &RaftCommand{Type: CmdRaftPurge, Topic: topic}
	resp, err := cb.node.Apply(cmd, cb.applyTimeout)
	if err != nil {
		return 0, err
	}
	return resp.PurgedCount, resp.Error
}

// PurgeDeadLetters purges a dead-letter queue through Raft consensus.
func (cb *ClusterBroker) PurgeDeadLetters(topic string) (int64, error) {
	if !cb.node.IsLeader() {
		return 0, cb.notLeaderError()
	}
	cmd := &RaftCommand{Type: CmdRaftPurgeDL, Topic: topic}
	resp, err := cb.node.Apply(cmd, cb.applyTimeout)
	if err != nil {
		return 0, err
	}
	return resp.PurgedCount, resp.Error
}

// Stats returns broker stats augmented with cluster info.
func (cb *ClusterBroker) Stats() broker.Stats {
	return cb.localBroker.Stats()
}

// StorageSize returns the current size of the underlying storage in bytes.
func (cb *ClusterBroker) StorageSize() int64 {
	return cb.localBroker.StorageSize()
}

// StorageMode returns the storage mode (memory or disk).
func (cb *ClusterBroker) StorageMode() string {
	return cb.localBroker.StorageMode()
}

// CompactionThreshold returns the current compaction threshold.
func (cb *ClusterBroker) CompactionThreshold() int64 {
	return cb.localBroker.CompactionThreshold()
}

// ProcessAdvancedFeatures processes delayed messages and maintenance on the leader.
// It uses Raft to replicate state mutations to followers.
func (cb *ClusterBroker) ProcessAdvancedFeatures() {
	if !cb.node.IsLeader() {
		return
	}

	// 1. Promote ready delayed messages via Raft.
	ready := cb.localBroker.GetReadyDelayedMessages()
	for _, msg := range ready {
		cmd := &RaftCommand{Type: CmdRaftPromote, MessageID: msg.ID}
		// We use a shorter timeout for internal maintenance.
		cb.node.Apply(cmd, 2*time.Second)
	}

	// 2. Perform local maintenance (e.g., reloading from spill files).
	// Currently, localBroker.ProcessAdvancedFeatures() also does local promotion.
	// In cluster mode, we want the promotion to be driven by Raft.
	// For now, it's safe because PromoteDelayed is idempotent if message is already gone.
	cb.localBroker.ProcessAdvancedFeatures()
}

// ExchangeDeclare replicates an exchange declaration through Raft.
func (cb *ClusterBroker) ExchangeDeclare(name string, typ broker.ExchangeType, durable bool) error {
	if !cb.node.IsLeader() {
		return cb.notLeaderError()
	}
	cmd := &RaftCommand{
		Type:         CmdRaftExchangeDeclare,
		ExchangeName: name,
		ExchangeType: string(typ),
		Durable:      durable,
	}
	resp, err := cb.node.Apply(cmd, cb.applyTimeout)
	if err != nil {
		return err
	}
	return resp.Error
}

// ExchangeDelete replicates an exchange deletion through Raft.
func (cb *ClusterBroker) ExchangeDelete(name string) error {
	if !cb.node.IsLeader() {
		return cb.notLeaderError()
	}
	cmd := &RaftCommand{
		Type:         CmdRaftExchangeDelete,
		ExchangeName: name,
	}
	resp, err := cb.node.Apply(cmd, cb.applyTimeout)
	if err != nil {
		return err
	}
	return resp.Error
}

// BindQueue replicates a queue binding through Raft.
func (cb *ClusterBroker) BindQueue(exchange, queueName, bindingKey string, headers map[string]string, matchAll bool) error {
	if !cb.node.IsLeader() {
		return cb.notLeaderError()
	}
	cmd := &RaftCommand{
		Type:         CmdRaftBindQueue,
		ExchangeName: exchange,
		QueueName:    queueName,
		BindingKey:   bindingKey,
		MatchHeaders: headers,
		MatchAll:     matchAll,
	}
	resp, err := cb.node.Apply(cmd, cb.applyTimeout)
	if err != nil {
		return err
	}
	return resp.Error
}

// UnbindQueue replicates a queue unbinding through Raft.
func (cb *ClusterBroker) UnbindQueue(exchange, queueName, bindingKey string) error {
	if !cb.node.IsLeader() {
		return cb.notLeaderError()
	}
	cmd := &RaftCommand{
		Type:         CmdRaftUnbindQueue,
		ExchangeName: exchange,
		QueueName:    queueName,
		BindingKey:   bindingKey,
	}
	resp, err := cb.node.Apply(cmd, cb.applyTimeout)
	if err != nil {
		return err
	}
	return resp.Error
}

// PublishExchange replicates an exchange publish through Raft.
func (cb *ClusterBroker) PublishExchange(exchange, routingKey string, msg *protocol.Message) error {
	if !cb.node.IsLeader() {
		return cb.notLeaderError()
	}
	if err := cb.node.VerifyLeader(); err != nil {
		return cb.notLeaderError()
	}
	cmd := &RaftCommand{
		Type:         CmdRaftPublishExchange,
		ExchangeName: exchange,
		RoutingKey:   routingKey,
		Message:      msg,
	}
	resp, err := cb.node.Apply(cmd, cb.applyTimeout)
	if err != nil {
		return err
	}
	return resp.Error
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
