package api

import (
	"bufio"
	"crypto/sha1"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/boltq/boltq/internal/broker"
	"github.com/boltq/boltq/internal/cluster"
	"github.com/boltq/boltq/pkg/protocol"
)

// WebSocket opcodes (RFC 6455 §5.2).
const (
	wsOpText   = 1
	wsOpBinary = 2
	wsOpClose  = 8
	wsOpPing   = 9
	wsOpPong   = 10
)

// wsConn wraps a hijacked net.Conn with WebSocket frame read/write.
type wsConn struct {
	conn   net.Conn
	reader *bufio.Reader
	mu     sync.Mutex // serialise writes
}

// --- WebSocket frame reader / writer (RFC 6455) ---

func (ws *wsConn) readMessage() (opcode int, payload []byte, err error) {
	for {
		op, data, err := ws.readFrame()
		if err != nil {
			return 0, nil, err
		}
		switch op {
		case wsOpPing:
			ws.writeFrame(wsOpPong, data)
			continue
		case wsOpPong:
			continue
		case wsOpClose:
			ws.writeFrame(wsOpClose, data)
			return wsOpClose, nil, io.EOF
		default:
			return op, data, nil
		}
	}
}

func (ws *wsConn) readFrame() (opcode int, payload []byte, err error) {
	// Read first 2 bytes: FIN+opcode, MASK+length.
	hdr := make([]byte, 2)
	if _, err := io.ReadFull(ws.reader, hdr); err != nil {
		return 0, nil, err
	}

	opcode = int(hdr[0] & 0x0F)
	masked := hdr[1]&0x80 != 0
	length := uint64(hdr[1] & 0x7F)

	switch {
	case length == 126:
		ext := make([]byte, 2)
		if _, err := io.ReadFull(ws.reader, ext); err != nil {
			return 0, nil, err
		}
		length = uint64(binary.BigEndian.Uint16(ext))
	case length == 127:
		ext := make([]byte, 8)
		if _, err := io.ReadFull(ws.reader, ext); err != nil {
			return 0, nil, err
		}
		length = binary.BigEndian.Uint64(ext)
	}

	if length > 4<<20 { // 4MB max like TCP protocol
		return 0, nil, fmt.Errorf("websocket frame too large: %d", length)
	}

	var maskKey [4]byte
	if masked {
		if _, err := io.ReadFull(ws.reader, maskKey[:]); err != nil {
			return 0, nil, err
		}
	}

	payload = make([]byte, length)
	if length > 0 {
		if _, err := io.ReadFull(ws.reader, payload); err != nil {
			return 0, nil, err
		}
		if masked {
			for i := range payload {
				payload[i] ^= maskKey[i%4]
			}
		}
	}

	return opcode, payload, nil
}

func (ws *wsConn) writeFrame(opcode int, payload []byte) error {
	ws.mu.Lock()
	defer ws.mu.Unlock()

	length := len(payload)
	// Server frames are never masked.
	var hdr []byte
	switch {
	case length <= 125:
		hdr = []byte{byte(0x80 | opcode), byte(length)}
	case length <= 65535:
		hdr = make([]byte, 4)
		hdr[0] = byte(0x80 | opcode)
		hdr[1] = 126
		binary.BigEndian.PutUint16(hdr[2:4], uint16(length))
	default:
		hdr = make([]byte, 10)
		hdr[0] = byte(0x80 | opcode)
		hdr[1] = 127
		binary.BigEndian.PutUint64(hdr[2:10], uint64(length))
	}

	if _, err := ws.conn.Write(hdr); err != nil {
		return err
	}
	if length > 0 {
		if _, err := ws.conn.Write(payload); err != nil {
			return err
		}
	}
	return nil
}

func (ws *wsConn) writeJSON(v interface{}) error {
	data, err := json.Marshal(v)
	if err != nil {
		return err
	}
	return ws.writeFrame(wsOpText, data)
}

func (ws *wsConn) close() {
	ws.conn.Close()
}

// --- WebSocket HTTP upgrade ---

const wsMagicGUID = "258EAFA5-E914-47DA-95CA-5AB5DC085B11"

