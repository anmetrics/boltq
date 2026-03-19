package boltq

import (
	"bufio"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"net"
	"strings"
	"sync"
	"time"

	"github.com/boltq/boltq/pkg/protocol"
)

// Client is the BoltQ Go SDK client using TCP protocol.
type Client struct {
	addr      string
	apiKey    string
	timeout   time.Duration
	tlsConfig *tls.Config

	conn   net.Conn
	reader *bufio.Reader
	writer *bufio.Writer
	mu     sync.Mutex // serializes commands over the connection

	autoReconnect     bool
	reconnectInterval time.Duration

	subsMu sync.RWMutex
	subs   map[string]chan *Message // key: topic:subscriberID
	active bool                     // whether dispatcher is running
}

// Message represents a consumed message.
type Message struct {
	ID            string            `json:"id"`
	Topic         string            `json:"topic"`
	Payload       json.RawMessage   `json:"payload"`
	Headers       map[string]string `json:"headers,omitempty"`
	Timestamp     int64             `json:"timestamp"`
	Retry         int               `json:"retry"`
	DeliveryCount int               `json:"delivery_count"`
	Delay         int64             `json:"delay,omitempty"`
	TTL           int64             `json:"ttl,omitempty"`
}

// PublishOptions configures a single publish operation.
type PublishOptions struct {
	Delay    time.Duration
	TTL      time.Duration
	Priority int // 0-9, higher = more urgent
}

// NotLeaderError is returned when the connected node is not the cluster leader.
type NotLeaderError struct {
	Leader   string `json:"leader"`
	LeaderID string `json:"leader_id"`
}

func (e *NotLeaderError) Error() string {
	return fmt.Sprintf("not leader, current leader is %s (%s)", e.LeaderID, e.Leader)
}

// ServerError is returned when the server returns an error response.
type ServerError struct {
	Message string `json:"error"`
}

func (e *ServerError) Error() string {
	return fmt.Sprintf("server error: %s", e.Message)
}

