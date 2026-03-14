package api

import (
	"bufio"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"sync"

	"github.com/boltq/boltq/internal/broker"
	"github.com/boltq/boltq/internal/cluster"
	"github.com/boltq/boltq/internal/config"
	"github.com/boltq/boltq/internal/metrics"
	"github.com/boltq/boltq/pkg/protocol"
)

// TCPServer provides the binary TCP protocol for the message broker.
type TCPServer struct {
	broker      broker.BrokerIface
	metrics     *metrics.Metrics
	apiKey      string
	tlsConfig   config.TLSConfig
	clusterNode *cluster.RaftNode
	listener    net.Listener
	wg          sync.WaitGroup
	quit        chan struct{}
}

// NewTCPServer creates a new TCP server.
func NewTCPServer(b broker.BrokerIface, m *metrics.Metrics, cfg config.ServerConfig, apiKey string) *TCPServer {
	return &TCPServer{
		broker:    b,
		metrics:   m,
		apiKey:    apiKey,
		tlsConfig: cfg.TLS,
		quit:      make(chan struct{}),
	}
}

// Start begins listening for TCP connections on the given address.
func (s *TCPServer) Start(addr string) error {
	var ln net.Listener
	var err error

	if s.tlsConfig.Enabled {
		cert, err2 := tls.LoadX509KeyPair(s.tlsConfig.CertFile, s.tlsConfig.KeyFile)
		if err2 != nil {
			return fmt.Errorf("load tls keys: %w", err2)
		}
		tlsCfg := &tls.Config{Certificates: []tls.Certificate{cert}}
		ln, err = tls.Listen("tcp", addr, tlsCfg)
		log.Printf("[tcp] listening on %s (TLS)", addr)
	} else {
		ln, err = net.Listen("tcp", addr)
		log.Printf("[tcp] listening on %s", addr)
	}

	if err != nil {
		return fmt.Errorf("tcp listen: %w", err)
	}
	s.listener = ln

	go s.acceptLoop()
	return nil
}

// Shutdown gracefully stops the TCP server.
func (s *TCPServer) Shutdown() {
	close(s.quit)
	if s.listener != nil {
		s.listener.Close()
	}
	s.wg.Wait()
}

func (s *TCPServer) acceptLoop() {
	for {
		conn, err := s.listener.Accept()
		if err != nil {
			select {
			case <-s.quit:
				return
			default:
				log.Printf("[tcp] accept error: %v", err)
				continue
			}
		}
		s.wg.Add(1)
		go s.handleConn(conn)
	}
}

func (s *TCPServer) handleConn(conn net.Conn) {
	defer s.wg.Done()
	defer conn.Close()

	reader := bufio.NewReaderSize(conn, 64*1024)
	writer := bufio.NewWriterSize(conn, 64*1024)
	authenticated := s.apiKey == "" // auto-authenticated if no key configured
	prefetch := 0                  // 0 means no limit
	unackedCount := 0

	var writeMu sync.Mutex
	for {
		select {
		case <-s.quit:
			return
		default:
		}

		frame, err := protocol.ReadFrame(reader)
		if err != nil {
			if err != io.EOF {
				log.Printf("[tcp] read error: %v", err)
			}
			return
		}

		// AUTH must be the first command if apiKey is configured.
		if !authenticated && frame.Command != protocol.CmdAuth {
			writeMu.Lock()
			s.writeError(writer, "authentication required, send AUTH first")
			writer.Flush()
			writeMu.Unlock()
			return
		}

		resp := s.dispatch(frame, &authenticated, &prefetch, &unackedCount, writer, &writeMu)

		writeMu.Lock()
		if err := protocol.WriteFrame(writer, resp); err != nil {
			log.Printf("[tcp] write error: %v", err)
			writeMu.Unlock()
			return
		}
		if err := writer.Flush(); err != nil {
			log.Printf("[tcp] flush error: %v", err)
			writeMu.Unlock()
			return
		}
		writeMu.Unlock()
	}
}