func upgradeWebSocket(w http.ResponseWriter, r *http.Request) (*wsConn, error) {
	if r.Header.Get("Upgrade") != "websocket" {
		http.Error(w, "expected websocket upgrade", http.StatusBadRequest)
		return nil, fmt.Errorf("not a websocket request")
	}

	key := r.Header.Get("Sec-WebSocket-Key")
	if key == "" {
		http.Error(w, "missing Sec-WebSocket-Key", http.StatusBadRequest)
		return nil, fmt.Errorf("missing key")
	}

	// Compute accept key per RFC 6455 §4.2.2.
	h := sha1.New()
	h.Write([]byte(key + wsMagicGUID))
	acceptKey := base64.StdEncoding.EncodeToString(h.Sum(nil))

	// Hijack the connection.
	hj, ok := w.(http.Hijacker)
	if !ok {
		http.Error(w, "server doesn't support hijacking", http.StatusInternalServerError)
		return nil, fmt.Errorf("hijack not supported")
	}
	conn, bufrw, err := hj.Hijack()
	if err != nil {
		return nil, fmt.Errorf("hijack: %w", err)
	}

	// Write HTTP 101 Switching Protocols.
	resp := "HTTP/1.1 101 Switching Protocols\r\n" +
		"Upgrade: websocket\r\n" +
		"Connection: Upgrade\r\n" +
		"Sec-WebSocket-Accept: " + acceptKey + "\r\n\r\n"
	if _, err := bufrw.WriteString(resp); err != nil {
		conn.Close()
		return nil, err
	}
	if err := bufrw.Flush(); err != nil {
		conn.Close()
		return nil, err
	}

	return &wsConn{conn: conn, reader: bufrw.Reader}, nil
}

// --- WebSocket JSON protocol ---

// wsRequest is the incoming JSON message from a WebSocket client.
type wsRequest struct {
	Cmd        string            `json:"cmd"`
	Topic      string            `json:"topic,omitempty"`
	Payload    json.RawMessage   `json:"payload,omitempty"`
	Headers    map[string]string `json:"headers,omitempty"`
	ID         string            `json:"id,omitempty"`
	Delay      int64             `json:"delay,omitempty"`
	TTL        int64             `json:"ttl,omitempty"`
	Priority   int               `json:"priority,omitempty"`
	Durable    bool              `json:"durable,omitempty"`
	Count      int               `json:"count,omitempty"`
	Exchange   string            `json:"exchange,omitempty"`
	Type       string            `json:"type,omitempty"`
	Queue      string            `json:"queue,omitempty"`
	BindingKey string            `json:"binding_key,omitempty"`
	MatchAll   bool              `json:"match_all,omitempty"`
	RoutingKey string            `json:"routing_key,omitempty"`
	APIKey     string            `json:"api_key,omitempty"`
}

// activeSub tracks a WebSocket subscription for cleanup on disconnect.
type activeSub struct {
	topic, subID string
}

// wsResponse is the outgoing JSON message to a WebSocket client.
type wsResponse struct {
	Status string      `json:"status"`
	Error  string      `json:"error,omitempty"`
	Data   interface{} `json:"data,omitempty"`
}

// handleWebSocket is the HTTP handler that upgrades to WebSocket and dispatches commands.
func (s *HTTPServer) handleWebSocket(w http.ResponseWriter, r *http.Request) {
	ws, err := upgradeWebSocket(w, r)
	if err != nil {
		log.Printf("[ws] upgrade error: %v", err)
		return
	}
	defer ws.close()

	authenticated := s.apiKey == ""
	confirmMode := false
	nextSeqNo := uint64(0)
	prefetch := 0
	unackedCount := 0

	// Track active subscriptions so we can clean up on disconnect.
	var activeSubs []activeSub
	var subsMu sync.Mutex

	defer func() {
		subsMu.Lock()
		for _, sub := range activeSubs {
			s.broker.Unsubscribe(sub.topic, sub.subID)
		}
		subsMu.Unlock()
	}()

	for {
		_, data, err := ws.readMessage()
		if err != nil {
			if err != io.EOF {
				log.Printf("[ws] read error: %v", err)
			}
			return
		}

		// Validate UTF-8.
		if !utf8.Valid(data) {
			ws.writeJSON(wsResponse{Status: "error", Error: "invalid utf-8"})
			continue
		}

		var req wsRequest
		if err := json.Unmarshal(data, &req); err != nil {
			ws.writeJSON(wsResponse{Status: "error", Error: "invalid json: " + err.Error()})
			continue
		}

		// Auth gate.
		if !authenticated {
			if req.Cmd != "auth" {
				ws.writeJSON(wsResponse{Status: "error", Error: "authentication required, send auth first"})
				continue
			}
		}

		resp := s.dispatchWS(ws, &req, &authenticated, &confirmMode, &nextSeqNo, &prefetch, &unackedCount, &activeSubs, &subsMu)
		if resp != nil {
			ws.writeJSON(resp)
		}
	}
}

