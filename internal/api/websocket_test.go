package api

import (
	"crypto/sha1"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/boltq/boltq/internal/broker"
	"github.com/boltq/boltq/internal/metrics"
	"github.com/boltq/boltq/pkg/protocol"
)

// --- helpers ---

func newTestHTTPServer(t *testing.T) *HTTPServer {
	t.Helper()
	b := broker.New(broker.Config{
		MaxRetry:   3,
		AckTimeout: 30 * time.Second,
		QueueCap:   1 << 16,
	})
	m := metrics.Global()
	s := &HTTPServer{
		broker:  b,
		metrics: m,
		mux:     http.NewServeMux(),
	}
	s.mux.HandleFunc("/ws", s.handleWebSocket)
	return s
}

// wsTestClient dials the test server and performs the WebSocket handshake.
type wsTestClient struct {
	conn net.Conn
	t    *testing.T
}

func dialWS(t *testing.T, url string) *wsTestClient {
	t.Helper()
	// url is like "http://127.0.0.1:PORT"
	addr := strings.TrimPrefix(url, "http://")
	conn, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}

	key := base64.StdEncoding.EncodeToString([]byte("test-key-1234567"))
	req := "GET /ws HTTP/1.1\r\n" +
		"Host: " + addr + "\r\n" +
		"Upgrade: websocket\r\n" +
		"Connection: Upgrade\r\n" +
		"Sec-WebSocket-Key: " + key + "\r\n" +
		"Sec-WebSocket-Version: 13\r\n\r\n"
	if _, err := conn.Write([]byte(req)); err != nil {
		t.Fatalf("write handshake: %v", err)
	}

	// Read response until \r\n\r\n.
	buf := make([]byte, 4096)
	n, err := conn.Read(buf)
	if err != nil {
		t.Fatalf("read handshake: %v", err)
	}
	resp := string(buf[:n])
	if !strings.Contains(resp, "101 Switching Protocols") {
		t.Fatalf("unexpected handshake response: %s", resp)
	}

	// Verify accept key.
	h := sha1.New()
	h.Write([]byte(key + wsMagicGUID))
	expectedAccept := base64.StdEncoding.EncodeToString(h.Sum(nil))
	if !strings.Contains(resp, expectedAccept) {
		t.Fatalf("accept key mismatch")
	}

	return &wsTestClient{conn: conn, t: t}
}

func (c *wsTestClient) send(v interface{}) {
	c.t.Helper()
	data, _ := json.Marshal(v)
	c.writeFrame(wsOpText, data)
}

func (c *wsTestClient) writeFrame(opcode int, payload []byte) {
	c.t.Helper()
	length := len(payload)
	// Client frames MUST be masked (RFC 6455).
	mask := [4]byte{0x12, 0x34, 0x56, 0x78}

	var hdr []byte
	switch {
	case length <= 125:
		hdr = []byte{byte(0x80 | opcode), byte(0x80 | length)}
	case length <= 65535:
		hdr = make([]byte, 4)
		hdr[0] = byte(0x80 | opcode)
		hdr[1] = byte(0x80 | 126)
		binary.BigEndian.PutUint16(hdr[2:4], uint16(length))
	default:
		hdr = make([]byte, 10)
		hdr[0] = byte(0x80 | opcode)
		hdr[1] = byte(0x80 | 127)
		binary.BigEndian.PutUint64(hdr[2:10], uint64(length))
	}

	hdr = append(hdr, mask[:]...)

	masked := make([]byte, length)
	for i := range payload {
		masked[i] = payload[i] ^ mask[i%4]
	}

	if _, err := c.conn.Write(append(hdr, masked...)); err != nil {
		c.t.Fatalf("write frame: %v", err)
	}
}

func (c *wsTestClient) recv() wsResponse {
	c.t.Helper()
	c.conn.SetReadDeadline(time.Now().Add(3 * time.Second))

	hdr := make([]byte, 2)
	if _, err := io.ReadFull(c.conn, hdr); err != nil {
		c.t.Fatalf("read frame header: %v", err)
	}

	length := uint64(hdr[1] & 0x7F)
	switch {
	case length == 126:
		ext := make([]byte, 2)
		io.ReadFull(c.conn, ext)
		length = uint64(binary.BigEndian.Uint16(ext))
	case length == 127:
		ext := make([]byte, 8)
		io.ReadFull(c.conn, ext)
		length = binary.BigEndian.Uint64(ext)
	}

	payload := make([]byte, length)
	if length > 0 {
		if _, err := io.ReadFull(c.conn, payload); err != nil {
			c.t.Fatalf("read frame payload: %v", err)
		}
	}

	var resp wsResponse
	if err := json.Unmarshal(payload, &resp); err != nil {
		// Try as raw map (for subscription events).
		c.t.Fatalf("unmarshal response: %v (payload: %s)", err, string(payload))
	}
	return resp
}

