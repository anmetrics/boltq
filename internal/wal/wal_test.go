package wal

import (
	"os"
	"testing"

	"github.com/boltq/boltq/pkg/protocol"
)

func TestWALWriteAndReadAll(t *testing.T) {
	dir, err := os.MkdirTemp("", "boltq-wal-test")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)

	w, err := New(dir)
	if err != nil {
		t.Fatal(err)
	}

	msgs := []*protocol.Message{
		protocol.NewMessage("queue1", []byte(`{"key":"value1"}`), map[string]string{"h1": "v1"}),
		protocol.NewMessage("queue2", []byte(`{"key":"value2"}`), nil),
		protocol.NewMessage("queue1", []byte(`{"key":"value3"}`), map[string]string{"h2": "v2"}),
	}

	for _, msg := range msgs {
		if _, err := w.Write(msg); err != nil {
			t.Fatalf("write: %v", err)
		}
	}

	// Read back.
	recovered, err := w.ReadAll()
	if err != nil {
		t.Fatalf("read: %v", err)
	}

	if len(recovered) != len(msgs) {
		t.Fatalf("expected %d messages, got %d", len(msgs), len(recovered))
	}

	for i, msg := range recovered {
		if msg.ID != msgs[i].ID {
			t.Errorf("msg %d: expected id %s, got %s", i, msgs[i].ID, msg.ID)
		}
		if msg.Topic != msgs[i].Topic {
			t.Errorf("msg %d: expected topic %s, got %s", i, msgs[i].Topic, msg.Topic)
		}
	}

	w.Close()
}

func TestWALRecovery(t *testing.T) {
	dir, err := os.MkdirTemp("", "boltq-wal-recovery")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)

	// Write some messages.
	w, err := New(dir)
	if err != nil {
		t.Fatal(err)
	}

	for i := 0; i < 100; i++ {
		w.Write(protocol.NewMessage("test", []byte(`"hello"`), nil))
	}
	w.Close()

	// Reopen and recover.
	w2, err := New(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer w2.Close()

	recovered, err := w2.ReadAll()
	if err != nil {
		t.Fatalf("recovery: %v", err)
	}
	if len(recovered) != 100 {
		t.Fatalf("expected 100 messages, got %d", len(recovered))
	}
}

func TestWALTruncate(t *testing.T) {
	dir, err := os.MkdirTemp("", "boltq-wal-truncate")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)

	w, err := New(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()

	w.Write(protocol.NewMessage("test", []byte(`"data"`), nil))
	if err := w.Truncate(); err != nil {
		t.Fatal(err)
	}

	recovered, _ := w.ReadAll()
	if len(recovered) != 0 {
		t.Fatalf("expected 0 messages after truncate, got %d", len(recovered))
	}
}

// --- Benchmarks ---

func BenchmarkWALWrite(b *testing.B) {
	dir, _ := os.MkdirTemp("", "boltq-wal-bench")
	defer os.RemoveAll(dir)

	w, _ := New(dir)
	defer w.Close()

	msg := protocol.NewMessage("bench", []byte(`{"benchmark":"data","size":1024}`), nil)

	b.ResetTimer()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		w.Write(msg)
	}
}

func BenchmarkWALReadAll(b *testing.B) {
	dir, _ := os.MkdirTemp("", "boltq-wal-bench-read")
	defer os.RemoveAll(dir)

	w, _ := New(dir)
	msg := protocol.NewMessage("bench", []byte(`{"benchmark":"data"}`), nil)
	for i := 0; i < 10000; i++ {
		w.Write(msg)
	}

	b.ResetTimer()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		w.ReadAllRecords()
	}
}