func (s *HTTPServer) dispatchWS(ws *wsConn, req *wsRequest, authenticated *bool, confirmMode *bool, nextSeqNo *uint64, prefetch *int, unackedCount *int, activeSubs *[]activeSub, subsMu *sync.Mutex) *wsResponse {
	switch req.Cmd {
	case "auth":
		return s.wsAuth(req, authenticated)
	case "ping":
		return &wsResponse{Status: "ok", Data: map[string]string{"status": "pong"}}
	case "prefetch":
		*prefetch = req.Count
		return &wsResponse{Status: "ok"}
	case "confirm":
		*confirmMode = true
		return &wsResponse{Status: "ok"}
	case "publish":
		return s.wsPublish(req, confirmMode, nextSeqNo)
	case "publish_topic":
		return s.wsPublishTopic(req, confirmMode, nextSeqNo)
	case "consume":
		return s.wsConsume(req, prefetch, unackedCount)
	case "subscribe":
		return s.wsSubscribe(ws, req, activeSubs, subsMu)
	case "ack":
		return s.wsAck(req, unackedCount)
	case "nack":
		return s.wsNack(req, unackedCount)
	case "stats":
		return &wsResponse{Status: "ok", Data: s.broker.Stats()}
	case "exchange_declare":
		return s.wsExchangeDeclare(req)
	case "exchange_delete":
		return s.wsExchangeDelete(req)
	case "bind_queue":
		return s.wsBindQueue(req)
	case "unbind_queue":
		return s.wsUnbindQueue(req)
	case "publish_exchange":
		return s.wsPublishExchange(req, confirmMode, nextSeqNo)
	default:
		return &wsResponse{Status: "error", Error: fmt.Sprintf("unknown command: %s", req.Cmd)}
	}
}

// --- Command handlers ---

func (s *HTTPServer) wsAuth(req *wsRequest, authenticated *bool) *wsResponse {
	if s.apiKey == "" {
		*authenticated = true
		return &wsResponse{Status: "ok"}
	}
	if req.APIKey != s.apiKey {
		return &wsResponse{Status: "error", Error: "unauthorized"}
	}
	*authenticated = true
	return &wsResponse{Status: "ok"}
}

func (s *HTTPServer) wsPublish(req *wsRequest, confirmMode *bool, nextSeqNo *uint64) *wsResponse {
	if req.Topic == "" {
		return &wsResponse{Status: "error", Error: "topic is required"}
	}

	msg := protocol.NewMessage(req.Topic, req.Payload, req.Headers)
	msg.Delay = req.Delay
	msg.TTL = req.TTL
	msg.Priority = req.Priority

	var publishErr error
	if *confirmMode {
		publishErr = s.broker.PublishConfirm(req.Topic, msg)
	} else {
		publishErr = s.broker.Publish(req.Topic, msg)
	}
	if publishErr != nil {
		return s.wsClusterError(publishErr)
	}

	s.metrics.IncPublished()
	result := map[string]interface{}{"id": msg.ID, "topic": msg.Topic}
	if *confirmMode {
		*nextSeqNo++
		result["seq_no"] = *nextSeqNo
		result["ack"] = true
	}
	return &wsResponse{Status: "ok", Data: result}
}

func (s *HTTPServer) wsPublishTopic(req *wsRequest, confirmMode *bool, nextSeqNo *uint64) *wsResponse {
	if req.Topic == "" {
		return &wsResponse{Status: "error", Error: "topic is required"}
	}

	msg := protocol.NewMessage(req.Topic, req.Payload, req.Headers)
	msg.Delay = req.Delay
	msg.TTL = req.TTL
	msg.Priority = req.Priority

	if err := s.broker.PublishTopic(req.Topic, msg); err != nil {
		return s.wsClusterError(err)
	}

	s.metrics.IncPublished()
	result := map[string]interface{}{"id": msg.ID, "topic": msg.Topic}
	if *confirmMode {
		*nextSeqNo++
		result["seq_no"] = *nextSeqNo
		result["ack"] = true
	}
	return &wsResponse{Status: "ok", Data: result}
}

func (s *HTTPServer) wsConsume(req *wsRequest, prefetch *int, unackedCount *int) *wsResponse {
	if req.Topic == "" {
		return &wsResponse{Status: "error", Error: "topic is required"}
	}

	if *prefetch > 0 && *unackedCount >= *prefetch {
		return &wsResponse{Status: "error", Error: "prefetch limit reached"}
	}

	msg := s.broker.TryConsume(req.Topic)
	if msg == nil {
		return &wsResponse{Status: "empty"}
	}

	*unackedCount++
	s.metrics.IncConsumed()
	return &wsResponse{Status: "ok", Data: map[string]interface{}{
		"id":        msg.ID,
		"topic":     msg.Topic,
		"payload":   json.RawMessage(msg.Payload),
		"headers":   msg.Headers,
		"timestamp": msg.Timestamp,
		"retry":     msg.Retry,
		"priority":  msg.Priority,
	}}
}