func (c *wsTestClient) recvRaw() map[string]interface{} {
	c.t.Helper()
	c.conn.SetReadDeadline(time.Now().Add(3 * time.Second))

	hdr := make([]byte, 2)
	if _, err := io.ReadFull(c.conn, hdr); err != nil {
		c.t.Fatalf("read frame header: %v", err)
	}

	length := uint64(hdr[1] & 0x7F)
	switch {
	case length == 126:
		ext := make([]byte, 2)
		io.ReadFull(c.conn, ext)
		length = uint64(binary.BigEndian.Uint16(ext))
	case length == 127:
		ext := make([]byte, 8)
		io.ReadFull(c.conn, ext)
		length = binary.BigEndian.Uint64(ext)
	}

	payload := make([]byte, length)
	if length > 0 {
		if _, err := io.ReadFull(c.conn, payload); err != nil {
			c.t.Fatalf("read frame payload: %v", err)
		}
	}

	var m map[string]interface{}
	if err := json.Unmarshal(payload, &m); err != nil {
		c.t.Fatalf("unmarshal raw: %v", err)
	}
	return m
}

func (c *wsTestClient) close() {
	c.conn.Close()
}

// --- Tests ---

func TestWSPingPong(t *testing.T) {
	srv := newTestHTTPServer(t)
	ts := httptest.NewServer(srv.mux)
	defer ts.Close()

	ws := dialWS(t, ts.URL)
	defer ws.close()

	ws.send(map[string]string{"cmd": "ping"})
	resp := ws.recv()
	if resp.Status != "ok" {
		t.Fatalf("expected ok, got %s", resp.Status)
	}
}

func TestWSPublishConsume(t *testing.T) {
	srv := newTestHTTPServer(t)
	ts := httptest.NewServer(srv.mux)
	defer ts.Close()

	ws := dialWS(t, ts.URL)
	defer ws.close()

	// Publish.
	ws.send(map[string]interface{}{
		"cmd":     "publish",
		"topic":   "test-q",
		"payload": map[string]string{"msg": "hello"},
	})
	resp := ws.recv()
	if resp.Status != "ok" {
		t.Fatalf("publish: expected ok, got %s (err: %s)", resp.Status, resp.Error)
	}

	// Consume.
	ws.send(map[string]string{"cmd": "consume", "topic": "test-q"})
	resp = ws.recv()
	if resp.Status != "ok" {
		t.Fatalf("consume: expected ok, got %s", resp.Status)
	}
	data := resp.Data.(map[string]interface{})
	if data["topic"] != "test-q" {
		t.Fatalf("expected topic test-q, got %v", data["topic"])
	}
}

func TestWSConsumeEmpty(t *testing.T) {
	srv := newTestHTTPServer(t)
	ts := httptest.NewServer(srv.mux)
	defer ts.Close()

	ws := dialWS(t, ts.URL)
	defer ws.close()

	ws.send(map[string]string{"cmd": "consume", "topic": "empty-q"})
	resp := ws.recv()
	if resp.Status != "empty" {
		t.Fatalf("expected empty, got %s", resp.Status)
	}
}

func TestWSAckNack(t *testing.T) {
	srv := newTestHTTPServer(t)
	ts := httptest.NewServer(srv.mux)
	defer ts.Close()

	ws := dialWS(t, ts.URL)
	defer ws.close()

	// Publish and consume.
	ws.send(map[string]interface{}{"cmd": "publish", "topic": "ack-q", "payload": "data"})
	ws.recv()

	ws.send(map[string]string{"cmd": "consume", "topic": "ack-q"})
	resp := ws.recv()
	data := resp.Data.(map[string]interface{})
	msgID := data["id"].(string)

	// Ack.
	ws.send(map[string]string{"cmd": "ack", "id": msgID})
	resp = ws.recv()
	if resp.Status != "ok" {
		t.Fatalf("ack: expected ok, got %s", resp.Status)
	}

	// Publish another, consume, nack.
	ws.send(map[string]interface{}{"cmd": "publish", "topic": "ack-q", "payload": "data2"})
	ws.recv()
	ws.send(map[string]string{"cmd": "consume", "topic": "ack-q"})
	resp = ws.recv()
	data = resp.Data.(map[string]interface{})
	msgID = data["id"].(string)

	ws.send(map[string]string{"cmd": "nack", "id": msgID})
	resp = ws.recv()
	if resp.Status != "ok" {
		t.Fatalf("nack: expected ok, got %s", resp.Status)
	}
}

