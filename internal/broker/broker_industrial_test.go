package broker

import (
	"fmt"
	"testing"
	"time"

	"github.com/boltq/boltq/pkg/protocol"
)

func TestSpillToDisk(t *testing.T) {
	// 1. Create broker with small power-of-two capacity
	b := New(Config{QueueCap: 4})
	defer b.Close()

	topic := "overflow_test"

	// 2. Publish 10 messages (4 in RAM, 6 to disk)
	for i := 1; i <= 10; i++ {
		msg := protocol.NewMessage(topic, []byte(fmt.Sprintf(`"data%d"`, i)), nil)
		if err := b.Publish(topic, msg); err != nil {
			t.Fatalf("failed to publish message %d: %v", i, err)
		}
		t.Logf("Published %d: %s", i, msg.ID)
	}

	// 3. Consume first 4 (from RAM)
	for i := 1; i <= 4; i++ {
		msg := b.TryConsume(topic)
		if msg == nil {
			t.Fatalf("expected message %d from RAM", i)
		}
		t.Logf("Consumed %d: %s (payload: %s)", i, msg.ID, string(msg.Payload))
	}

	// 4. Queue should be empty now
	if msg := b.TryConsume(topic); msg != nil {
		t.Fatalf("expected queue to be empty, but got message %s: %s", msg.ID, string(msg.Payload))
	}

	// 5. Consume next 6 (triggering reloads)
	for i := 5; i <= 10; i++ {
		b.ProcessAdvancedFeatures() // Trigger reload if there is space
		msg := b.TryConsume(topic)
		if msg == nil {
			t.Fatalf("expected reloaded message %d from disk", i)
		}
		t.Logf("Consumed %d: %s (payload: %s)", i, msg.ID, string(msg.Payload))
	}
}

func TestDurablePubSub(t *testing.T) {
	b := newTestBroker()
	defer b.Close()

	topic := "news"
	subID := "durable_sub"

	// 1. First subscription to "register" as durable
	_ = b.Subscribe(topic, subID, 10, true)
	b.Unsubscribe(topic, subID) // Go offline

	// 2. Publish messages while offline
	msg1 := protocol.NewMessage(topic, []byte(`"msg1"`), nil)
	msg2 := protocol.NewMessage(topic, []byte(`"msg2"`), nil)
	b.PublishTopic(topic, msg1)
	b.PublishTopic(topic, msg2)

	// 3. Reconnect (Subscribe with durable=true)
	ch2 := b.Subscribe(topic, subID, 10, true)

	// 4. Should receive missed messages
	select {
	case m := <-ch2:
		if string(m.Payload) != `"msg1"` {
			t.Fatalf("expected msg1, got %s", string(m.Payload))
		}
	case <-time.After(time.Second):
		t.Fatal("timeout waiting for msg1")
	}

	select {
	case m := <-ch2:
		if string(m.Payload) != `"msg2"` {
			t.Fatalf("expected msg2, got %s", string(m.Payload))
		}
	case <-time.After(time.Second):
		t.Fatal("timeout waiting for msg2")
	}
}