func (s *TCPServer) dispatch(frame protocol.Frame, authenticated *bool, prefetch *int, unackedCount *int, writer *bufio.Writer, writeMu *sync.Mutex) protocol.Frame {
	switch frame.Command {
	case protocol.CmdAuth:
		return s.handleAuth(frame, authenticated)
	case protocol.CmdPing:
		return s.handlePing()
	case protocol.CmdPrefetch:
		return s.handlePrefetchTCP(frame, prefetch)
	case protocol.CmdPublish:
		return s.handlePublishTCP(frame)
	case protocol.CmdPublishTopic:
		return s.handlePublishTopicTCP(frame)
	case protocol.CmdConsume:
		return s.handleConsumeTCP(frame, prefetch, unackedCount, writer, writeMu)
	case protocol.CmdAck:
		return s.handleAckTCP(frame, unackedCount)
	case protocol.CmdNack:
		return s.handleNackTCP(frame, unackedCount)
	case protocol.CmdStats:
		return s.handleStatsTCP()
	case protocol.CmdClusterJoin:
		return s.handleClusterJoinTCP(frame)
	case protocol.CmdClusterLeave:
		return s.handleClusterLeaveTCP(frame)
	case protocol.CmdClusterStatus:
		return s.handleClusterStatusTCP()
	default:
		return s.errorFrame(fmt.Sprintf("unknown command: 0x%02x", frame.Command))
	}
}

// --- AUTH ---

type authRequest struct {
	APIKey string `json:"api_key"`
}

func (s *TCPServer) handleAuth(frame protocol.Frame, authenticated *bool) protocol.Frame {
	if s.apiKey == "" {
		*authenticated = true
		return s.okFrame([]byte(`{"status":"ok"}`))
	}

	var req authRequest
	if err := json.Unmarshal(frame.Payload, &req); err != nil {
		return s.errorFrame("invalid auth payload")
	}

	if req.APIKey != s.apiKey {
		return s.errorFrame("unauthorized")
	}

	*authenticated = true
	return s.okFrame([]byte(`{"status":"ok"}`))
}

// --- PING ---

func (s *TCPServer) handlePing() protocol.Frame {
	return s.okFrame([]byte(`{"status":"pong"}`))
}

// --- PREFETCH ---

func (s *TCPServer) handlePrefetchTCP(frame protocol.Frame, prefetch *int) protocol.Frame {
	var req struct {
		Count int `json:"count"`
	}
	if err := json.Unmarshal(frame.Payload, &req); err != nil {
		return s.errorFrame("invalid prefetch payload")
	}
	*prefetch = req.Count
	return s.okFrame([]byte(`{"status":"ok"}`))
}

// --- Messaging types ---

type publishRequest struct {
	Topic   string            `json:"topic"`
	Payload json.RawMessage   `json:"payload"`
	Headers map[string]string `json:"headers"`
	Delay   int64             `json:"delay"`
	TTL     int64             `json:"ttl"`
}

type publishResponse struct {
	ID    string `json:"id"`
	Topic string `json:"topic"`
}

type consumeResponse struct {
	ID        string            `json:"id"`
	Topic     string            `json:"topic"`
	Payload   json.RawMessage   `json:"payload"`
	Headers   map[string]string `json:"headers,omitempty"`
	Timestamp int64             `json:"timestamp"`
	Retry     int               `json:"retry"`
}

func newMessage(topic string, payload json.RawMessage, headers map[string]string) *protocol.Message {
	return protocol.NewMessage(topic, payload, headers)
}

// --- PUBLISH ---

func (s *TCPServer) handlePublishTCP(frame protocol.Frame) protocol.Frame {
	var req publishRequest
	if err := json.Unmarshal(frame.Payload, &req); err != nil {
		return s.errorFrame("invalid json: " + err.Error())
	}
	if req.Topic == "" {
		return s.errorFrame("topic is required")
	}

	msg := newMessage(req.Topic, req.Payload, req.Headers)
	msg.Delay = req.Delay
	msg.TTL = req.TTL

	if err := s.broker.Publish(req.Topic, msg); err != nil {
		if nle, ok := cluster.IsNotLeaderError(err); ok {
			return s.notLeaderFrame(nle)
		}
		return s.errorFrame(err.Error())
	}

	s.metrics.IncPublished()
	data, _ := json.Marshal(publishResponse{ID: msg.ID, Topic: msg.Topic})
	return s.okFrame(data)
}

// --- PUBLISH TOPIC ---