func TestWSPriority(t *testing.T) {
	srv := newTestHTTPServer(t)
	ts := httptest.NewServer(srv.mux)
	defer ts.Close()

	ws := dialWS(t, ts.URL)
	defer ws.close()

	// Publish with different priorities.
	ws.send(map[string]interface{}{"cmd": "publish", "topic": "prio-q", "payload": "low", "priority": 1})
	ws.recv()
	ws.send(map[string]interface{}{"cmd": "publish", "topic": "prio-q", "payload": "high", "priority": 9})
	ws.recv()

	// Consume — should get high priority first.
	ws.send(map[string]string{"cmd": "consume", "topic": "prio-q"})
	resp := ws.recv()
	data := resp.Data.(map[string]interface{})
	if data["priority"].(float64) != 9 {
		t.Fatalf("expected priority 9, got %v", data["priority"])
	}
}

func TestWSConfirmMode(t *testing.T) {
	srv := newTestHTTPServer(t)
	ts := httptest.NewServer(srv.mux)
	defer ts.Close()

	ws := dialWS(t, ts.URL)
	defer ws.close()

	// Enable confirm mode.
	ws.send(map[string]string{"cmd": "confirm"})
	resp := ws.recv()
	if resp.Status != "ok" {
		t.Fatalf("confirm: expected ok, got %s", resp.Status)
	}

	// Publish should return seq_no and ack.
	ws.send(map[string]interface{}{"cmd": "publish", "topic": "confirm-q", "payload": "test"})
	resp = ws.recv()
	data := resp.Data.(map[string]interface{})
	if data["seq_no"].(float64) != 1 {
		t.Fatalf("expected seq_no 1, got %v", data["seq_no"])
	}
	if data["ack"] != true {
		t.Fatalf("expected ack true, got %v", data["ack"])
	}

	// Second publish should get seq_no 2.
	ws.send(map[string]interface{}{"cmd": "publish", "topic": "confirm-q", "payload": "test2"})
	resp = ws.recv()
	data = resp.Data.(map[string]interface{})
	if data["seq_no"].(float64) != 2 {
		t.Fatalf("expected seq_no 2, got %v", data["seq_no"])
	}
}

func TestWSPrefetch(t *testing.T) {
	srv := newTestHTTPServer(t)
	ts := httptest.NewServer(srv.mux)
	defer ts.Close()

	ws := dialWS(t, ts.URL)
	defer ws.close()

	// Publish 3 messages.
	for i := 0; i < 3; i++ {
		ws.send(map[string]interface{}{"cmd": "publish", "topic": "pre-q", "payload": i})
		ws.recv()
	}

	// Set prefetch to 1.
	ws.send(map[string]interface{}{"cmd": "prefetch", "count": 1})
	ws.recv()

	// Consume first — should work.
	ws.send(map[string]string{"cmd": "consume", "topic": "pre-q"})
	resp := ws.recv()
	if resp.Status != "ok" {
		t.Fatalf("first consume should work, got %s", resp.Status)
	}
	data := resp.Data.(map[string]interface{})
	msgID := data["id"].(string)

	// Consume second — should fail (prefetch limit).
	ws.send(map[string]string{"cmd": "consume", "topic": "pre-q"})
	resp = ws.recv()
	if resp.Status != "error" || resp.Error != "prefetch limit reached" {
		t.Fatalf("expected prefetch error, got %s: %s", resp.Status, resp.Error)
	}

	// Ack first, then consume should work again.
	ws.send(map[string]string{"cmd": "ack", "id": msgID})
	ws.recv()

	ws.send(map[string]string{"cmd": "consume", "topic": "pre-q"})
	resp = ws.recv()
	if resp.Status != "ok" {
		t.Fatalf("consume after ack should work, got %s", resp.Status)
	}
}

