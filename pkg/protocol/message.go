package protocol

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"time"
)

// Message represents a message in the queue system.
type Message struct {
	ID        string            `json:"id"`
	Topic     string            `json:"topic"`
	Payload   []byte            `json:"payload"`
	Headers   map[string]string `json:"headers,omitempty"`
	Timestamp int64             `json:"timestamp"`
	Retry         int               `json:"retry"`
	MaxRetry      int               `json:"max_retry"`
	DeliveryCount int               `json:"delivery_count"`
	Delay         int64             `json:"delay,omitempty"`      // delay in nanoseconds
	TTL           int64             `json:"ttl,omitempty"`        // time to live in nanoseconds
	DeliverAt     int64             `json:"deliver_at,omitempty"` // UnixNano
	ExpiresAt     int64             `json:"expires_at,omitempty"` // UnixNano
	Priority      int               `json:"priority,omitempty"`   // 0-9, higher = more urgent
	Exchange      string            `json:"exchange,omitempty"`
	RoutingKey    string            `json:"routing_key,omitempty"`
}

// NewMessage creates a new message with a generated ID and current timestamp.
func NewMessage(topic string, payload []byte, headers map[string]string) *Message {
	return &Message{
		ID:        generateID(),
		Topic:     topic,
		Payload:   payload,
		Headers:   headers,
		Timestamp: time.Now().UnixNano(),
		Retry:     0,
		MaxRetry:  5,
	}
}

// GenerateID creates a random hex-encoded message ID.
func GenerateID() string {
	return generateID()
}

func generateID() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// Encode serializes the message to JSON bytes.
func (m *Message) Encode() ([]byte, error) {
	return json.Marshal(m)
}

// DecodeMessage deserializes a message from JSON bytes.
func DecodeMessage(data []byte) (*Message, error) {
	var msg Message
	if err := json.Unmarshal(data, &msg); err != nil {
		return nil, err
	}
	return &msg, nil
}
