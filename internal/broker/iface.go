package broker

import "github.com/boltq/boltq/pkg/protocol"

// BrokerIface defines the interface that both local Broker and ClusterBroker implement.
// API handlers and scheduler use this interface so they work transparently with either mode.
type BrokerIface interface {
	Publish(topic string, msg *protocol.Message) error
	PublishTopic(topicName string, msg *protocol.Message) error
	Consume(topic string) *protocol.Message
	TryConsume(topic string) *protocol.Message
	Subscribe(topicName string, subscriberID string, bufSize int) <-chan *protocol.Message
	Unsubscribe(topicName string, subscriberID string)
	Ack(messageID string) error
	Nack(messageID string) error
	GetPendingMessages() map[string]*PendingMessage
	RequeueTimedOut(messageID string) error
	Stats() Stats
	Close()
}