func TestWSSubscribe(t *testing.T) {
	srv := newTestHTTPServer(t)
	ts := httptest.NewServer(srv.mux)
	defer ts.Close()

	ws := dialWS(t, ts.URL)
	defer ws.close()

	// Subscribe.
	ws.send(map[string]interface{}{
		"cmd":   "subscribe",
		"topic": "events",
		"id":    "sub-1",
	})
	resp := ws.recv()
	if resp.Status != "ok" {
		t.Fatalf("subscribe: expected ok, got %s", resp.Status)
	}

	// Publish to topic from another connection.
	ws2 := dialWS(t, ts.URL)
	defer ws2.close()

	ws2.send(map[string]interface{}{
		"cmd":     "publish_topic",
		"topic":   "events",
		"payload": map[string]string{"event": "user_created"},
	})
	ws2.recv()

	// Subscriber should receive the message.
	msg := ws.recvRaw()
	if msg["event"] != "message" {
		t.Fatalf("expected event=message, got %v", msg["event"])
	}
	if msg["topic"] != "events" {
		t.Fatalf("expected topic=events, got %v", msg["topic"])
	}
	if msg["subscriber_id"] != "sub-1" {
		t.Fatalf("expected subscriber_id=sub-1, got %v", msg["subscriber_id"])
	}
}

func TestWSExchangeRouting(t *testing.T) {
	srv := newTestHTTPServer(t)
	ts := httptest.NewServer(srv.mux)
	defer ts.Close()

	ws := dialWS(t, ts.URL)
	defer ws.close()

	// Declare exchange.
	ws.send(map[string]interface{}{"cmd": "exchange_declare", "exchange": "logs", "type": "direct"})
	resp := ws.recv()
	if resp.Status != "ok" {
		t.Fatalf("exchange_declare: %s %s", resp.Status, resp.Error)
	}

	// Bind queues.
	ws.send(map[string]interface{}{"cmd": "bind_queue", "exchange": "logs", "queue": "error-q", "binding_key": "error"})
	resp = ws.recv()
	if resp.Status != "ok" {
		t.Fatalf("bind_queue: %s %s", resp.Status, resp.Error)
	}

	ws.send(map[string]interface{}{"cmd": "bind_queue", "exchange": "logs", "queue": "info-q", "binding_key": "info"})
	ws.recv()

	// Publish to exchange with routing key "error".
	ws.send(map[string]interface{}{
		"cmd":         "publish_exchange",
		"exchange":    "logs",
		"routing_key": "error",
		"payload":     "disk full",
	})
	resp = ws.recv()
	if resp.Status != "ok" {
		t.Fatalf("publish_exchange: %s %s", resp.Status, resp.Error)
	}

	// Consume from error-q — should have message.
	ws.send(map[string]string{"cmd": "consume", "topic": "error-q"})
	resp = ws.recv()
	if resp.Status != "ok" {
		t.Fatalf("consume error-q: expected message, got %s", resp.Status)
	}

	// Consume from info-q — should be empty.
	ws.send(map[string]string{"cmd": "consume", "topic": "info-q"})
	resp = ws.recv()
	if resp.Status != "empty" {
		t.Fatalf("consume info-q: expected empty, got %s", resp.Status)
	}

	// Unbind.
	ws.send(map[string]interface{}{"cmd": "unbind_queue", "exchange": "logs", "queue": "error-q", "binding_key": "error"})
	resp = ws.recv()
	if resp.Status != "ok" {
		t.Fatalf("unbind: %s", resp.Error)
	}

	// Delete exchange.
	ws.send(map[string]interface{}{"cmd": "exchange_delete", "exchange": "logs"})
	resp = ws.recv()
	if resp.Status != "ok" {
		t.Fatalf("exchange_delete: %s", resp.Error)
	}
}

func TestWSStats(t *testing.T) {
	srv := newTestHTTPServer(t)
	ts := httptest.NewServer(srv.mux)
	defer ts.Close()

	ws := dialWS(t, ts.URL)
	defer ws.close()

	ws.send(map[string]string{"cmd": "stats"})
	resp := ws.recv()
	if resp.Status != "ok" {
		t.Fatalf("stats: expected ok, got %s", resp.Status)
	}
}

func TestWSUnknownCommand(t *testing.T) {
	srv := newTestHTTPServer(t)
	ts := httptest.NewServer(srv.mux)
	defer ts.Close()

	ws := dialWS(t, ts.URL)
	defer ws.close()

	ws.send(map[string]string{"cmd": "foobar"})
	resp := ws.recv()
	if resp.Status != "error" {
		t.Fatalf("expected error for unknown cmd, got %s", resp.Status)
	}
	if !strings.Contains(resp.Error, "unknown command") {
		t.Fatalf("expected 'unknown command' error, got %s", resp.Error)
	}
}