func (s *TCPServer) handlePublishTopicTCP(frame protocol.Frame) protocol.Frame {
	var req publishRequest
	if err := json.Unmarshal(frame.Payload, &req); err != nil {
		return s.errorFrame("invalid json: " + err.Error())
	}
	if req.Topic == "" {
		return s.errorFrame("topic is required")
	}

	msg := newMessage(req.Topic, req.Payload, req.Headers)
	msg.Delay = req.Delay
	msg.TTL = req.TTL

	if err := s.broker.PublishTopic(req.Topic, msg); err != nil {
		if nle, ok := cluster.IsNotLeaderError(err); ok {
			return s.notLeaderFrame(nle)
		}
		return s.errorFrame(err.Error())
	}

	s.metrics.IncPublished()
	data, _ := json.Marshal(publishResponse{ID: msg.ID, Topic: msg.Topic})
	return s.okFrame(data)
}

// --- CONSUME ---

type consumeRequest struct {
	Topic   string `json:"topic"`
	ID      string `json:"id"`
	Durable bool   `json:"durable"`
}

func (s *TCPServer) handleConsumeTCP(frame protocol.Frame, prefetch *int, unackedCount *int, writer *bufio.Writer, writeMu *sync.Mutex) protocol.Frame {
	var req consumeRequest
	if err := json.Unmarshal(frame.Payload, &req); err != nil {
		return s.errorFrame("invalid json: " + err.Error())
	}
	if req.Topic == "" {
		return s.errorFrame("topic is required")
	}

	// If it's a pub/sub subscription (has ID), bypass prefetch for now.
	// Prefetch is mainly for Work Queues (Competing Consumers).
	isPubSub := req.ID != ""

	if !isPubSub && *prefetch > 0 && *unackedCount >= *prefetch {
		log.Printf("[tcp] prefetch limit reached: unacked=%d prefetch=%d", *unackedCount, *prefetch)
		return s.errorFrame("prefetch limit reached")
	}

	if isPubSub {
		ch := s.broker.Subscribe(req.Topic, req.ID, 100, req.Durable)
		if ch == nil {
			return s.errorFrame("failed to subscribe")
		}

		// Now stream messages from the channel until the connection closes
		go func() {
			defer s.broker.Unsubscribe(req.Topic, req.ID)
			for msg := range ch {
				// Wrap message to include subscriber_id and ensure payload is handled as RawMessage
				// to avoid base64 encoding of JSON payloads.
				type streamingResponse struct {
					ID            string            `json:"id"`
					Topic         string            `json:"topic"`
					Payload       json.RawMessage   `json:"payload"`
					Headers       map[string]string `json:"headers,omitempty"`
					Timestamp     int64             `json:"timestamp"`
					SubscriberID  string            `json:"subscriber_id"`
				}

				resp := streamingResponse{
					ID:            msg.ID,
					Topic:         msg.Topic,
					Payload:       json.RawMessage(msg.Payload),
					Headers:       msg.Headers,
					Timestamp:     msg.Timestamp,
					SubscriberID:  req.ID,
				}

				data, _ := json.Marshal(resp)
				frame := protocol.Frame{Command: protocol.StatusOK, Payload: data}
				writeMu.Lock()
				if err := protocol.WriteFrame(writer, frame); err != nil {
					writeMu.Unlock()
					return // Connection probably closed
				}
				writer.Flush()
				writeMu.Unlock()
			}
		}()
		return s.okFrame([]byte(`{"status":"subscribed"}`))
	}

	msg := s.broker.TryConsume(req.Topic)
	if msg == nil {
		return protocol.Frame{Command: protocol.StatusEmpty, Payload: nil}
	}

	*unackedCount++
	s.metrics.IncConsumed()
	data, _ := json.Marshal(consumeResponse{
		ID:        msg.ID,
		Topic:     msg.Topic,
		Payload:   msg.Payload,
		Headers:   msg.Headers,
		Timestamp: msg.Timestamp,
		Retry:     msg.Retry,
	})
	return s.okFrame(data)
}

// --- ACK ---

type tcpAckRequest struct {
	ID string `json:"id"`
}

func (s *TCPServer) handleAckTCP(frame protocol.Frame, unackedCount *int) protocol.Frame {
	var req tcpAckRequest
	if err := json.Unmarshal(frame.Payload, &req); err != nil {
		return s.errorFrame("invalid json")
	}
	if req.ID == "" {
		return s.errorFrame("id is required")
	}

	if err := s.broker.Ack(req.ID); err != nil {
		if nle, ok := cluster.IsNotLeaderError(err); ok {
			return s.notLeaderFrame(nle)
		}
		return s.errorFrame(err.Error())
	}

	if *unackedCount > 0 {
		*unackedCount--
	}
	s.metrics.IncAcked()
	return s.okFrame([]byte(`{"status":"acked"}`))
}