// ClusterStatus represents the cluster status response.
type ClusterStatus struct {
	Enabled bool                   `json:"enabled"`
	Cluster map[string]interface{} `json:"cluster,omitempty"`
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

// WithTLS enables TLS and sets the TLS configuration.
func WithTLS(config *tls.Config) Option {
	return func(c *Client) { c.tlsConfig = config }
}

// WithAutoReconnect enables or disables automatic reconnection.
func WithAutoReconnect(enabled bool) Option {
	return func(c *Client) { c.autoReconnect = enabled }
}

// WithReconnectInterval sets the interval between reconnection attempts.
func WithReconnectInterval(d time.Duration) Option {
	return func(c *Client) { c.reconnectInterval = d }
}

// New creates a new BoltQ TCP client. addr should be "host:port".
func New(addr string, opts ...Option) *Client {
	c := &Client{
		addr:              addr,
		timeout:           10 * time.Second,
		autoReconnect:     true,
		reconnectInterval: 2 * time.Second,
	}
	for _, opt := range opts {
		opt(c)
	}
	return c
}

// Connect establishes the TCP connection and authenticates if apiKey is set.
func (c *Client) Connect() error {
	var conn net.Conn
	var err error

	if c.tlsConfig != nil {
		conn, err = tls.DialWithDialer(&net.Dialer{Timeout: c.timeout}, "tcp", c.addr, c.tlsConfig)
	} else {
		conn, err = net.DialTimeout("tcp", c.addr, c.timeout)
	}

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

	// Restart dispatcher if needed
	c.subsMu.Lock()
	if !c.active {
		c.active = true
		go c.dispatcher()
	}
	c.subsMu.Unlock()

	return nil
}

// Close closes the TCP connection.
func (c *Client) Close() error {
	c.subsMu.Lock()
	c.active = false
	c.subsMu.Unlock()
	if c.conn != nil {
		return c.conn.Close()
	}
	return nil
}

// EnableConfirm enables publisher confirm mode on this connection.
// After enabling, publish responses will include seq_no and ack fields.
func (c *Client) EnableConfirm() error {
	_, err := c.command(protocol.CmdConfirmSelect, []byte(`{}`))
	return err
}

func (c *Client) Publish(topic string, payload interface{}, headers map[string]string, opts *PublishOptions) (string, error) {
	if opts == nil {
		opts = &PublishOptions{}
	}
	data, err := json.Marshal(payload)
	if err != nil {
		return "", fmt.Errorf("marshal payload: %w", err)
	}

	body, _ := json.Marshal(map[string]interface{}{
		"topic":    topic,
		"payload":  json.RawMessage(data),
		"headers":  headers,
		"delay":    int64(opts.Delay),
		"ttl":      int64(opts.TTL),
		"priority": opts.Priority,
	})

	idBytes, err := c.command(protocol.CmdPublish, body)
	if err != nil {
		return "", err
	}

	var result struct {
		ID string `json:"id"`
	}
	json.Unmarshal(idBytes, &result)
	return result.ID, nil
}

// PublishTopic sends a message to a pub/sub topic.
func (c *Client) PublishTopic(topic string, payload interface{}, headers map[string]string, opts *PublishOptions) (string, error) {
	if opts == nil {
		opts = &PublishOptions{}
	}
	data, err := json.Marshal(payload)
	if err != nil {
		return "", fmt.Errorf("marshal payload: %w", err)
	}

	body, _ := json.Marshal(map[string]interface{}{
		"topic":    topic,
		"payload":  json.RawMessage(data),
		"headers":  headers,
		"priority": opts.Priority,
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

// ExchangeDeclare creates or asserts an exchange on the server.
func (c *Client) ExchangeDeclare(name, typ string, durable bool) error {
	body, _ := json.Marshal(map[string]interface{}{
		"name":    name,
		"type":    typ,
		"durable": durable,
	})
	_, err := c.command(protocol.CmdExchangeDeclare, body)
	return err
}

// ExchangeDelete removes an exchange from the server.
func (c *Client) ExchangeDelete(name string) error {
	body, _ := json.Marshal(map[string]string{"name": name})
	_, err := c.command(protocol.CmdExchangeDelete, body)
	return err
}

// BindQueue binds a queue to an exchange with the given binding key.
func (c *Client) BindQueue(exchange, queue, bindingKey string, headers map[string]string, matchAll bool) error {
	body, _ := json.Marshal(map[string]interface{}{
		"exchange":    exchange,
		"queue":       queue,
		"binding_key": bindingKey,
		"headers":     headers,
		"match_all":   matchAll,
	})
	_, err := c.command(protocol.CmdBindQueue, body)
	return err
}

// UnbindQueue removes a binding between a queue and an exchange.
func (c *Client) UnbindQueue(exchange, queue, bindingKey string) error {
	body, _ := json.Marshal(map[string]string{
		"exchange":    exchange,
		"queue":       queue,
		"binding_key": bindingKey,
	})
	_, err := c.command(protocol.CmdUnbindQueue, body)
	return err
}

// PublishToExchange publishes a message to an exchange with a routing key.
func (c *Client) PublishToExchange(exchange, routingKey string, payload interface{}, headers map[string]string, opts *PublishOptions) (string, error) {
	if opts == nil {
		opts = &PublishOptions{}
	}
	data, err := json.Marshal(payload)
	if err != nil {
		return "", fmt.Errorf("marshal payload: %w", err)
	}

	body, _ := json.Marshal(map[string]interface{}{
		"exchange":    exchange,
		"routing_key": routingKey,
		"payload":     json.RawMessage(data),
		"headers":     headers,
		"priority":    opts.Priority,
	})

	resp, err := c.command(protocol.CmdPublishExchange, body)
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
	if frame.Command == protocol.StatusNotLeader {
		return nil, parseNotLeaderError(frame.Payload)
	}
	if frame.Command == protocol.StatusError {
		var errResp ServerError
		json.Unmarshal(frame.Payload, &errResp)
		return nil, &errResp
	}

	var msg Message
	if err := json.Unmarshal(frame.Payload, &msg); err != nil {
		return nil, fmt.Errorf("decode message: %w", err)
	}
	return &msg, nil
}

// Subscribe opens a pub/sub subscription. If durable is true, missed messages are replayed on reconnect.
func (c *Client) Subscribe(topic string, subscriberID string, durable bool) (<-chan *Message, error) {
	body, _ := json.Marshal(map[string]interface{}{
		"topic":   topic,
		"id":      subscriberID,
		"durable": durable,
	})

	frame, err := c.sendCommand(protocol.CmdConsume, body) // Uses consume command for pubsub too in TCP protocol
	if err != nil {
		return nil, err
	}

	if frame.Command != protocol.StatusOK {
		var errResp ServerError
		json.Unmarshal(frame.Payload, &errResp)
		return nil, &errResp
	}

	key := fmt.Sprintf("%s:%s", topic, subscriberID)
	ch := make(chan *Message, 100)

	c.subsMu.Lock()
	if c.subs == nil {
		c.subs = make(map[string]chan *Message)
	}
	c.subs[key] = ch
	c.subsMu.Unlock()

	return ch, nil
}

func (c *Client) dispatcher() {
	for {
		c.subsMu.Lock()
		if !c.active {
			c.subsMu.Unlock()
			return
		}
		c.subsMu.Unlock()

		frame, err := protocol.ReadFrame(c.reader)
		if err != nil {
			if !c.autoReconnect {
				return
			}

			// Try to reconnect
			time.Sleep(c.reconnectInterval)
			if err := c.Connect(); err != nil {
				continue
			}

			// Resubscribe all
			c.subsMu.RLock()
			for key := range c.subs {
				parts := strings.SplitN(key, ":", 2)
				if len(parts) != 2 {
					continue
				}
				body, _ := json.Marshal(map[string]interface{}{
					"topic":   parts[0],
					"id":      parts[1],
					"durable": true, // Assume durable for recovery
				})
				c.sendCommand(protocol.CmdConsume, body)
			}
			c.subsMu.RUnlock()
			continue
		}

		var msg Message
		if err := json.Unmarshal(frame.Payload, &msg); err == nil {
			// Find subscriber by topic (for simple pubsub) or subscriber_id (for work queues)
			// The protocol doesn't explicitly send subscriber_id in the frame yet, 
			// let's assume we match by topic for now or extract from payload if available.
			
			var meta struct {
				SubscriberID string `json:"subscriber_id"`
			}
			json.Unmarshal(frame.Payload, &meta)

			key := msg.Topic
			if meta.SubscriberID != "" {
				key = fmt.Sprintf("%s:%s", msg.Topic, meta.SubscriberID)
			} else {
				// Fallback to searching for any subscriber for this topic
				c.subsMu.RLock()
				for k := range c.subs {
					if strings.HasPrefix(k, msg.Topic+":") {
						key = k
						break
					}
				}
				c.subsMu.RUnlock()
			}

			c.subsMu.RLock()
			if subCh, ok := c.subs[key]; ok {
				select {
				case subCh <- &msg:
				default:
					// Buffer full, drop or handle?
				}
			}
			c.subsMu.RUnlock()
		}
	}
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

// ClusterStatusInfo returns the cluster status information.
func (c *Client) ClusterStatusInfo() (*ClusterStatus, error) {
	resp, err := c.command(protocol.CmdClusterStatus, []byte("{}"))
	if err != nil {
		return nil, err
	}
	var status ClusterStatus
	if err := json.Unmarshal(resp, &status); err != nil {
		return nil, err
	}
	return &status, nil
}

// SetPrefetch sets the prefetch limit for this client connection.
func (c *Client) SetPrefetch(count int) error {
	body, _ := json.Marshal(map[string]int{"count": count})
	_, err := c.command(protocol.CmdPrefetch, body)
	return err
}

// Health checks if the server is healthy.
func (c *Client) Health() error {
	return c.Ping()
}

// command sends a command and returns the parsed payload, or error if status != OK.
func (c *Client) command(cmd byte, payload []byte) ([]byte, error) {
	frame, err := c.sendCommand(cmd, payload)
	if err != nil {
		// Try to reconnect once if auto-reconnect is enabled and it's a connection error
		if c.autoReconnect {
			if connectErr := c.Connect(); connectErr == nil {
				frame, err = c.sendCommand(cmd, payload)
			}
		}

		if err != nil {
			return nil, err
		}
	}
	if frame.Command == protocol.StatusNotLeader {
		return nil, parseNotLeaderError(frame.Payload)
	}
	if frame.Command == protocol.StatusError {
		var errResp ServerError
		json.Unmarshal(frame.Payload, &errResp)
		return nil, &errResp
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

func parseNotLeaderError(payload []byte) *NotLeaderError {
	var nle NotLeaderError
	json.Unmarshal(payload, &nle)
	return &nle
}

// ---------------------------------------------------------------------------
// Cache / KV Store (HTTP-based)
// ---------------------------------------------------------------------------

// CacheEntry represents a cached key-value entry.
type CacheEntry struct {
	Key       string          `json:"key"`
	Value     json.RawMessage `json:"value"`
	TTL       int64           `json:"ttl"` // remaining TTL in ms, -1 = no expiry
	CreatedAt int64           `json:"created_at"`
}

// CacheStats represents cache statistics.
type CacheStats struct {
	Enabled bool `json:"enabled"`
	Stats   struct {
		KeyCount   int   `json:"key_count"`
		MemoryUsed int64 `json:"memory_used"`
		Hits       int64 `json:"hits"`
		Misses     int64 `json:"misses"`
		Sets       int64 `json:"sets"`
		Deletes    int64 `json:"deletes"`
		Expired    int64 `json:"expired"`
	} `json:"stats"`
}

// httpAddr returns the HTTP admin address derived from the TCP address.
// Convention: HTTP port = TCP port - 1 (e.g., TCP 9091 -> HTTP 9090).
func (c *Client) httpAddr() string {
	host, portStr, err := net.SplitHostPort(c.addr)
	if err != nil {
		return "http://localhost:9090"
	}
	var port int
	fmt.Sscanf(portStr, "%d", &port)
	return fmt.Sprintf("http://%s:%d", host, port-1)
}

// httpGet performs an HTTP GET request to the admin API.
func (c *Client) httpGet(path string) ([]byte, error) {
	url := c.httpAddr() + path
	req, err := newHTTPRequest("GET", url, nil)
	if err != nil {
		return nil, err
	}
	if c.apiKey != "" {
		req.Header.Set("X-API-Key", c.apiKey)
	}
	return doHTTPRequest(req, c.timeout)
}

// httpPost performs an HTTP POST request to the admin API.
func (c *Client) httpPost(path string, body interface{}) ([]byte, error) {
	data, _ := json.Marshal(body)
	url := c.httpAddr() + path
	req, err := newHTTPRequest("POST", url, data)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	if c.apiKey != "" {
		req.Header.Set("X-API-Key", c.apiKey)
	}
	return doHTTPRequest(req, c.timeout)
}

// CacheGet retrieves a value from the cache by key.
func (c *Client) CacheGet(key string) (*CacheEntry, error) {
	data, err := c.httpGet("/cache/get?key=" + key)
	if err != nil {
		return nil, err
	}
	var entry CacheEntry
	if err := json.Unmarshal(data, &entry); err != nil {
		return nil, err
	}
	return &entry, nil
}

// CacheSet stores a key-value pair in the cache with optional TTL in milliseconds.
func (c *Client) CacheSet(key string, value interface{}, ttlMs int64) error {
	_, err := c.httpPost("/cache/set", map[string]interface{}{
		"key":   key,
		"value": value,
		"ttl":   ttlMs,
	})
	return err
}

// CacheDel removes a key from the cache.
func (c *Client) CacheDel(key string) (bool, error) {
	data, err := c.httpPost("/cache/del", map[string]string{"key": key})
	if err != nil {
		return false, err
	}
	var result struct {
		Deleted bool `json:"deleted"`
	}
	json.Unmarshal(data, &result)
	return result.Deleted, nil
}

// CacheKeys returns all keys matching the given pattern.
func (c *Client) CacheKeys(pattern string) ([]string, error) {
	data, err := c.httpGet("/cache/keys?pattern=" + pattern)
	if err != nil {
		return nil, err
	}
	var result struct {
		Keys []string `json:"keys"`
	}
	json.Unmarshal(data, &result)
	return result.Keys, nil
}

// CacheExists checks if a key exists in the cache.
func (c *Client) CacheExists(key string) (bool, error) {
	data, err := c.httpGet("/cache/exists?key=" + key)
	if err != nil {
		return false, err
	}
	var result struct {
		Exists bool `json:"exists"`
	}
	json.Unmarshal(data, &result)
	return result.Exists, nil
}

// CacheExpire sets a new TTL on a key (milliseconds). Use 0 to remove TTL.
func (c *Client) CacheExpire(key string, ttlMs int64) error {
	_, err := c.httpPost("/cache/expire", map[string]interface{}{
		"key": key,
		"ttl": ttlMs,
	})
	return err
}

// CacheIncr atomically increments a numeric value and returns the new value.
func (c *Client) CacheIncr(key string, delta int64) (int64, error) {
	data, err := c.httpPost("/cache/incr", map[string]interface{}{
		"key":   key,
		"delta": delta,
	})
	if err != nil {
		return 0, err
	}
	var result struct {
		Value int64 `json:"value"`
	}
	json.Unmarshal(data, &result)
	return result.Value, nil
}

// CacheFlush removes all entries from the cache.
func (c *Client) CacheFlush() (int, error) {
	data, err := c.httpPost("/cache/flush", nil)
	if err != nil {
		return 0, err
	}
	var result struct {
		Removed int `json:"removed"`
	}
	json.Unmarshal(data, &result)
	return result.Removed, nil
}

// CacheGetStats returns cache statistics.
func (c *Client) CacheGetStats() (*CacheStats, error) {
	data, err := c.httpGet("/cache/stats")
	if err != nil {
		return nil, err
	}
	var stats CacheStats
	json.Unmarshal(data, &stats)
	return &stats, nil
}