func TestWSMissingTopic(t *testing.T) {
	srv := newTestHTTPServer(t)
	ts := httptest.NewServer(srv.mux)
	defer ts.Close()

	ws := dialWS(t, ts.URL)
	defer ws.close()

	ws.send(map[string]string{"cmd": "publish"})
	resp := ws.recv()
	if resp.Status != "error" || resp.Error != "topic is required" {
		t.Fatalf("expected 'topic is required', got %s: %s", resp.Status, resp.Error)
	}
}

func TestWSMissingID(t *testing.T) {
	srv := newTestHTTPServer(t)
	ts := httptest.NewServer(srv.mux)
	defer ts.Close()

	ws := dialWS(t, ts.URL)
	defer ws.close()

	ws.send(map[string]string{"cmd": "ack"})
	resp := ws.recv()
	if resp.Status != "error" || resp.Error != "id is required" {
		t.Fatalf("expected 'id is required', got %s: %s", resp.Status, resp.Error)
	}

	ws.send(map[string]string{"cmd": "nack"})
	resp = ws.recv()
	if resp.Status != "error" || resp.Error != "id is required" {
		t.Fatalf("expected 'id is required', got %s: %s", resp.Status, resp.Error)
	}
}

func TestWSAuthRequired(t *testing.T) {
	b := broker.New(broker.Config{
		MaxRetry:   3,
		AckTimeout: 30 * time.Second,
		QueueCap:   1 << 16,
	})
	m := metrics.Global()
	s := &HTTPServer{
		broker:  b,
		metrics: m,
		apiKey:  "secret-key",
		mux:     http.NewServeMux(),
	}
	s.mux.HandleFunc("/ws", s.handleWebSocket)
	ts := httptest.NewServer(s.mux)
	defer ts.Close()

	ws := dialWS(t, ts.URL)
	defer ws.close()

	// Command without auth should fail.
	ws.send(map[string]string{"cmd": "ping"})
	resp := ws.recv()
	if resp.Status != "error" || !strings.Contains(resp.Error, "authentication required") {
		t.Fatalf("expected auth error, got %s: %s", resp.Status, resp.Error)
	}

	// Wrong key.
	ws.send(map[string]string{"cmd": "auth", "api_key": "wrong"})
	resp = ws.recv()
	if resp.Status != "error" || resp.Error != "unauthorized" {
		t.Fatalf("expected unauthorized, got %s: %s", resp.Status, resp.Error)
	}

	// Correct key.
	ws.send(map[string]string{"cmd": "auth", "api_key": "secret-key"})
	resp = ws.recv()
	if resp.Status != "ok" {
		t.Fatalf("auth should succeed, got %s", resp.Status)
	}

	// Now ping should work.
	ws.send(map[string]string{"cmd": "ping"})
	resp = ws.recv()
	if resp.Status != "ok" {
		t.Fatalf("ping after auth should work, got %s", resp.Status)
	}
}

func TestWSConcurrentPublishConsume(t *testing.T) {
	srv := newTestHTTPServer(t)
	ts := httptest.NewServer(srv.mux)
	defer ts.Close()

	const n = 50
	var wg sync.WaitGroup

	// Publish from multiple connections.
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			ws := dialWS(t, ts.URL)
			defer ws.close()
			ws.send(map[string]interface{}{"cmd": "publish", "topic": "conc-q", "payload": i})
			ws.recv()
		}(i)
	}
	wg.Wait()

	// Consume all.
	ws := dialWS(t, ts.URL)
	defer ws.close()
	consumed := 0
	for i := 0; i < n; i++ {
		ws.send(map[string]string{"cmd": "consume", "topic": "conc-q"})
		resp := ws.recv()
		if resp.Status == "ok" {
			consumed++
		}
	}
	if consumed != n {
		t.Fatalf("expected %d consumed, got %d", n, consumed)
	}
}