func (s *HTTPServer) wsSubscribe(ws *wsConn, req *wsRequest, activeSubs *[]activeSub, subsMu *sync.Mutex) *wsResponse {
	if req.Topic == "" {
		return &wsResponse{Status: "error", Error: "topic is required"}
	}
	subID := req.ID
	if subID == "" {
		subID = fmt.Sprintf("ws-%d", time.Now().UnixNano())
	}

	ch := s.broker.Subscribe(req.Topic, subID, 256, req.Durable)
	if ch == nil {
		return &wsResponse{Status: "error", Error: "failed to subscribe"}
	}

	subsMu.Lock()
	*activeSubs = append(*activeSubs, activeSub{req.Topic, subID})
	subsMu.Unlock()

	// Stream messages in background.
	go func() {
		for msg := range ch {
			err := ws.writeJSON(map[string]interface{}{
				"event":         "message",
				"id":            msg.ID,
				"topic":         msg.Topic,
				"payload":       json.RawMessage(msg.Payload),
				"headers":       msg.Headers,
				"timestamp":     msg.Timestamp,
				"subscriber_id": subID,
				"priority":      msg.Priority,
			})
			if err != nil {
				return // connection closed
			}
		}
	}()

	return &wsResponse{Status: "ok", Data: map[string]string{
		"subscribed": req.Topic,
		"id":         subID,
	}}
}

func (s *HTTPServer) wsAck(req *wsRequest, unackedCount *int) *wsResponse {
	if req.ID == "" {
		return &wsResponse{Status: "error", Error: "id is required"}
	}
	if err := s.broker.Ack(req.ID); err != nil {
		return s.wsClusterError(err)
	}
	if *unackedCount > 0 {
		*unackedCount--
	}
	s.metrics.IncAcked()
	return &wsResponse{Status: "ok"}
}

func (s *HTTPServer) wsNack(req *wsRequest, unackedCount *int) *wsResponse {
	if req.ID == "" {
		return &wsResponse{Status: "error", Error: "id is required"}
	}
	if err := s.broker.Nack(req.ID); err != nil {
		return s.wsClusterError(err)
	}
	if *unackedCount > 0 {
		*unackedCount--
	}
	s.metrics.IncNacked()
	return &wsResponse{Status: "ok"}
}

func (s *HTTPServer) wsExchangeDeclare(req *wsRequest) *wsResponse {
	if req.Exchange == "" {
		return &wsResponse{Status: "error", Error: "exchange is required"}
	}
	typ := req.Type
	if typ == "" {
		typ = "direct"
	}
	if err := s.broker.ExchangeDeclare(req.Exchange, broker.ExchangeType(typ), req.Durable); err != nil {
		return s.wsClusterError(err)
	}
	return &wsResponse{Status: "ok"}
}

func (s *HTTPServer) wsExchangeDelete(req *wsRequest) *wsResponse {
	if req.Exchange == "" {
		return &wsResponse{Status: "error", Error: "exchange is required"}
	}
	if err := s.broker.ExchangeDelete(req.Exchange); err != nil {
		return s.wsClusterError(err)
	}
	return &wsResponse{Status: "ok"}
}

func (s *HTTPServer) wsBindQueue(req *wsRequest) *wsResponse {
	if req.Exchange == "" || req.Queue == "" {
		return &wsResponse{Status: "error", Error: "exchange and queue are required"}
	}
	if err := s.broker.BindQueue(req.Exchange, req.Queue, req.BindingKey, req.Headers, req.MatchAll); err != nil {
		return s.wsClusterError(err)
	}
	return &wsResponse{Status: "ok"}
}

func (s *HTTPServer) wsUnbindQueue(req *wsRequest) *wsResponse {
	if req.Exchange == "" || req.Queue == "" {
		return &wsResponse{Status: "error", Error: "exchange and queue are required"}
	}
	if err := s.broker.UnbindQueue(req.Exchange, req.Queue, req.BindingKey); err != nil {
		return s.wsClusterError(err)
	}
	return &wsResponse{Status: "ok"}
}

func (s *HTTPServer) wsPublishExchange(req *wsRequest, confirmMode *bool, nextSeqNo *uint64) *wsResponse {
	msg := protocol.NewMessage("", req.Payload, req.Headers)
	msg.Priority = req.Priority

	if err := s.broker.PublishExchange(req.Exchange, req.RoutingKey, msg); err != nil {
		return s.wsClusterError(err)
	}

	s.metrics.IncPublished()
	result := map[string]interface{}{"id": msg.ID}
	if *confirmMode {
		*nextSeqNo++
		result["seq_no"] = *nextSeqNo
		result["ack"] = true
	}
	return &wsResponse{Status: "ok", Data: result}
}

func (s *HTTPServer) wsClusterError(err error) *wsResponse {
	if nle, ok := cluster.IsNotLeaderError(err); ok {
		return &wsResponse{Status: "error", Error: "not leader", Data: map[string]string{
			"leader": nle.Leader, "leader_id": nle.LeaderID,
		}}
	}
	return &wsResponse{Status: "error", Error: err.Error()}
}
