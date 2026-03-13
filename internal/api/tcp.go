package api

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"sync"

	"github.com/boltq/boltq/internal/broker"
	"github.com/boltq/boltq/internal/metrics"
	"github.com/boltq/boltq/pkg/protocol"
)

// TCPServer provides the binary TCP protocol for the message broker.
type TCPServer struct {
	broker   *broker.Broker
	metrics  *metrics.Metrics
	apiKey   string
	listener net.Listener
	wg       sync.WaitGroup
	quit     chan struct{}
}

// NewTCPServer creates a new TCP server.
func NewTCPServer(b *broker.Broker, m *metrics.Metrics, apiKey string) *TCPServer {
	return &TCPServer{
		broker:  b,
		metrics: m,
		apiKey:  apiKey,
		quit:    make(chan struct{}),
	}
}

// Start begins listening for TCP connections on the given address.
func (s *TCPServer) Start(addr string) error {
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return fmt.Errorf("tcp listen: %w", err)
	}
	s.listener = ln
	log.Printf("[tcp] listening on %s", addr)

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
			s.writeError(writer, "authentication required, send AUTH first")
			writer.Flush()
			return
		}

		resp := s.dispatch(frame, &authenticated)
		if err := protocol.WriteFrame(writer, resp); err != nil {
			log.Printf("[tcp] write error: %v", err)
			return
		}
		if err := writer.Flush(); err != nil {
			log.Printf("[tcp] flush error: %v", err)
			return
		}
	}
}

func (s *TCPServer) dispatch(frame protocol.Frame, authenticated *bool) protocol.Frame {
	switch frame.Command {
	case protocol.CmdAuth:
		return s.handleAuth(frame, authenticated)
	case protocol.CmdPing:
		return s.handlePing()
	case protocol.CmdPublish:
		return s.handlePublishTCP(frame)
	case protocol.CmdPublishTopic:
		return s.handlePublishTopicTCP(frame)
	case protocol.CmdConsume:
		return s.handleConsumeTCP(frame)
	case protocol.CmdAck:
		return s.handleAckTCP(frame)
	case protocol.CmdNack:
		return s.handleNackTCP(frame)
	case protocol.CmdStats:
		return s.handleStatsTCP()
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
	if err := s.broker.Publish(req.Topic, msg); err != nil {
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
	if err := s.broker.PublishTopic(req.Topic, msg); err != nil {
		return s.errorFrame(err.Error())
	}

	s.metrics.IncPublished()
	data, _ := json.Marshal(publishResponse{ID: msg.ID, Topic: msg.Topic})
	return s.okFrame(data)
}

// --- CONSUME ---

type consumeRequest struct {
	Topic string `json:"topic"`
}

func (s *TCPServer) handleConsumeTCP(frame protocol.Frame) protocol.Frame {
	var req consumeRequest
	if err := json.Unmarshal(frame.Payload, &req); err != nil {
		return s.errorFrame("invalid json: " + err.Error())
	}
	if req.Topic == "" {
		return s.errorFrame("topic is required")
	}

	msg := s.broker.TryConsume(req.Topic)
	if msg == nil {
		return protocol.Frame{Command: protocol.StatusEmpty, Payload: nil}
	}

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

func (s *TCPServer) handleAckTCP(frame protocol.Frame) protocol.Frame {
	var req tcpAckRequest
	if err := json.Unmarshal(frame.Payload, &req); err != nil {
		return s.errorFrame("invalid json")
	}
	if req.ID == "" {
		return s.errorFrame("id is required")
	}

	if err := s.broker.Ack(req.ID); err != nil {
		return s.errorFrame(err.Error())
	}

	s.metrics.IncAcked()
	return s.okFrame([]byte(`{"status":"acked"}`))
}

// --- NACK ---

func (s *TCPServer) handleNackTCP(frame protocol.Frame) protocol.Frame {
	var req tcpAckRequest
	if err := json.Unmarshal(frame.Payload, &req); err != nil {
		return s.errorFrame("invalid json")
	}
	if req.ID == "" {
		return s.errorFrame("id is required")
	}

	if err := s.broker.Nack(req.ID); err != nil {
		return s.errorFrame(err.Error())
	}

	s.metrics.IncNacked()
	return s.okFrame([]byte(`{"status":"nacked"}`))
}

// --- STATS ---

func (s *TCPServer) handleStatsTCP() protocol.Frame {
	data, _ := json.Marshal(s.broker.Stats())
	return s.okFrame(data)
}

// --- Helpers ---

func (s *TCPServer) okFrame(payload []byte) protocol.Frame {
	return protocol.Frame{Command: protocol.StatusOK, Payload: payload}
}

func (s *TCPServer) errorFrame(msg string) protocol.Frame {
	data, _ := json.Marshal(map[string]string{"error": msg})
	return protocol.Frame{Command: protocol.StatusError, Payload: data}
}

func (s *TCPServer) writeError(w *bufio.Writer, msg string) {
	protocol.WriteFrame(w, s.errorFrame(msg))
}