// --- NACK ---

func (s *TCPServer) handleNackTCP(frame protocol.Frame, unackedCount *int) protocol.Frame {
	var req tcpAckRequest
	if err := json.Unmarshal(frame.Payload, &req); err != nil {
		return s.errorFrame("invalid json")
	}
	if req.ID == "" {
		return s.errorFrame("id is required")
	}

	if err := s.broker.Nack(req.ID); err != nil {
		if nle, ok := cluster.IsNotLeaderError(err); ok {
			return s.notLeaderFrame(nle)
		}
		return s.errorFrame(err.Error())
	}

	if *unackedCount > 0 {
		*unackedCount--
	}
	s.metrics.IncNacked()
	return s.okFrame([]byte(`{"status":"nacked"}`))
}

// --- STATS ---

func (s *TCPServer) handleStatsTCP() protocol.Frame {
	data, _ := json.Marshal(s.broker.Stats())
	return s.okFrame(data)
}

// --- CLUSTER JOIN ---

func (s *TCPServer) handleClusterJoinTCP(frame protocol.Frame) protocol.Frame {
	if s.clusterNode == nil {
		return s.errorFrame("clustering is not enabled")
	}
	var req struct {
		NodeID string `json:"node_id"`
		Addr   string `json:"addr"`
	}
	if err := json.Unmarshal(frame.Payload, &req); err != nil {
		return s.errorFrame("invalid json")
	}
	if req.NodeID == "" || req.Addr == "" {
		return s.errorFrame("node_id and addr are required")
	}
	if err := s.clusterNode.Join(req.NodeID, req.Addr); err != nil {
		return s.errorFrame(err.Error())
	}
	data, _ := json.Marshal(map[string]string{"status": "joined", "node_id": req.NodeID})
	return s.okFrame(data)
}

// --- CLUSTER LEAVE ---

func (s *TCPServer) handleClusterLeaveTCP(frame protocol.Frame) protocol.Frame {
	if s.clusterNode == nil {
		return s.errorFrame("clustering is not enabled")
	}
	var req struct {
		NodeID string `json:"node_id"`
	}
	if err := json.Unmarshal(frame.Payload, &req); err != nil {
		return s.errorFrame("invalid json")
	}
	if req.NodeID == "" {
		return s.errorFrame("node_id is required")
	}
	if err := s.clusterNode.Leave(req.NodeID); err != nil {
		return s.errorFrame(err.Error())
	}
	data, _ := json.Marshal(map[string]string{"status": "removed", "node_id": req.NodeID})
	return s.okFrame(data)
}

// --- CLUSTER STATUS ---

func (s *TCPServer) handleClusterStatusTCP() protocol.Frame {
	if s.clusterNode == nil {
		data, _ := json.Marshal(map[string]interface{}{"enabled": false})
		return s.okFrame(data)
	}
	status := s.clusterNode.Status()
	data, _ := json.Marshal(map[string]interface{}{"enabled": true, "cluster": status})
	return s.okFrame(data)
}

// SetClusterNode sets the Raft node for cluster management operations.
func (s *TCPServer) SetClusterNode(node *cluster.RaftNode) {
	s.clusterNode = node
}

// --- Helpers ---

func (s *TCPServer) okFrame(payload []byte) protocol.Frame {
	return protocol.Frame{Command: protocol.StatusOK, Payload: payload}
}

func (s *TCPServer) errorFrame(msg string) protocol.Frame {
	data, _ := json.Marshal(map[string]string{"error": msg})
	return protocol.Frame{Command: protocol.StatusError, Payload: data}
}

func (s *TCPServer) notLeaderFrame(nle *cluster.NotLeaderError) protocol.Frame {
	data, _ := json.Marshal(map[string]string{
		"error":     "not leader",
		"leader":    nle.Leader,
		"leader_id": nle.LeaderID,
	})
	return protocol.Frame{Command: protocol.StatusNotLeader, Payload: data}
}

func (s *TCPServer) writeError(w *bufio.Writer, msg string) {
	protocol.WriteFrame(w, s.errorFrame(msg))
}
