package boltq

import (
	"bufio"
	"encoding/json"
	"fmt"
	"net"
	"sync"
	"time"

	"github.com/boltq/boltq/pkg/protocol"
)

// Client is the BoltQ Go SDK client using TCP protocol.
type Client struct {
	addr    string
	apiKey  string
	timeout time.Duration

	conn   net.Conn
	reader *bufio.Reader
	writer *bufio.Writer
	mu     sync.Mutex // serializes commands over the connection
}

// Message represents a consumed message.
type Message struct {
	ID        string            `json:"id"`
	Topic     string            `json:"topic"`
	Payload   json.RawMessage   `json:"payload"`
	Headers   map[string]string `json:"headers,omitempty"`
	Timestamp int64             `json:"timestamp"`
	Retry     int               `json:"retry"`
}

// Option configures the client.
type Option func(*Client)

// WithAPIKey sets the API key for authentication.
func WithAPIKey(key string) Option {
	return func(c *Client) { c.apiKey = key }
}

// WithTimeout sets the request timeout.
func WithTimeout(d time.Duration) Option {
	return func(c *Client) { c.timeout = d }
}

// New creates a new BoltQ TCP client. addr should be "host:port".
func New(addr string, opts ...Option) *Client {
	c := &Client{
		addr:    addr,
		timeout: 10 * time.Second,
	}
	for _, opt := range opts {
		opt(c)
	}
	return c
}

// Connect establishes the TCP connection and authenticates if apiKey is set.
func (c *Client) Connect() error {
	conn, err := net.DialTimeout("tcp", c.addr, c.timeout)
	if err != nil {
		return fmt.Errorf("connect: %w", err)
	}

	c.conn = conn
	c.reader = bufio.NewReaderSize(conn, 64*1024)
	c.writer = bufio.NewWriterSize(conn, 64*1024)

	// Authenticate if API key is set.
	if c.apiKey != "" {
		payload, _ := json.Marshal(map[string]string{"api_key": c.apiKey})
		resp, err := c.sendCommand(protocol.CmdAuth, payload)
		if err != nil {
			c.conn.Close()
			return fmt.Errorf("auth: %w", err)
		}
		if resp.Command != protocol.StatusOK {
			c.conn.Close()
			return fmt.Errorf("auth failed: %s", string(resp.Payload))
		}
	}

	return nil
}

// Close closes the TCP connection.
func (c *Client) Close() error {
	if c.conn != nil {
		return c.conn.Close()
	}
	return nil
}

// Publish sends a message to a work queue.
func (c *Client) Publish(topic string, payload interface{}, headers map[string]string) (string, error) {
	data, err := json.Marshal(payload)
	if err != nil {
		return "", fmt.Errorf("marshal payload: %w", err)
	}

	body, _ := json.Marshal(map[string]interface{}{
		"topic":   topic,
		"payload": json.RawMessage(data),
		"headers": headers,
	})

	resp, err := c.command(protocol.CmdPublish, body)
	if err != nil {
		return "", err
	}

	var result struct {
		ID string `json:"id"`
	}
	json.Unmarshal(resp, &result)
	return result.ID, nil
}

// PublishTopic sends a message to a pub/sub topic.
func (c *Client) PublishTopic(topic string, payload interface{}, headers map[string]string) (string, error) {
	data, err := json.Marshal(payload)
	if err != nil {
		return "", fmt.Errorf("marshal payload: %w", err)
	}

	body, _ := json.Marshal(map[string]interface{}{
		"topic":   topic,
		"payload": json.RawMessage(data),
		"headers": headers,
	})

	resp, err := c.command(protocol.CmdPublishTopic, body)
	if err != nil {
		return "", err
	}

	var result struct {
		ID string `json:"id"`
	}
	json.Unmarshal(resp, &result)
	return result.ID, nil
}

// Consume retrieves a message from a work queue. Returns nil if no message available.
func (c *Client) Consume(topic string) (*Message, error) {
	body, _ := json.Marshal(map[string]string{"topic": topic})

	frame, err := c.sendCommand(protocol.CmdConsume, body)
	if err != nil {
		return nil, err
	}

	if frame.Command == protocol.StatusEmpty {
		return nil, nil
	}
	if frame.Command == protocol.StatusError {
		return nil, fmt.Errorf("consume: %s", string(frame.Payload))
	}

	var msg Message
	if err := json.Unmarshal(frame.Payload, &msg); err != nil {
		return nil, fmt.Errorf("decode message: %w", err)
	}
	return &msg, nil
}

// Ack acknowledges a message.
func (c *Client) Ack(messageID string) error {
	body, _ := json.Marshal(map[string]string{"id": messageID})
	_, err := c.command(protocol.CmdAck, body)
	return err
}

// Nack negatively acknowledges a message (triggers retry).
func (c *Client) Nack(messageID string) error {
	body, _ := json.Marshal(map[string]string{"id": messageID})
	_, err := c.command(protocol.CmdNack, body)
	return err
}

// Ping pings the server.
func (c *Client) Ping() error {
	_, err := c.command(protocol.CmdPing, []byte("{}"))
	return err
}

// Stats returns broker statistics.
func (c *Client) Stats() (map[string]interface{}, error) {
	resp, err := c.command(protocol.CmdStats, []byte("{}"))
	if err != nil {
		return nil, err
	}

	var result map[string]interface{}
	if err := json.Unmarshal(resp, &result); err != nil {
		return nil, err
	}
	return result, nil
}

// Health checks if the server is healthy.
func (c *Client) Health() error {
	return c.Ping()
}

// command sends a command and returns the parsed payload, or error if status != OK.
func (c *Client) command(cmd byte, payload []byte) ([]byte, error) {
	frame, err := c.sendCommand(cmd, payload)
	if err != nil {
		return nil, err
	}
	if frame.Command == protocol.StatusError {
		var errResp struct {
			Error string `json:"error"`
		}
		json.Unmarshal(frame.Payload, &errResp)
		return nil, fmt.Errorf("server: %s", errResp.Error)
	}
	return frame.Payload, nil
}

// sendCommand sends a frame and reads the response. Thread-safe via mutex.
func (c *Client) sendCommand(cmd byte, payload []byte) (protocol.Frame, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.conn == nil {
		return protocol.Frame{}, fmt.Errorf("not connected")
	}

	c.conn.SetDeadline(time.Now().Add(c.timeout))

	if err := protocol.WriteFrame(c.writer, protocol.Frame{Command: cmd, Payload: payload}); err != nil {
		return protocol.Frame{}, fmt.Errorf("write: %w", err)
	}
	if err := c.writer.Flush(); err != nil {
		return protocol.Frame{}, fmt.Errorf("flush: %w", err)
	}

	resp, err := protocol.ReadFrame(c.reader)
	if err != nil {
		return protocol.Frame{}, fmt.Errorf("read: %w", err)
	}

	return resp, nil
}