func TestWSPublishTopicConfirm(t *testing.T) {
	srv := newTestHTTPServer(t)
	ts := httptest.NewServer(srv.mux)
	defer ts.Close()

	ws := dialWS(t, ts.URL)
	defer ws.close()

	// Enable confirm, then publish_topic.
	ws.send(map[string]string{"cmd": "confirm"})
	ws.recv()

	ws.send(map[string]interface{}{"cmd": "publish_topic", "topic": "events", "payload": "data"})
	resp := ws.recv()
	data := resp.Data.(map[string]interface{})
	if data["ack"] != true {
		t.Fatalf("expected ack=true in confirm mode")
	}
	if data["seq_no"].(float64) != 1 {
		t.Fatalf("expected seq_no=1, got %v", data["seq_no"])
	}
}

func TestWSInvalidJSON(t *testing.T) {
	srv := newTestHTTPServer(t)
	ts := httptest.NewServer(srv.mux)
	defer ts.Close()

	ws := dialWS(t, ts.URL)
	defer ws.close()

	// Send invalid JSON.
	ws.writeFrame(wsOpText, []byte(`{not json}`))
	resp := ws.recv()
	if resp.Status != "error" || !strings.Contains(resp.Error, "invalid json") {
		t.Fatalf("expected invalid json error, got %s: %s", resp.Status, resp.Error)
	}
}

func TestWSPublishConsumeWithHeaders(t *testing.T) {
	srv := newTestHTTPServer(t)
	ts := httptest.NewServer(srv.mux)
	defer ts.Close()

	ws := dialWS(t, ts.URL)
	defer ws.close()

	ws.send(map[string]interface{}{
		"cmd":     "publish",
		"topic":   "hdr-q",
		"payload": "test",
		"headers": map[string]string{"x-trace": "abc123", "source": "ws"},
	})
	ws.recv()

	ws.send(map[string]string{"cmd": "consume", "topic": "hdr-q"})
	resp := ws.recv()
	data := resp.Data.(map[string]interface{})
	headers := data["headers"].(map[string]interface{})
	if headers["x-trace"] != "abc123" {
		t.Fatalf("expected x-trace=abc123, got %v", headers["x-trace"])
	}
}

func TestWSDelayedMessage(t *testing.T) {
	srv := newTestHTTPServer(t)
	ts := httptest.NewServer(srv.mux)
	defer ts.Close()

	ws := dialWS(t, ts.URL)
	defer ws.close()

	// Publish with 500ms delay.
	ws.send(map[string]interface{}{
		"cmd":     "publish",
		"topic":   "delay-q",
		"payload": "delayed",
		"delay":   int64(500 * time.Millisecond),
	})
	ws.recv()

	// Consume immediately — should be empty (delayed).
	ws.send(map[string]string{"cmd": "consume", "topic": "delay-q"})
	resp := ws.recv()
	if resp.Status != "empty" {
		t.Fatalf("expected empty for delayed message, got %s", resp.Status)
	}

	// Wait and process.
	time.Sleep(600 * time.Millisecond)
	srv.broker.ProcessAdvancedFeatures()

	ws.send(map[string]string{"cmd": "consume", "topic": "delay-q"})
	resp = ws.recv()
	if resp.Status != "ok" {
		t.Fatalf("expected message after delay, got %s", resp.Status)
	}
}

// TestWSPublishExchangeConfirm tests publisher confirm with exchange publish.
func TestWSPublishExchangeConfirm(t *testing.T) {
	srv := newTestHTTPServer(t)
	ts := httptest.NewServer(srv.mux)
	defer ts.Close()

	ws := dialWS(t, ts.URL)
	defer ws.close()

	// Setup: declare exchange + bind queue.
	ws.send(map[string]interface{}{"cmd": "exchange_declare", "exchange": "x1", "type": "direct"})
	ws.recv()
	ws.send(map[string]interface{}{"cmd": "bind_queue", "exchange": "x1", "queue": "q1", "binding_key": "k1"})
	ws.recv()

	// Enable confirm.
	ws.send(map[string]string{"cmd": "confirm"})
	ws.recv()

	// Publish to exchange.
	ws.send(map[string]interface{}{
		"cmd":         "publish_exchange",
		"exchange":    "x1",
		"routing_key": "k1",
		"payload":     "confirmed-data",
	})
	resp := ws.recv()
	data := resp.Data.(map[string]interface{})
	if data["ack"] != true {
		t.Fatalf("expected ack=true")
	}

	// Verify message arrived.
	ws.send(map[string]string{"cmd": "consume", "topic": "q1"})
	resp = ws.recv()
	if resp.Status != "ok" {
		t.Fatalf("message should be in q1")
	}
}

// Ensure unused import is consumed.
var _ = protocol.NewMessage
