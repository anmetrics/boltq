package api

import (
	"encoding/json"
	"net"
	"testing"

	"github.com/boltq/boltq/internal/broker"
	"github.com/boltq/boltq/internal/config"
	"github.com/boltq/boltq/internal/metrics"
	"github.com/boltq/boltq/pkg/protocol"
)

func TestConsumerPrefetch(t *testing.T) {
	b := broker.New(broker.Config{QueueCap: 100})
	m := metrics.Global()
	server := NewTCPServer(b, m, config.ServerConfig{}, "")
	
	addr := "127.0.0.1:9092"
	if err := server.Start(addr); err != nil {
		t.Fatal(err)
	}
	defer server.Shutdown()

	conn, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()

	// 1. Set prefetch = 1
	prefetchReq, _ := json.Marshal(map[string]int{"count": 1})
	protocol.WriteFrame(conn, protocol.Frame{Command: protocol.CmdPrefetch, Payload: prefetchReq})
	protocol.ReadFrame(conn) // OK

	// 2. Publish 2 messages
	b.Publish("test", protocol.NewMessage("test", []byte(`"1"`), nil))
	b.Publish("test", protocol.NewMessage("test", []byte(`"2"`), nil))

	// 3. Consume 1st message
	consumeReq, _ := json.Marshal(map[string]string{"topic": "test"})
	protocol.WriteFrame(conn, protocol.Frame{Command: protocol.CmdConsume, Payload: consumeReq})
	frame1, _ := protocol.ReadFrame(conn)
	if frame1.Command != protocol.StatusOK {
		t.Fatal("expected status OK for first consume")
	}

	// 4. Consume 2nd message (should fail because prefetch limit 1 is reached)
	protocol.WriteFrame(conn, protocol.Frame{Command: protocol.CmdConsume, Payload: consumeReq})
	frame2, _ := protocol.ReadFrame(conn)
	if frame2.Command != protocol.StatusError {
		t.Fatal("expected error for second consume due to prefetch")
	}

	// 5. ACK first message
	var msg protocol.Message
	json.Unmarshal(frame1.Payload, &msg)
	ackReq, _ := json.Marshal(map[string]string{"id": msg.ID})
	protocol.WriteFrame(conn, protocol.Frame{Command: protocol.CmdAck, Payload: ackReq})
	protocol.ReadFrame(conn) // Status OK

	// 6. Consume again (should succeed now)
	protocol.WriteFrame(conn, protocol.Frame{Command: protocol.CmdConsume, Payload: consumeReq})
	frame3, _ := protocol.ReadFrame(conn)
	if frame3.Command != protocol.StatusOK {
		t.Fatal("expected status OK for third consume after ACK")
	}
}
